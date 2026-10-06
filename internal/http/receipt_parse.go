package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
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

	parsed, usage, err := vision.ParseReceiptImage(ctx, image, todayIn(tz))
	if err != nil {
		s.recordAIProviderFailure()
		log.Printf("receipt image parse failed: %v", err)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "could_not_parse",
		})
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "could_not_parse"})
		return
	}

	// Every failure from here on followed an answered provider call, so it
	// carries what that answer cost.
	promptTokens, completionTokens, totalTokens := tokenPointers(usage)
	responseBytes := len(parsed)
	fail := func(status int, code string, body gin.H) {
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        code,
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			ResponseBytes:    &responseBytes,
		})
		body["error"] = code
		c.JSON(status, body)
	}

	draft, summary, err := decodeReceiptDraft(parsed)
	if err != nil {
		fail(http.StatusInternalServerError, "invalid_parse_response", gin.H{})
		return
	}
	normalizeParsedDraft(draft, summary)
	flagForeignCurrency(draft)
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

// decodeReceiptDraft unwraps the model's JSON and pulls out the one-line
// summary, which becomes the draft's source_text — the line the review sheet
// shows as "what Finnri read".
func decodeReceiptDraft(raw []byte) (map[string]any, string, error) {
	trimmed := strings.TrimSpace(string(raw))
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")

	var draft map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &draft); err != nil {
		return nil, "", err
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
	return draft, summary, nil
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
