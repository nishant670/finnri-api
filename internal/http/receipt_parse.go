package http

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

// handleParseReceipt is the photo producer for the capture draft. It returns
// exactly what /v1/parse returns for a capture, so the app's review sheet does
// not need to know the draft came from a picture.
//
// The image is read in memory, sent once to the vision model and released. It
// is not stored here: if the user keeps it as the entry's receipt, the app
// uploads it through /v1/upload on save like any other attachment.
func (s *Server) handleParseReceipt(c *gin.Context) {
	if s.rejectIfAIParseDisabled(c) || s.rejectIfProviderCircuitOpen(c) {
		return
	}
	vision, ok := s.parser.(ai.ReceiptImageParser)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "receipt_parser_unavailable"})
		return
	}
	action, err := ai.DefaultActionRegistry().RequireImplemented(ai.ActionTransactionParseReceipt)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "receipt_parser_unavailable"})
		return
	}

	file, err := c.FormFile("image")
	if err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "image_required"})
		return
	}
	maxBytes := action.InputLimits.MaxFileBytes
	if file.Size <= 0 || file.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "receipt_image_too_large", "max_bytes": maxBytes})
		return
	}
	image, err := readStatementScreenshot(file, maxBytes)
	if err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "receipt_image_too_large", "max_bytes": maxBytes})
		} else {
			c.JSON(http.StatusUnsupportedMediaType, gin.H{
				"error": "unsupported_receipt_image", "message": "Use a JPEG, PNG or WebP photo.",
			})
		}
		return
	}
	defer func() {
		clear(image.Data)
		image.Data = nil
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
	usageEvent, allowance, reserveErr := creditService.ReserveCredits(subject, action.Code, parseIdempotencyHeader(c))
	if reserveErr != nil {
		writeCreditReservationError(c, allowance, reserveErr)
		return
	}

	tz := c.PostForm("tz")
	if tz == "" {
		tz = s.cfg.TZDefault
	}
	requestTimeout := s.cfg.ReqTimeoutSec
	if requestTimeout <= 0 {
		requestTimeout = 30
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(requestTimeout)*time.Second)
	defer cancel()

	// A receipt read that does not end in a draft costs the user nothing. The
	// photo is a single attempt with no partial result to keep, and charging
	// 25 credits for "could not read a total" (or for our own provider error)
	// is what made a bad photo feel like a penalty. The event still records
	// failed_after_provider, so the failure cooldown counts it and a run of
	// junk photos is still slowed down.
	noCharge := 0

	parsed, usage, err := vision.ParseReceiptImage(ctx, image, todayIn(tz))
	if err != nil {
		s.recordAIProviderFailure()
		log.Printf("receipt image parse failed: %v", err)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "could_not_parse",
			FinalCredits: &noCharge,
		})
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "could_not_parse", "credits_charged": 0})
		return
	}

	// Every failure from here on followed an answered provider call, so it
	// carries what that answer cost.
	promptTokens, completionTokens, totalTokens := tokenPointers(usage)
	responseBytes := len(parsed)
	model := usage.Model
	if model == "" {
		model = s.cfg.OpenAILlmModel
	}
	fail := func(status int, code string, body gin.H) {
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        code,
			Provider:         "openai",
			Model:            model,
			FinalCredits:     &noCharge,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			ResponseBytes:    &responseBytes,
		})
		body["error"] = code
		body["credits_charged"] = 0
		c.JSON(status, body)
	}

	draft, summary, signals, err := decodeReceiptDraft(parsed)
	if err != nil {
		fail(http.StatusInternalServerError, "invalid_parse_response", gin.H{})
		return
	}
	normalizeParsedDraft(draft, summary)
	flagForeignCurrency(draft)
	flagDoubtfulReceiptAmount(draft, signals)
	if amount, ok := draft["amount"].(float64); !ok || amount <= 0 {
		// A receipt with no readable total is not a draft worth reviewing; the
		// user would be re-typing the one number the photo was meant to give.
		fail(http.StatusUnprocessableEntity, "receipt_unreadable", gin.H{
			"message":    "Finnri could not read a total on that receipt.",
			"transcript": summary,
		})
		return
	}

	encoded, err := json.Marshal(draft)
	if err != nil {
		fail(http.StatusInternalServerError, "serialization_failed", gin.H{})
		return
	}
	result, err := s.validator.Validate(gojsonschema.NewBytesLoader(encoded))
	if err != nil {
		fail(http.StatusInternalServerError, "validation_failed", gin.H{})
		return
	}
	if !result.Valid() {
		repaired, dropped, recovered := s.repairInvalidParsedDraft(draft)
		if !recovered {
			details := []string{}
			for _, problem := range result.Errors() {
				details = append(details, problem.String())
			}
			log.Printf("receipt parse schema_invalid: %v", details)
			fail(http.StatusUnprocessableEntity, "schema_invalid", gin.H{"transcript": summary})
			return
		}
		log.Printf("receipt parse schema_invalid recovered by dropping %v", dropped)
		encoded = repaired
	}

	credits, err := s.finalizeParseSuccess(creditService, usageEvent.ID, subject, action.Code, 0, len(encoded), 0, usage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_finalization_failed"})
		return
	}
	for key, value := range credits {
		draft[key] = value
	}
	c.JSON(http.StatusOK, draft)
}

