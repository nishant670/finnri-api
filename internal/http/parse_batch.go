package http

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"time"
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

// batchItemNullKeys are the capture keys a batch item may leave out to save
// output tokens (the prompt asks it to). They are put back as null, so each
// item reaches the app in exactly the shape of a single draft.
var batchItemNullKeys = []string{
	"type", "title", "time", "amount", "currency", "mode", "card_network", "account_hint",
	"category", "merchant", "tag", "purpose_type", "note", "date", "recurring_candidate",
	"subscription_candidate", "event_name", "split_candidate", "split_candidate_details",
}

func fillBatchItemDefaults(item map[string]any) {
	for _, key := range batchItemNullKeys {
		if _, present := item[key]; !present {
			item[key] = nil
		}
	}
	if _, present := item["tags"]; !present {
		item["tags"] = []any{}
	}
}

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
	tz            string
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
	fillBatchItemDefaults(item)
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
	read := make([]map[string]any, 0, len(items))
	dropped := 0
	for _, item := range items {
		if s.readBatchItem(item, req.transcript, req) {
			kept = append(kept, item)
			read = append(read, item)
		} else {
			dropped++
		}
	}
	fillSpokenDates(read, req.transcript, todayIn(req.tz))

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

// spokenDayWords are the day words a spoken day says once for everything after
// it. "kal" and "parso" are left out: each means both a day before and a day
// after, so the model's reading of them stands.
var spokenDayWords = regexp.MustCompile(`(?i)\b(day before yesterday|yesterday|today)\b`)

// otherDayCues are words that may point at a day other than today without
// saying which: a weekday, a month, "kal", "last week", "3 days ago", "the
// 5th". Where one comes before an item, an undated item stays undated and
// flagged. Words that only look like a cue ("last", "may", "sat") cost nothing
// worse than the flag, which is what every undated item had before.
var otherDayCues = regexp.MustCompile(`(?i)\b(kal|parso|parson|monday|tuesday|wednesday|thursday|friday|saturday|sunday|mon|tue|tues|wed|thu|thur|thurs|fri|sat|sun|week|weekend|month|ago|last|pichhle|pichle|jan|january|feb|february|mar|march|apr|april|may|jun|june|jul|july|aug|august|sep|sept|september|oct|october|nov|november|dec|december|\d{1,2}(st|nd|rd|th))\b|\b\d{1,2}[/-]\d{1,2}\b`)

func spokenDayOffset(word string) int {
	switch strings.ToLower(word) {
	case "day before yesterday":
		return -2
	case "yesterday":
		return -1
	default:
		return 0
	}
}

/*
fillSpokenDates is the server's own hold on "said once, carries forward" for
dates. "Yesterday I had sweets for 176, then lunch 140, then paid Aditya 140"
came back on 26 Nov with no date on any item: the lean batch leaves a key out
when the model doesn't fill it, and the app then showed three rows dated today,
each saying "Date unclear, check it".

An item with no date takes the last day word said before or within its own
words, after the previous item's; failing that, the previous item's date.
Items the model dated keep their date, and pass it on.

An item that is still undated, with nothing before it that could mean another
day (otherDayCues), is today: a day told without a date is today's, as the
app's sheet already says (24 Nov retest: every row of "glue 20, tape 30" read
"Date unclear, check it"). The same goes for an item the model dated today but
flagged. Where such a cue was said, the item keeps its flag for the person.
*/
func fillSpokenDates(items []map[string]any, transcript, today string) {
	todayDate, err := time.Parse("2006-01-02", today)
	if err != nil || len(items) == 0 {
		return
	}
	lower := strings.ToLower(transcript)
	words := [][]int{}
	for _, word := range spokenDayWords.FindAllStringIndex(transcript, -1) {
		// "for yesterday's dinner" names another day's thing, not the day
		// being told: Priya paying back today stays today.
		rest := transcript[word[1]:]
		if strings.HasPrefix(rest, "'s") || strings.HasPrefix(rest, "’s") {
			continue
		}
		words = append(words, word)
	}
	otherDay := otherDayCues.FindAllStringIndex(transcript, -1)
	cueBefore := func(end int) bool {
		if end < 0 {
			end = len(transcript)
		}
		return len(otherDay) > 0 && otherDay[0][0] < end
	}
	cursor := 0
	carried := ""
	for _, item := range items {
		end := -1
		if source, _ := item["source_text"].(string); strings.TrimSpace(source) != "" {
			phrase := strings.ToLower(strings.TrimSpace(source))
			if phrase != strings.TrimSpace(lower) {
				if at := strings.Index(lower[cursor:], phrase); at >= 0 {
					end = cursor + at + len(phrase)
				}
			}
		}
		if date, ok := item["date"].(string); ok && strings.TrimSpace(date) != "" {
			carried = date
			if date == today && !cueBefore(end) {
				clearDateFlags(item)
			}
			if end >= 0 {
				cursor = end
			}
			continue
		}
		date := carried
		if end >= 0 {
			for _, word := range words {
				if word[0] >= cursor && word[0] < end {
					offset := spokenDayOffset(transcript[word[0]:word[1]])
					date = todayDate.AddDate(0, 0, offset).Format("2006-01-02")
				}
			}
			cursor = end
		}
		if date == "" && !cueBefore(end) {
			date = today
		}
		if date == "" {
			continue
		}
		item["date"] = date
		carried = date
		clearDateFlags(item)
	}
}

// clearDateFlags takes "date" off an item's confirm and missing lists, once a
// date the person said has been put on it.
func clearDateFlags(item map[string]any) {
	if flags, ok := item["needs_confirmation"].(map[string]any); ok {
		delete(flags, "date")
	}
	if confidence, ok := item["confidence"].(map[string]any); ok {
		delete(confidence, "date")
	}
	switch missing := item["missing_fields"].(type) {
	case []string:
		kept := make([]string, 0, len(missing))
		for _, field := range missing {
			if field != "date" {
				kept = append(kept, field)
			}
		}
		item["missing_fields"] = kept
	case []any:
		kept := make([]any, 0, len(missing))
		for _, field := range missing {
			if field != "date" {
				kept = append(kept, field)
			}
		}
		item["missing_fields"] = kept
	}
}
