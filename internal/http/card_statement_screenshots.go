package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

const maxStatementScreenshots = 8

var statementScreenshotMIMEs = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

type statementImageResponse struct {
	Lines []statementLine `json:"lines"`
}

// uploadCardStatementScreenshots is the second producer for the existing
// statement diff/import pipeline. Images are read in memory, sent once to the
// configured AI service, then released; only the parsed rows reach the
// matcher. Nothing is persisted until the user explicitly imports rows.
func (s *Server) uploadCardStatementScreenshots(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statement, ok := loadStatementForRequest(c, userID)
	if !ok {
		return
	}

	if s.rejectIfAIParseDisabled(c) || s.rejectIfProviderCircuitOpen(c) {
		return
	}
	vision, ok := s.parser.(ai.StatementImageParser)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "statement_image_parser_unavailable"})
		return
	}
	action, err := ai.DefaultActionRegistry().RequireImplemented(ai.ActionFutureAIStatementImport)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "statement_image_parser_unavailable"})
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "images_required"})
		return
	}
	files := form.File["images"]
	if len(files) == 0 {
		// Accept the singular field too, which makes curl and simple clients less
		// surprising while the mobile app uses the documented plural field.
		files = form.File["image"]
	}
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "images_required"})
		return
	}
	if len(files) > maxStatementScreenshots {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "too_many_statement_images", "max_images": maxStatementScreenshots,
		})
		return
	}

	maxBytes := action.InputLimits.MaxFileBytes
	images := make([]ai.StatementImage, 0, len(files))
	var totalBytes int64
	for _, file := range files {
		if file.Size <= 0 || (maxBytes > 0 && totalBytes+file.Size > maxBytes) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "statement_images_too_large", "max_bytes": maxBytes})
			return
		}
		image, readErr := readStatementScreenshot(file, maxBytes-totalBytes)
		if readErr != nil {
			if requestBodyTooLarge(readErr) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "statement_images_too_large", "max_bytes": maxBytes})
			} else {
				c.JSON(http.StatusUnsupportedMediaType, gin.H{
					"error": "unsupported_statement_image", "message": "Use JPEG, PNG or WebP screenshots.",
				})
			}
			return
		}
		totalBytes += int64(len(image.Data))
		images = append(images, image)
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
		return
	}
	if s.rejectIfAIAbuseBlocked(c, subject) || s.rejectIfAIFailureCooldown(c, subject) {
		return
	}
	creditService := billing.NewCreditService(database.DB)
	usageEvent, allowance, reserveErr := creditService.ReserveCredits(
		subject, action.Code, parseIdempotencyHeader(c),
	)
	if reserveErr != nil {
		writeCreditReservationError(c, allowance, reserveErr)
		return
	}

	requestTimeout := s.cfg.ReqTimeoutSec
	if requestTimeout <= 0 {
		requestTimeout = 30
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(requestTimeout)*time.Second)
	defer cancel()
	parsed, visionUsage, parseErr := vision.ParseStatementImages(ctx, images, statement.CycleStart, statement.CycleEnd)
	if parseErr != nil {
		s.recordAIProviderFailure()
		log.Printf("statement image parse failed: %v", parseErr)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "statement_image_parse_failed",
		})
		c.JSON(http.StatusBadGateway, gin.H{"error": "statement_image_parse_failed"})
		return
	}

	lines, decodeErr := decodeStatementImageLines(parsed)
	if decodeErr != nil {
		responseBytes := len(parsed)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "invalid_statement_image_response", ResponseBytes: &responseBytes,
		})
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_transactions_found"})
		return
	}
	lines = dedupeStatementLines(statement.ID, lines)

	diff, err := buildStatementDiff(statement, lines)
	if err != nil {
		responseBytes := len(parsed)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "failed_build_statement_diff", ResponseBytes: &responseBytes,
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_cycle_entries"})
		return
	}

	responseBytes := len(parsed)
	credits, err := s.finalizeParseSuccess(creditService, usageEvent.ID, subject, action.Code, 0, responseBytes, 0, visionUsage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_finalization_failed"})
		return
	}

	diff.Source = "screenshots_ai"
	setStatementDiffCredits(&diff, credits)
	c.JSON(http.StatusOK, diff)
}