// receiptSignals are what the model says about the photo rather than the
// purchase: how many papers it saw and which figure it took the total from.
// They never reach the app; they decide whether the amount gets flagged.
type receiptSignals struct {
	documentsInPhoto int
	otherPapers      string
	otherTotals      []otherReceiptTotal
	totalLabel       string
	totalAsPrinted   string
}

// otherReceiptTotal is another paper in the same photo, as the model read it.
type otherReceiptTotal struct {
	shop   string
	amount float64
}

// decodeReceiptDraft unwraps the model's JSON and pulls out the one-line
// summary, which becomes the draft's source_text — the line the review sheet
// shows as "what Finnri read".
func decodeReceiptDraft(raw []byte) (map[string]any, string, receiptSignals, error) {
	trimmed := strings.TrimSpace(string(raw))
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")

	var draft map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &draft); err != nil {
		return nil, "", receiptSignals{}, err
	}
	summary, _ := draft["summary"].(string)
	summary = strings.Join(strings.Fields(summary), " ")
	delete(draft, "summary")
	if summary == "" {
		summary = "Receipt photo"
	}
	// A receipt is always a capture. The intent/query keys belong to the text
	// channel and are dropped so a stray one cannot route this anywhere else.
	delete(draft, "intent")
	delete(draft, "query")

	var signals receiptSignals
	if documents, ok := draft["documents_in_photo"].(float64); ok && documents > 0 {
		signals.documentsInPhoto = int(documents)
	}
	signals.otherPapers, _ = draft["other_papers"].(string)
	signals.otherPapers = strings.ToLower(strings.TrimSpace(signals.otherPapers))
	if details, ok := draft["other_papers_detail"].([]any); ok {
		for _, raw := range details {
			detail, _ := raw.(map[string]any)
			shop, _ := detail["shop"].(string)
			amount, _ := detail["amount"].(float64)
			signals.otherTotals = append(signals.otherTotals, otherReceiptTotal{shop: strings.TrimSpace(shop), amount: amount})
		}
	}
	signals.totalLabel, _ = draft["total_label"].(string)
	signals.totalAsPrinted, _ = draft["total_as_printed"].(string)
	for _, key := range []string{"documents_in_photo", "other_papers", "other_papers_detail", "total_label", "total_as_printed"} {
		delete(draft, key)
	}
	return draft, summary, signals, nil
}

