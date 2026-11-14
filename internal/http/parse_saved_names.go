package http

import (
	"regexp"
	"strings"
	"unicode"

	"gorm.io/gorm"

	"finnri/internal/ai"
	"finnri/internal/models"
)

// loadKnownNames reads what a spoken or typed capture can point at by name: the
// user's events, split groups and friends. A failed read is not a failed parse
// — the capture still works, it just cannot match names — so errors leave the
// list empty rather than stopping the request.
func loadKnownNames(db *gorm.DB, userID uint) (ai.KnownNames, []models.Event) {
	var known ai.KnownNames
	if db == nil || userID == 0 {
		return known, nil
	}

	var events []models.Event
	if err := db.Where("user_id = ?", userID).
		Order("created_at desc, id desc").
		Limit(30).
		Find(&events).Error; err != nil {
		events = nil
	}
	for _, event := range events {
		known.Events = append(known.Events, event.Name)
	}

	groups := db.Model(&models.SplitGroup{}).Where("archived = ?", false)
	if shared, err := activeSharedSplitGroupIDs(db, userID); err == nil && len(shared) > 0 {
		groups = groups.Where("user_id = ? OR id IN ?", userID, shared)
	} else {
		groups = groups.Where("user_id = ?", userID)
	}
	var groupNames []string
	if err := groups.Order("updated_at desc").Limit(40).Pluck("name", &groupNames).Error; err == nil {
		known.Groups = groupNames
	}

	var friendNames []string
	if err := db.Model(&models.SplitFriend{}).
		Where("user_id = ? AND archived = ?", userID, false).
		Order("updated_at desc").
		Limit(60).
		Pluck("name", &friendNames).Error; err == nil {
		known.Friends = friendNames
	}
	return known, events
}

const maxParsedEventNameRunes = 60

// normalizeEventName keeps the event the user asked to file this under, as
// plain text. Only expenses belong to an event (see models.Event).
func normalizeEventName(entry map[string]any) {
	name, _ := entry["event_name"].(string)
	name = strings.Join(strings.Fields(name), " ")
	entryType, _ := entry["type"].(string)
	if name == "" || strings.EqualFold(entryType, "income") {
		entry["event_name"] = nil
		return
	}
	if runes := []rune(name); len(runes) > maxParsedEventNameRunes {
		name = string(runes[:maxParsedEventNameRunes])
	}
	entry["event_name"] = name
}

// resolveParsedEvent turns the spoken event into one of the user's events, as
// event_id. The model was shown the saved names and usually copies one, but a
// voice transcript can still bend it ("Kanatal" for "Kanatal trip"), so the
// match tolerates that — and gives up rather than guess between two.
func resolveParsedEvent(entry map[string]any, events []models.Event) {
	entry["event_id"] = nil
	name, _ := entry["event_name"].(string)
	if name == "" || len(events) == 0 {
		return
	}
	if event := matchEventByName(events, name); event != nil {
		entry["event_id"] = event.ID
		entry["event_name"] = event.Name
	}
}

func matchEventByName(events []models.Event, spoken string) *models.Event {
	for _, normalize := range []func(string) string{compactName, compactNameWithoutFiller} {
		target := normalize(spoken)
		if target == "" {
			continue
		}
		if found := uniqueEvent(events, func(name string) bool { return name == target }, normalize); found != nil {
			return found
		}
		if len([]rune(target)) >= 4 {
			if found := uniqueEvent(events, func(name string) bool {
				return len([]rune(name)) >= 4 && (strings.Contains(name, target) || strings.Contains(target, name))
			}, normalize); found != nil {
				return found
			}
		}
		if len([]rune(target)) >= 5 {
			if found := uniqueEvent(events, func(name string) bool {
				return editDistance(name, target) <= len([]rune(target))/6+1
			}, normalize); found != nil {
				return found
			}
		}
	}
	return nil
}

func uniqueEvent(events []models.Event, matches func(string) bool, normalize func(string) string) *models.Event {
	var found *models.Event
	for index := range events {
		if !matches(normalize(events[index].Name)) {
			continue
		}
		if found != nil {
			return nil
		}
		found = &events[index]
	}
	return found
}