func readStatementScreenshot(file *multipart.FileHeader, remaining int64) (ai.StatementImage, error) {
	opened, err := file.Open()
	if err != nil {
		return ai.StatementImage{}, err
	}
	defer opened.Close()
	if remaining <= 0 {
		return ai.StatementImage{}, &http.MaxBytesError{Limit: remaining}
	}
	data, err := io.ReadAll(io.LimitReader(opened, remaining+1))
	if err != nil {
		return ai.StatementImage{}, err
	}
	if int64(len(data)) > remaining {
		return ai.StatementImage{}, &http.MaxBytesError{Limit: remaining}
	}
	mimeType, _, _ := strings.Cut(http.DetectContentType(data), ";")
	mimeType = strings.TrimSpace(mimeType)
	if !statementScreenshotMIMEs[mimeType] {
		return ai.StatementImage{}, fmt.Errorf("unsupported statement image type %q", mimeType)
	}
	return ai.StatementImage{MIME: mimeType, Data: data}, nil
}

func decodeStatementImageLines(raw []byte) ([]statementLine, error) {
	trimmed := strings.TrimSpace(string(raw))
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")

	var response statementImageResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &response); err != nil {
		return nil, err
	}
	for index := range response.Lines {
		line := &response.Lines[index]
		line.Description = strings.Join(strings.Fields(line.Description), " ")
		if line.Description == "" {
			return nil, fmt.Errorf("line %d has no description", index)
		}
		if strings.EqualFold(strings.TrimSpace(line.Type), "income") {
			line.Type = "income"
		} else {
			line.Type = "expense"
		}
	}
	input := statementLinesInput{Lines: response.Lines}
	if fields := input.validate(); len(fields) > 0 {
		return nil, fmt.Errorf("invalid statement lines: %v", fields)
	}
	return response.Lines, nil
}

func dedupeStatementLines(statementID uint, lines []statementLine) []statementLine {
	seen := make(map[string]bool, len(lines))
	result := make([]statementLine, 0, len(lines))
	for _, line := range lines {
		key := statementLineIdempotencyKey(statementID, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, line)
	}
	return result
}

// checksumStatementLines asks whether the rows read off the file add up to the
// bill, which is how a cropped screenshot or a page the PDF reader skipped
// shows itself.
//
// A statement balances as closing = opening - payments + purchases, so the
// rows' purchases (spends, fees, interest and EMIs, less refunds) should equal
// closing - opening + payments. Bill payments are taken out of the rows first:
// counting last month's payment against this month's spending reported a
// false gap for everyone who pays in full.
//
// `opening` is the previous statement's total when Finnri has one. Without it
// the previous bill is assumed to have been paid in full inside this cycle —
// opening equals the payments — which is the common case and is said so in
// the message.
func checksumStatementLines(lines []statementLine, totalDue models.Money, opening *models.Money) statementChecksum {
	var debits, credits, payments models.Money
	for _, line := range lines {
		switch {
		case classifyLine(line) == lineKindPayment:
			payments += line.Amount
		case line.isCredit():
			credits += line.Amount
		default:
			debits += line.Amount
		}
	}
	parsedNet := debits - credits
	expectedNet := totalDue
	if opening != nil {
		expectedNet = totalDue - *opening + payments
	}
	difference := parsedNet - expectedNet
	matches := absMoney(difference) <= reconcileTolerance
	message := "The rows Finnri read add up to this bill."
	if !matches {
		message = "The rows Finnri read do not add up to this bill. A page or row may be missing from the file."
		if opening == nil {
			message += " Finnri has no earlier bill for this card, so it assumed last month was paid in full."
		}
	}
	return statementChecksum{
		ParsedDebits: debits, ParsedCredits: credits, ParsedNet: parsedNet,
		Payments: payments, OpeningBalance: opening,
		ExpectedNet: expectedNet, Difference: difference, Matches: matches, Message: message,
	}
}

func setStatementDiffCredits(diff *statementDiff, credits map[string]any) {
	set := func(key string, target **int) {
		value, ok := credits[key].(int)
		if ok {
			copy := value
			*target = &copy
		}
	}
	set("credits_charged", &diff.CreditsCharged)
	set("credits_remaining_today", &diff.CreditsRemainingToday)
	set("credits_remaining_total", &diff.CreditsRemainingTotal)
}
