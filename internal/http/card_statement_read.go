package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Reading a statement before the bill exists.

`/accounts/:id/statements/read` takes the file the user picked in the Add
statement sheet and returns three things, saving none of them:

  - the bill's own summary, to prefill the sheet,
  - the transaction rows, for the statement check once the bill is saved,
  - what the statement says about the card that Finnri has differently.

A PDF stays free. Its rows are read locally as before; its summary is one
small text-model call on the masked text, and every amount that call returns
must be printed in that text or it is dropped. When AI is off or failing, the
PDF still yields its rows and simply prefills nothing.

Screenshots are the existing paid read, now returning the summary in the same
call — no extra charge.
*/

// statementSummary is what the statement says about the bill and the card.
// Every field is optional: nil means "not printed", never "zero".
type statementSummary struct {
	StatementDate  string        `json:"statement_date,omitempty"`
	DueDate        string        `json:"due_date,omitempty"`
	TotalDue       *models.Money `json:"total_due,omitempty"`
	MinimumDue     *models.Money `json:"minimum_due,omitempty"`
	CreditLimit    *models.Money `json:"credit_limit,omitempty"`
	AvailableLimit *models.Money `json:"available_limit,omitempty"`
	OpeningBalance *models.Money `json:"opening_balance,omitempty"`
	Payments       *models.Money `json:"payments,omitempty"`
	Purchases      *models.Money `json:"purchases,omitempty"`
	FeesAndCharges *models.Money `json:"fees_and_charges,omitempty"`
	AnnualFee      *models.Money `json:"annual_fee,omitempty"`
	FeeWaiverSpend *models.Money `json:"fee_waiver_spend,omitempty"`
	RenewalMonth   string        `json:"renewal_month,omitempty"`
	CardLast4      string        `json:"card_last4,omitempty"`
	Issuer         string        `json:"issuer,omitempty"`
}

func (summary statementSummary) isEmpty() bool {
	return summary == (statementSummary{})
}

// cardUpdate is one card setting the statement disagrees with. Current is
// what Finnri holds now (zero values mean "not set"); Proposed is what the
// statement says. The user picks which to apply.
type cardUpdate struct {
	Field    string `json:"field"`
	Label    string `json:"label"`
	Current  any    `json:"current"`
	Proposed any    `json:"proposed"`
}

type statementReadResponse struct {
	Summary     statementSummary `json:"summary"`
	Lines       []statementLine  `json:"lines"`
	CardUpdates []cardUpdate     `json:"card_updates"`
	// Warnings are facts worth stopping for, chiefly a statement that belongs
	// to a different card.
	Warnings              []string `json:"warnings"`
	Source                string   `json:"source"`
	CreditsCharged        *int     `json:"credits_charged,omitempty"`
	CreditsRemainingToday *int     `json:"credits_remaining_today,omitempty"`
	CreditsRemainingTotal *int     `json:"credits_remaining_total,omitempty"`
}

var monthNames = []string{
	"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
}

func (s *Server) readCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	account, err := loadUserCard(userID, accountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}
	if _, err := c.MultipartForm(); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_required"})
		return
	}

	var response statementReadResponse
	var handled bool
	if _, fileErr := c.FormFile("file"); fileErr == nil {
		response, handled = s.readStatementPDF(c)
	} else {
		response, handled = s.readStatementScreenshots(c)
	}
	if !handled {
		return
	}

	if response.Lines == nil {
		response.Lines = []statementLine{}
	}
	response.CardUpdates, response.Warnings = compareCardWithStatement(account, response.Summary)
	c.JSON(http.StatusOK, response)
}