// compactName is a name as it would sound: letters and digits only, lower case.
func compactName(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var eventFillerWords = map[string]bool{"the": true, "event": true, "my": true, "our": true}

func compactNameWithoutFiller(value string) string {
	words := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	kept := words[:0]
	for _, word := range words {
		if !eventFillerWords[word] {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, "")
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	previous := make([]int, len(rb)+1)
	current := make([]int, len(rb)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		current[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(rb)]
}

// recurringLanguage is what a sentence about a repeating payment says. Without
// any of it, a "Subscription" tag is the model reaching: "5000 to Chaukhat
// Hospitality for staying" came back tagged Subscription with nothing in it
// about a schedule (2026-11-11).
var recurringLanguage = regexp.MustCompile(`(?i)\b(` +
	`subscri\w*|recurr\w*|renew\w*|membership|auto[\s-]?debit\w*|auto[\s-]?pay\w*|mandate|standing instruction|sip|` +
	`daily|weekly|biweekly|fortnight\w*|monthly|quarterly|yearly|annual\w*|per annum|` +
	`every\s+(day|week|month|year|quarter|\d+)|each\s+(day|week|month|year)|per\s+(day|week|month|year)|a\s+(month|year)|` +
	`weekdays?|business\s+days?|market\s+days?|` +
	`har\s+(din|roz|hafte|mahine|mahina|saal)|mahina|mahine|saalana|rozana` +
	`)\b`)

// dropUnstatedSubscription takes the subscription back off a text or voice
// capture that never described a repeating payment. Receipts do not come
// through here: their text is a summary of the bill, not the user's words.
//
// Saved names are taken out of the sentence first: a group called "Bubu Dudu
// Daily" says nothing about how often anything is paid.
func dropUnstatedSubscription(entry map[string]any, transcript string, known ai.KnownNames) {
	if recurringLanguage.MatchString(withoutSavedNames(transcript, entry, known)) {
		return
	}
	tagIsSubscription := false
	if tag, ok := entry["tag"].(string); ok && strings.EqualFold(strings.TrimSpace(tag), "Subscription") {
		tagIsSubscription = true
	}
	if entry["subscription_candidate"] == nil && !tagIsSubscription && !tagsHave(entry, "Subscription") {
		return
	}
	entry["subscription_candidate"] = nil
	if recurring, _ := entry["recurring_candidate"].(bool); recurring {
		entry["recurring_candidate"] = nil
	}
	if tags, ok := entry["tags"].([]any); ok {
		kept := make([]any, 0, len(tags))
		for _, value := range tags {
			if tag, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(tag), "Subscription") {
				continue
			}
			kept = append(kept, value)
		}
		entry["tags"] = kept
	}
	if tagIsSubscription {
		entry["tag"] = nil
		normalizePrimaryTag(entry)
	}
}

var dailyGroupPhrase = regexp.MustCompile(`(?i)\bdaily\s+(group|grp)\b`)

func withoutSavedNames(transcript string, entry map[string]any, known ai.KnownNames) string {
	names := append(append([]string{}, known.Groups...), known.Events...)
	if details, ok := entry["split_candidate_details"].(map[string]any); ok {
		if group, ok := details["group_name"].(string); ok {
			names = append(names, group)
		}
	}
	if event, ok := entry["event_name"].(string); ok {
		names = append(names, event)
	}
	text := transcript
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			text = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(name)).ReplaceAllString(text, " ")
		}
	}
	return dailyGroupPhrase.ReplaceAllString(text, " ")
}

func tagsHave(entry map[string]any, want string) bool {
	tags, _ := entry["tags"].([]any)
	for _, value := range tags {
		if tag, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(tag), want) {
			return true
		}
	}
	return false
}

var accountQuestion = regexp.MustCompile(`(?i)\b(bank|account)\b`)

// dropUnaskedAccountQuestion stops a "which bank account?" on a payment that
// never involved naming one. "Paid by UPI" was flagged Check on Paid with and
// asked which bank account to use (2026-11-11): the app already picks the
// user's usual account for UPI, cash and wallets, so the question only made a
// correct field look wrong. A stated bank account with no bank named still
// asks, and so does one the user did name.
func dropUnaskedAccountQuestion(entry map[string]any, needsConfirmation map[string]any) {
	mode, _ := entry["mode"].(string)
	switch mode {
	case "UPI", "Cash", "Wallets":
	default:
		return
	}
	if hint, _ := entry["account_hint"].(string); strings.TrimSpace(hint) != "" {
		return
	}
	if flagged, _ := needsConfirmation["account_hint"].(bool); !flagged {
		return
	}
	delete(needsConfirmation, "account_hint")
	questions, _ := entry["clarifications"].([]any)
	kept := make([]any, 0, len(questions))
	for _, value := range questions {
		if text, ok := value.(string); ok && accountQuestion.MatchString(text) {
			continue
		}
		kept = append(kept, value)
	}
	entry["clarifications"] = kept
}
