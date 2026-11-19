package http

import (
	"encoding/json"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/models"
)

/*
Speak your day (canvas Round 21, P103–P117, approved 2026-11-20).

An app that sends `multi=1` may get several transactions back from one text or
recording: {"stage":"batch","items":[draft, …]}. Each item is today's draft,
normalised, checked and repaired exactly as a single read is, so the app's
form code runs on it unchanged. Two keys ride along on each item and are taken
off for the schema check: "time_slot" (P108; the app turns it into a time) and
"mode_carried" (P109; the row says "as above").

It is one AI read, so it is charged once, at the text or voice price (P106).
An item that can't be read is dropped and counted; when none can, the read
failed and costs nothing.
*/

var batchTimeSlots = map[string]bool{"morning": true, "afternoon": true, "evening": true, "night": true}

// parsedBatchItems returns the items of a batch answer, or false for a single
// draft or a question.
func parsedBatchItems(parsed map[string]any) ([]map[string]any, bool) {
	if stage, _ := parsed["stage"].(string); stage != "batch" {
		if _, hasItems := parsed["items"]; !hasItems {
			return nil, false
		}
	}
	raw, ok := parsed["items"].([]any)
	if !ok {
		return nil, false
	}
	items := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		if item, ok := value.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items, true
}

// takeBatchExtras removes the two batch-only keys from an item, so it passes
// the draft schema, and returns them for putting back afterwards.
func takeBatchExtras(item map[string]any) (any, bool) {
	var slot any
	if value, ok := item["time_slot"].(string); ok {
		value = strings.ToLower(strings.TrimSpace(value))
		if batchTimeSlots[value] {
			slot = value
		}
	}
	carried, _ := item["mode_carried"].(bool)
	delete(item, "time_slot")
	delete(item, "mode_carried")
	return slot, carried
}

type batchParseRequest struct {
	transcript    string
	items         []map[string]any
	overCapText   string
	knownNames    ai.KnownNames
	knownEvents   []models.Event
	creditService *billing.CreditService
	usageEvent    *models.AIUsageEvent
	subject       billing.CreditSubject
	actionCode    ai.ActionCode
	audioSize     int64
	responseBytes int
	reported      ai.Usage
}

// readBatchItem runs one item through the single-draft pipeline. False means
// the item is not a readable transaction and is dropped.
func (s *Server) readBatchItem(item map[string]any, transcript string, req batchParseRequest) bool {
	slot, carried := takeBatchExtras(item)
	source, _ := item["source_text"].(string)
	source = strings.TrimSpace(source)
	if source == "" {
		source = transcript
	}
	normalizeParsedDraft(item, source)
	dropUnstatedSubscription(item, source, req.knownNames)
	resolveParsedEvent(item, req.knownEvents)
	if !parsedDraftHasTransactionSignal(item) {
		return false
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return false
	}
	result, err := s.validator.Validate(gojsonschema.NewBytesLoader(encoded))
	if err != nil {
		return false
	}
	if !result.Valid() {
		if _, dropped, recovered := s.repairInvalidParsedDraft(item); !recovered {
			return false
		} else {
			log.Printf("parse batch item recovered by dropping %v", dropped)
		}
	}
	item["time_slot"] = slot
	item["mode_carried"] = carried
	return true
}

// respondParsedBatch answers a multi=1 read whose model answer held several
// items. The caller has already settled that the answer is a batch with at
// least two items.
func (s *Server) respondParsedBatch(c *gin.Context, req batchParseRequest) {
	items := req.items
	overCap := strings.TrimSpace(req.overCapText)
	if len(items) > ai.MaxBatchItems {
		// The model was told to stop at ten. If it didn't, the rest are not
		// lost: their words go back as text the app can offer to read (P114).
		extra := []string{}
		for _, item := range items[ai.MaxBatchItems:] {
			if source, _ := item["source_text"].(string); strings.TrimSpace(source) != "" {
				extra = append(extra, strings.TrimSpace(source))
			}
		}
		if overCap != "" {
			extra = append([]string{overCap}, extra...)
		}
		overCap = strings.Join(extra, ", ")
		items = items[:ai.MaxBatchItems]
	}

	kept := make([]any, 0, len(items))
	dropped := 0
	for _, item := range items {
		if s.readBatchItem(item, req.transcript, req) {
			kept = append(kept, item)
		} else {
			dropped++
		}
	}

	inputChars := utf8.RuneCountInString(req.transcript)
	if len(kept) == 0 {
		// Nothing understood is free (P106), like an unreadable bill.
		noCharge := 0
		prompt, completion, total := tokenPointers(req.reported)
		_, _ = req.creditService.FinalizeUsage(req.usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "non_transactional_prompt",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			FinalCredits:     &noCharge,
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      total,
			InputChars:       &inputChars,
			ResponseBytes:    &req.responseBytes,
			AudioBytes:       optionalInt64(req.audioSize),
		})
		c.JSON(422, gin.H{
			"error":           "non_transactional_prompt",
			"message":         "Finnri couldn't find any expenses in that, so no credits were used. Say each one with its amount, like “lunch 450”.",
			"transcript":      req.transcript,
			"credits_charged": 0,
		})
		return
	}

	credits, err := s.finalizeParseSuccess(
		req.creditService,
		req.usageEvent.ID,
		req.subject,
		req.actionCode,
		inputChars,
		req.responseBytes,
		req.audioSize,
		req.reported,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": "credit_finalization_failed"})
		return
	}
	var overCapValue any
	if overCap != "" {
		overCapValue = overCap
	}
	response := gin.H{
		"stage":         "batch",
		"kind":          "batch",
		"source_text":   req.transcript,
		"items":         kept,
		"dropped":       dropped,
		"over_cap_text": overCapValue,
		"max_items":     ai.MaxBatchItems,
		"ai_request_id": req.usageEvent.RequestID,
	}
	for key, value := range credits {
		response[key] = value
	}
	c.JSON(200, response)
}