// readStatementPDF reads the rows locally and asks the text model for the
// summary. Errors that are the user's to fix (password, not a PDF) are
// written here; an unavailable model is not an error.
func (s *Server) readStatementPDF(c *gin.Context) (statementReadResponse, bool) {
	file, _ := c.FormFile("file")
	opened, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_unreadable"})
		return statementReadResponse{}, false
	}
	defer opened.Close()
	data, err := readUploadedPDF(opened)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_pdf"})
		return statementReadResponse{}, false
	}
	// Read once, used once, dropped — see card_statement_pdf.go.
	password := c.PostForm("password")
	text, err := extractStatementText(data, password)
	data = nil
	password = ""
	if err != nil {
		switch err {
		case errStatementPasswordRequired:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_password_required"})
		case errStatementPasswordWrong:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_password_incorrect"})
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_unreadable"})
		}
		return statementReadResponse{}, false
	}

	today := s.statementToday()
	summary := s.summarizeStatementText(c.Request.Context(), text, today)

	fallbackYear := today.Year()
	if date, err := parseStrictAPIDate(summary.StatementDate); err == nil {
		fallbackYear = date.Year()
	}
	lines := parseStatementText(text, fallbackYear)
	if len(lines) == 0 && summary.isEmpty() {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_transactions_found"})
		return statementReadResponse{}, false
	}
	return statementReadResponse{Summary: summary, Lines: lines, Source: "pdf"}, true
}

// summarizeStatementText is best-effort: a disabled, tripped or failing model
// returns an empty summary and the PDF read carries on without it.
func (s *Server) summarizeStatementText(ctx context.Context, text string, today time.Time) statementSummary {
	reader, ok := s.parser.(ai.StatementSummaryReader)
	if !ok || s.cfg.AIParseDisabled {
		return statementSummary{}
	}
	if s.cfg.AIProviderFailureThreshold > 0 && s.cfg.AIProviderCircuitBreakerSeconds > 0 {
		if allowed, _ := s.circuitBreaker().allow(time.Now().UTC()); !allowed {
			return statementSummary{}
		}
	}
	timeout := s.cfg.ReqTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	raw, _, err := reader.ExtractStatementSummary(ctx, text, today.Format(apiDateLayout))
	if err != nil {
		s.recordAIProviderFailure()
		log.Printf("statement summary read failed: %v", err)
		return statementSummary{}
	}
	s.recordAIProviderSuccess()
	summary, err := decodeStatementSummary(raw, text)
	if err != nil {
		log.Printf("statement summary unusable: %v", err)
		return statementSummary{}
	}
	return summary
}

// readStatementScreenshots is the paid read: rows and summary in one vision
// call, charged as the existing statement-screenshot action.
func (s *Server) readStatementScreenshots(c *gin.Context) (statementReadResponse, bool) {
	if s.rejectIfAIParseDisabled(c) || s.rejectIfProviderCircuitOpen(c) {
		return statementReadResponse{}, false
	}
	reader, ok := s.parser.(ai.StatementSummaryReader)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "statement_image_parser_unavailable"})
		return statementReadResponse{}, false
	}
	action, err := ai.DefaultActionRegistry().RequireImplemented(ai.ActionFutureAIStatementImport)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "statement_image_parser_unavailable"})
		return statementReadResponse{}, false
	}
	images, ok := readStatementScreenshotForm(c, action.InputLimits.MaxFileBytes)
	if !ok {
		return statementReadResponse{}, false
	}
	defer func() {
		for index := range images {
			clear(images[index].Data)
			images[index].Data = nil
		}
	}()

	subject, subjectOK := parseCreditSubject(c)
	if !subjectOK {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return statementReadResponse{}, false
	}
	if s.rejectIfAIAbuseBlocked(c, subject) || s.rejectIfAIFailureCooldown(c, subject) {
		return statementReadResponse{}, false
	}
	creditService := billing.NewCreditService(database.DB)
	usageEvent, allowance, reserveErr := creditService.ReserveCredits(subject, action.Code, parseIdempotencyHeader(c))
	if reserveErr != nil {
		writeCreditReservationError(c, allowance, reserveErr)
		return statementReadResponse{}, false
	}

	timeout := s.cfg.ReqTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	raw, usage, err := reader.ReadStatementImages(ctx, images, s.statementToday().Format(apiDateLayout))
	if err != nil {
		s.recordAIProviderFailure()
		log.Printf("statement screenshot read failed: %v", err)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "statement_image_parse_failed",
		})
		c.JSON(http.StatusBadGateway, gin.H{"error": "statement_image_parse_failed"})
		return statementReadResponse{}, false
	}
	responseBytes := len(raw)
	lines, linesErr := decodeStatementImageLines(raw)
	// Screenshots cannot be checked against source text, so the summary is
	// validated for shape only.
	summary, _ := decodeStatementSummary(raw, "")
	if linesErr != nil && summary.isEmpty() {
		_, _ = creditService.FinalizeUsage(usageEvent.ID, withStatementTokens(billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "invalid_statement_image_response", ResponseBytes: &responseBytes,
		}, usage, s.cfg.OpenAILlmModel))
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_transactions_found"})
		return statementReadResponse{}, false
	}
	credits, err := s.finalizeParseSuccess(creditService, usageEvent.ID, subject, action.Code, 0, responseBytes, 0, usage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_finalization_failed"})
		return statementReadResponse{}, false
	}
	// Overlapping screenshots repeat rows; the statement does not exist yet,
	// so dedupe on the same key with no statement id.
	lines = dedupeStatementLines(0, lines)

	response := statementReadResponse{Summary: summary, Lines: lines, Source: "screenshots_ai"}
	var diff statementDiff
	setStatementDiffCredits(&diff, credits)
	response.CreditsCharged = diff.CreditsCharged
	response.CreditsRemainingToday = diff.CreditsRemainingToday
	response.CreditsRemainingTotal = diff.CreditsRemainingTotal
	return response, true
}