// flagDoubtfulReceiptAmount sends the amount to "check these first" when the
// photo gives a reason to doubt it, instead of presenting it as certain:
//   - other papers in the photo that are not this purchase, which is how a
//     petrol bill got an unrelated card slip's ₹7,647.60. The model's own
//     "same_purchase" is not taken on trust: a paper whose total differs is
//     unrelated whatever it was labelled;
//   - an amount that disagrees with the figure the model says it copied.
//
// The amount itself is left alone; the user confirms or corrects it, and the
// clarification names the other paper's total so the fix is one edit.
func flagDoubtfulReceiptAmount(draft map[string]any, signals receiptSignals) {
	amount, ok := draft["amount"].(float64)
	if !ok || amount <= 0 {
		return
	}
	unrelated := signals.otherPapers == "unrelated" ||
		(signals.documentsInPhoto > 1 && signals.otherPapers != "same_purchase")
	var other *otherReceiptTotal
	for i := range signals.otherTotals {
		candidate := &signals.otherTotals[i]
		if candidate.amount > 0 && math.Abs(candidate.amount-amount) >= 1 {
			unrelated = true
			if other == nil {
				other = candidate
			}
		}
	}
	if unrelated {
		message := "There is more than one receipt in this photo. Check the amount is from the right one."
		if other != nil {
			described := rupeesForCopy(models.Money(math.Round(other.amount * 100)))
			if other.shop != "" {
				described = other.shop + ", " + described
			}
			message = "This photo also shows another receipt (" + described + "). Check the amount is from the right one."
		}
		flagReceiptAmount(draft, message)
		// The bank on the review sheet becomes an "Add HDFC Bank account?"
		// offer. With an unrelated paper in the photo there is no telling
		// which paper named it, and an offer to create the wrong account is
		// worse than no hint: the account picker still defaults by mode.
		delete(draft, "account_hint")
		delete(draft, "card_network")
		if needsConfirmation, ok := draft["needs_confirmation"].(map[string]any); ok {
			delete(needsConfirmation, "account_hint")
		}
	}
	if printed, ok := parsePrintedAmount(signals.totalAsPrinted); ok && math.Abs(printed-amount) >= 0.01 {
		flagReceiptAmount(draft, "Check the amount against the bill.")
	}
}

// flagReceiptAmount marks the amount for confirmation and caps its confidence,
// so the review sheet lists it first.
func flagReceiptAmount(draft map[string]any, clarification string) {
	needsConfirmation, _ := draft["needs_confirmation"].(map[string]any)
	if needsConfirmation == nil {
		needsConfirmation = map[string]any{}
	}
	needsConfirmation["amount"] = true
	draft["needs_confirmation"] = needsConfirmation
	confidence, _ := draft["confidence"].(map[string]any)
	if confidence == nil {
		confidence = map[string]any{}
		draft["confidence"] = confidence
	}
	if current, ok := confidence["amount"].(float64); !ok || current > 0.5 {
		confidence["amount"] = 0.5
	}
	draft["clarifications"] = appendUniqueAnyString(draft["clarifications"], clarification)
}

var printedAmountPattern = regexp.MustCompile(`\d[\d,]*(?:\.\d{1,2})?`)

// parsePrintedAmount reads a figure copied as printed: "Rs 3,301.00",
// "₹450", "1,26,560.00". The last number in the string is the amount, so a
// stray label digit before it ("Total (2 items) 548") does not win.
func parsePrintedAmount(printed string) (float64, bool) {
	matches := printedAmountPattern.FindAllString(printed, -1)
	if len(matches) == 0 {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(matches[len(matches)-1], ",", ""), 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// flagForeignCurrency keeps a bill printed in another currency saveable.
// Entries are INR-only, so the draft is switched to INR and the amount is
// flagged: the printed figure is a foreign amount the user must replace with
// what the card or account was actually charged.
func flagForeignCurrency(draft map[string]any) {
	currency, _ := draft["currency"].(string)
	if currency == "" || currency == "INR" {
		return
	}
	draft["currency"] = "INR"
	needsConfirmation, _ := draft["needs_confirmation"].(map[string]any)
	if needsConfirmation == nil {
		needsConfirmation = map[string]any{}
	}
	needsConfirmation["amount"] = true
	draft["needs_confirmation"] = needsConfirmation
	if confidence, ok := draft["confidence"].(map[string]any); ok {
		confidence["amount"] = 0.0
	}
	draft["clarifications"] = appendUniqueAnyString(
		draft["clarifications"],
		"This bill is in "+currency+". Enter the rupee amount you were charged.",
	)
}

func todayIn(tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	return time.Now().In(loc).Format("2006-01-02")
}