func (s *Server) statementToday() time.Time {
	loc, err := time.LoadLocation(s.cfg.TZDefault)
	if err != nil || loc == nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	return time.Now().In(loc)
}

// decodeStatementSummary keeps only what is well-formed. With sourceText set
// (a PDF), an amount is kept only when it is printed in that text — the
// model may read, never invent.
func decodeStatementSummary(raw []byte, sourceText string) (statementSummary, error) {
	trimmed := strings.TrimSpace(string(raw))
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	var envelope struct {
		Summary struct {
			StatementDate  string        `json:"statement_date"`
			DueDate        string        `json:"due_date"`
			TotalDue       *models.Money `json:"total_due"`
			MinimumDue     *models.Money `json:"minimum_due"`
			CreditLimit    *models.Money `json:"credit_limit"`
			AvailableLimit *models.Money `json:"available_limit"`
			OpeningBalance *models.Money `json:"opening_balance"`
			Payments       *models.Money `json:"payments"`
			Purchases      *models.Money `json:"purchases"`
			FeesAndCharges *models.Money `json:"fees_and_charges"`
			AnnualFee      *models.Money `json:"annual_fee"`
			FeeWaiverSpend *models.Money `json:"fee_waiver_spend"`
			RenewalMonth   string        `json:"renewal_month"`
			CardLast4      string        `json:"card_last4"`
			Issuer         string        `json:"issuer"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &envelope); err != nil {
		return statementSummary{}, err
	}
	in := envelope.Summary
	var printed map[models.Money]bool
	if sourceText != "" {
		printed = printedAmounts(sourceText)
	}

	amount := func(value *models.Money, allowZero bool) *models.Money {
		if value == nil || *value < 0 || (!allowZero && *value == 0) {
			return nil
		}
		if printed != nil && *value != 0 && !printed[*value] {
			return nil
		}
		copy := *value
		return &copy
	}

	out := statementSummary{
		TotalDue:       amount(in.TotalDue, true),
		MinimumDue:     amount(in.MinimumDue, true),
		CreditLimit:    amount(in.CreditLimit, false),
		AvailableLimit: amount(in.AvailableLimit, true),
		OpeningBalance: amount(in.OpeningBalance, true),
		Payments:       amount(in.Payments, true),
		Purchases:      amount(in.Purchases, true),
		FeesAndCharges: amount(in.FeesAndCharges, true),
		AnnualFee:      amount(in.AnnualFee, false),
		FeeWaiverSpend: amount(in.FeeWaiverSpend, false),
	}
	if date, err := parseStrictAPIDate(in.StatementDate); err == nil {
		out.StatementDate = date.Format(apiDateLayout)
		if due, err := parseStrictAPIDate(in.DueDate); err == nil && due.After(date) &&
			due.Sub(date) <= 60*24*time.Hour {
			out.DueDate = due.Format(apiDateLayout)
		}
	}
	if out.TotalDue != nil && out.MinimumDue != nil && *out.MinimumDue > *out.TotalDue {
		out.MinimumDue = nil
	}
	for _, month := range monthNames {
		if strings.EqualFold(strings.TrimSpace(in.RenewalMonth), month) {
			out.RenewalMonth = month
		}
	}
	if last4 := strings.TrimSpace(in.CardLast4); fourDigits.MatchString(last4) {
		out.CardLast4 = last4
	}
	if issuer := strings.Join(strings.Fields(in.Issuer), " "); len(issuer) <= 60 {
		out.Issuer = issuer
	}
	return out, nil
}

// printedNumber is one figure as a statement prints it: digits with optional
// Indian or western grouping and optional paise. Matching whole figures, not
// substrings, is what stops an invented ₹500 fee "appearing" inside ₹1,50,000.
var printedNumber = regexp.MustCompile(`\d[\d,]*(?:\.\d{1,2})?`)

func printedAmounts(text string) map[models.Money]bool {
	amounts := map[models.Money]bool{}
	for _, match := range printedNumber.FindAllString(text, -1) {
		if value, err := models.ParseMoney(strings.ReplaceAll(match, ",", "")); err == nil {
			amounts[value] = true
		}
	}
	return amounts
}

// compareCardWithStatement lists the card settings the statement disagrees
// with, and warns when the statement is for a different card altogether.
func compareCardWithStatement(account models.Account, summary statementSummary) ([]cardUpdate, []string) {
	updates := []cardUpdate{}
	warnings := []string{}

	if summary.CardLast4 != "" && account.Last4 != "" && summary.CardLast4 != account.Last4 {
		warnings = append(warnings,
			"This statement is for a card ending "+summary.CardLast4+", but this card ends "+account.Last4+". Check you picked the right card.")
		// Nothing about a different card should be copied onto this one.
		return updates, warnings
	}

	// Against the clamped anchor: a card billing on the 31st that is dated
	// the 30th in November has not moved.
	if date, err := parseStrictAPIDate(summary.StatementDate); err == nil &&
		!clampDayToMonth(date.Year(), date.Month(), account.StatementDay).Equal(date) {
		updates = append(updates, cardUpdate{"statement_day", "Statement day", account.StatementDay, date.Day()})
	}
	if date, err := parseStrictAPIDate(summary.DueDate); err == nil && account.DueDay != date.Day() {
		updates = append(updates, cardUpdate{"due_day", "Due day", account.DueDay, date.Day()})
	}
	if summary.CreditLimit != nil && *summary.CreditLimit != account.CreditLimit {
		updates = append(updates, cardUpdate{"credit_limit", "Credit limit", account.CreditLimit, *summary.CreditLimit})
	}
	if summary.AnnualFee != nil && *summary.AnnualFee != account.AnnualFee {
		updates = append(updates, cardUpdate{"annual_fee", "Annual fee", account.AnnualFee, *summary.AnnualFee})
	}
	if summary.FeeWaiverSpend != nil && *summary.FeeWaiverSpend != account.FeeWaiverSpend {
		updates = append(updates, cardUpdate{"fee_waiver_spend", "Fee waived above (yearly spend)", account.FeeWaiverSpend, *summary.FeeWaiverSpend})
	}
	if summary.RenewalMonth != "" && !strings.EqualFold(summary.RenewalMonth, strings.TrimSpace(account.FeeMonth)) {
		updates = append(updates, cardUpdate{"fee_month", "Annual fee month", account.FeeMonth, summary.RenewalMonth})
	}
	if summary.CardLast4 != "" && account.Last4 == "" {
		updates = append(updates, cardUpdate{"last4", "Card ending", "", summary.CardLast4})
	}
	return updates, warnings
}
