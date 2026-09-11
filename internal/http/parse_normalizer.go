package http

import (
	"strings"
	"time"
)

const defaultParseMode = "Cash"

// defaultParseCategory is the canonical fallback; see internal/http/categories.go.
const defaultParseCategory = defaultCategory

var confirmableParseFields = []string{"title", "amount", "type", "category", "date"}

var allowedParseRootFields = map[string]bool{
	"stage": true, "type": true, "title": true, "amount": true, "currency": true,
	"mode": true, "card_network": true, "account_hint": true, "category": true,
	"merchant": true, "tag": true, "purpose_type": true, "tags": true, "note": true,
	"date": true, "time": true, "source_text": true, "recurring_candidate": true,
	"subscription_candidate": true, "split_candidate": true, "split_candidate_details": true,
	"refundable_amount": true, "refund_expected_on": true,
	"emi_tenure_months": true, "emi_rate_pct": true,
	"confidence": true, "needs_confirmation": true, "missing_fields": true,
	"clarifications": true,
}

var allowedConfidenceFields = map[string]bool{
	"amount": true, "mode": true, "category": true, "date": true, "merchant": true,
	"tag": true, "type": true, "account_hint": true,
}

var allowedNeedsConfirmationFields = map[string]bool{
	"title": true, "amount": true, "mode": true, "category": true, "date": true,
	"type": true, "merchant": true, "tag": true, "account_hint": true,
}

var allowedSubscriptionCandidateFields = map[string]bool{
	"name": true, "merchant": true, "category": true, "amount": true,
	"billing_interval": true, "next_due_date": true, "last_charged_date": true,
	"reminder_days": true, "cancel_before_due": true, "cancel_on_date": true,
	"autopay": true, "payment_mode": true, "notes": true, "missing_fields": true,
}

var allowedSplitCandidateFields = map[string]bool{
	"group_name": true, "participants": true, "missing_fields": true,
}

var allowedSplitParticipantFields = map[string]bool{
	"friend_name": true, "share_amount": true, "direction": true,
}

func normalizeParsedDraft(entry map[string]any, transcript string) {
	normalizeParseAliases(entry)
	sanitizeParsedDraftFields(entry)
	entry["stage"] = "draft"
	entry["source_text"] = transcript

	if currency, ok := entry["currency"].(string); !ok || strings.TrimSpace(currency) == "" {
		entry["currency"] = "INR"
	} else {
		entry["currency"] = strings.ToUpper(strings.TrimSpace(currency))
	}

	needsConfirmation, _ := entry["needs_confirmation"].(map[string]any)
	if needsConfirmation == nil {
		needsConfirmation = map[string]any{}
	}

	missingSet := map[string]bool{}
	if values, ok := entry["missing_fields"].([]any); ok {
		for _, value := range values {
			if field, ok := value.(string); ok && field != "" {
				missingSet[field] = true
			}
		}
	}

	hasTransactionSignal := parsedDraftHasTransactionSignal(entry)
	normalizeParseMode(entry, needsConfirmation, missingSet)
	normalizeCardNetwork(entry)
	normalizeType(entry)
	normalizeIncomeMode(entry)
	normalizePurposeAndTags(entry)
	normalizeInvestmentType(entry, needsConfirmation, missingSet)
	// Investment normalisation can change an AI-produced income into an
	// expense. Resolve the category afterwards so it is checked against the
	// final transaction type rather than the model's preliminary one.
	normalizeCategory(entry, needsConfirmation, missingSet, hasTransactionSignal)

	for _, field := range confirmableParseFields {
		if parseFieldMissing(field, entry[field]) {
			entry[field] = nil
			missingSet[field] = true
			needsConfirmation[field] = true
		}
	}

	missing := make([]string, 0, len(missingSet))
	for _, field := range confirmableParseFields {
		if missingSet[field] {
			missing = append(missing, field)
		}
	}
	entry["missing_fields"] = missing
	normalizeNeedsConfirmation(needsConfirmation)
	entry["needs_confirmation"] = needsConfirmation

	if _, ok := entry["confidence"].(map[string]any); !ok {
		entry["confidence"] = map[string]any{}
	}
	normalizeConfidence(entry["confidence"].(map[string]any))
	entry["clarifications"] = normalizeClarifications(entry["clarifications"])

	normalizeSubscriptionDraft(entry)
	normalizeSplitDraft(entry)
	pruneKeys(entry, allowedParseRootFields)
}

// A credit card can be the source of an expense, not the destination of
// ordinary income. Keep the receiving side usable even when the model latches
// onto a card word elsewhere in the sentence.
func normalizeIncomeMode(entry map[string]any) {
	entryType, _ := entry["type"].(string)
	mode, _ := entry["mode"].(string)
	if strings.EqualFold(entryType, "income") && strings.EqualFold(mode, "Credit Card") {
		entry["mode"] = "Bank Account"
		entry["card_network"] = nil
	}
}

// sanitizeParsedDraftFields coerces the loose shapes a model reaches for on the
// root fields — "10,000" for an amount, "12:33 AM" for a time, "yes" for a
// boolean — before anything downstream reads them.
//
// It runs first for a reason beyond tidiness: parsedDraftHasTransactionSignal
// asks whether an amount is a positive number, so an amount that arrived as a
// string used to make a real capture look like idle chatter.
func sanitizeParsedDraftFields(entry map[string]any) {
	for _, field := range []string{"title", "merchant", "note", "account_hint", "tag"} {
		normalizeOptionalString(entry, field)
	}
	normalizeOptionalAmount(entry, "amount")
	normalizeOptionalAmount(entry, "refundable_amount")
	normalizeOptionalEMITenure(entry)
	normalizeOptionalEMIRate(entry)
	normalizeOptionalDate(entry, "date")
	normalizeOptionalDate(entry, "refund_expected_on")
	normalizeParseTime(entry)
	normalizeOptionalBool(entry, "recurring_candidate")
	normalizeOptionalBool(entry, "split_candidate")
}

func normalizeOptionalEMITenure(entry map[string]any) {
	value, present := entry["emi_tenure_months"]
	if !present {
		return
	}
	number, ok := coerceParseNumber(value)
	if !ok || number < 1 || number > maxEMITenureMonths || number != float64(int(number)) {
		entry["emi_tenure_months"] = nil
		return
	}
	entry["emi_tenure_months"] = number
}

func normalizeOptionalEMIRate(entry map[string]any) {
	value, present := entry["emi_rate_pct"]
	if !present {
		return
	}
	number, ok := coerceParseNumber(value)
	if !ok || number < 0 || number > maxEMIAnnualRatePercent {
		entry["emi_rate_pct"] = nil
		return
	}
	entry["emi_rate_pct"] = number
}

func normalizeInvestmentType(entry map[string]any, needsConfirmation map[string]any, missingSet map[string]bool) {
	purpose, _ := entry["purpose_type"].(string)
	if purpose != "investment" && !hasNormalizedTag(entry, "Investment") {
		return
	}
	entry["type"] = "expense"
	entry["purpose_type"] = "investment"
	entry["tag"] = "Investment"
	appendTag(entry, "Investment")
	delete(missingSet, "type")
	delete(needsConfirmation, "type")
}

func hasNormalizedTag(entry map[string]any, wanted string) bool {
	if tag, ok := entry["tag"].(string); ok && strings.EqualFold(strings.TrimSpace(tag), wanted) {
		return true
	}
	if tags, ok := entry["tags"].([]any); ok {
		for _, tag := range tags {
			if value, ok := tag.(string); ok && strings.EqualFold(strings.TrimSpace(value), wanted) {
				return true
			}
		}
	}
	return false
}

func parsedDraftHasTransactionSignal(entry map[string]any) bool {
	if amount, ok := entry["amount"].(float64); ok && amount > 0 {
		return true
	}
	for _, field := range []string{"type", "merchant", "category", "account_hint"} {
		if value, ok := entry[field].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	if tags, ok := entry["tags"].([]any); ok {
		for _, tag := range tags {
			if value, ok := tag.(string); ok && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	if recurring, ok := entry["recurring_candidate"].(bool); ok && recurring {
		return true
	}
	if split, ok := entry["split_candidate"].(bool); ok && split {
		return true
	}
	return false
}

func parseFieldMissing(field string, value any) bool {
	if value == nil {
		return true
	}
	if field == "amount" {
		amount, ok := value.(float64)
		return !ok || amount <= 0
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return true
	}
	if field == "date" {
		_, err := time.Parse("2006-01-02", text)
		return err != nil
	}
	return false
}

func normalizeParseMode(entry map[string]any, needsConfirmation map[string]any, missingSet map[string]bool) {
	mode, ok := entry["mode"].(string)
	if hint, hintOK := entry["account_hint"].(string); hintOK && isBankAccountHint(hint) {
		entry["mode"] = "Bank Account"
		needsConfirmation["account_hint"] = true
	} else if !ok || strings.TrimSpace(mode) == "" {
		entry["mode"] = defaultParseMode
	} else if normalized, ok := canonicalParseMode(mode); ok {
		entry["mode"] = normalized
	} else {
		if strings.Contains(strings.ToLower(mode), "credit") && strings.Contains(strings.ToLower(mode), "card") {
			entry["mode"] = "Credit Card"
			if accountHint, ok := entry["account_hint"].(string); !ok || strings.TrimSpace(accountHint) == "" {
				entry["account_hint"] = strings.TrimSpace(mode)
			}
		} else {
			entry["mode"] = defaultParseMode
		}
	}
	delete(missingSet, "mode")
	delete(needsConfirmation, "mode")
}

func canonicalParseMode(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cash":
		return "Cash", true
	case "upi":
		return "UPI", true
	case "bank", "bank account", "savings account", "saving account":
		return "Bank Account", true
	case "credit card", "creditcard":
		return "Credit Card", true
	case "wallets", "wallet":
		return "Wallets", true
	default:
		return "", false
	}
}

func isBankAccountHint(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "bank") || strings.Contains(normalized, "saving") || strings.Contains(normalized, "salary account")
}

// normalizeClarifications keeps only the questions that can actually be shown.
// The review sheet renders these verbatim, and a stray number among them would
// fail the schema — costing the draft over a line of copy.
func normalizeClarifications(value any) []any {
	values, _ := value.([]any)
	questions := make([]any, 0, len(values))
	for _, item := range values {
		text, ok := item.(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			questions = append(questions, trimmed)
		}
	}
	return questions
}

func normalizeSubscriptionDraft(entry map[string]any) {
	candidate, ok := entry["subscription_candidate"].(map[string]any)
	if !ok || candidate == nil {
		return
	}
	pruneKeys(candidate, allowedSubscriptionCandidateFields)
	normalizeSubscriptionCandidate(candidate)
	interval, _ := candidate["billing_interval"].(string)
	if interval == subscriptionIntervalDaily || interval == subscriptionIntervalBusinessDaily {
		candidate["autopay"] = true
		candidate["reminder_days"] = float64(0)
		if parseFieldMissing("next_due_date", candidate["next_due_date"]) {
			if entryDate, ok := entry["date"].(string); ok {
				if parsed, err := parseAPIDate(entryDate); err == nil {
					candidate["next_due_date"] = addSubscriptionInterval(parsed, interval).Format("2006-01-02")
				}
			}
		}
	}
	if mode, ok := entry["mode"].(string); ok {
		candidate["payment_mode"] = mode
	}
	entry["recurring_candidate"] = true
	if tag, ok := entry["tag"].(string); !ok || strings.TrimSpace(tag) == "" {
		entry["tag"] = "Subscription"
	}
	if tags, ok := entry["tags"].([]any); ok {
		hasSubscriptionTag := false
		for _, tag := range tags {
			if value, ok := tag.(string); ok && strings.EqualFold(strings.TrimSpace(value), "Subscription") {
				hasSubscriptionTag = true
				break
			}
		}
		if !hasSubscriptionTag {
			entry["tags"] = append(tags, "Subscription")
		}
	} else {
		entry["tags"] = []any{"Subscription"}
	}

	candidateMissing, _ := candidate["missing_fields"].([]any)
	if len(candidateMissing) == 0 {
		missing := []any{}
		for _, field := range []string{"name", "amount", "billing_interval", "next_due_date"} {
			if parseFieldMissing(field, candidate[field]) {
				missing = append(missing, field)
			}
		}
		candidate["missing_fields"] = missing
	} else if !parseFieldMissing("next_due_date", candidate["next_due_date"]) {
		filtered := make([]any, 0, len(candidateMissing))
		for _, value := range candidateMissing {
			if value != "next_due_date" {
				filtered = append(filtered, value)
			}
		}
		candidate["missing_fields"] = filtered
	}
	if cancel, _ := candidate["cancel_before_due"].(bool); cancel && parseFieldMissing("cancel_on_date", candidate["cancel_on_date"]) {
		candidate["missing_fields"] = appendUniqueAnyString(candidate["missing_fields"], "cancel_on_date")
	}
}

func appendUniqueAnyString(value any, wanted string) []any {
	values, _ := value.([]any)
	for _, current := range values {
		if text, ok := current.(string); ok && text == wanted {
			return values
		}
	}
	return append(values, wanted)
}

// normalizeSplitDraft makes the split block safe to show and safe to validate.
//
// This is where the strictest part of the schema meets the least predictable
// part of the model. "Split it with the bubu-dudu group" gives it a group it
// cannot enumerate and shares it cannot compute, and what it does with that —
// a placeholder participant of nulls, a missing_fields entry it invented a name
// for — used to fail validation and take the whole ₹10,000 rent capture with
// it. None of that is worth a transaction: the group is named, the app knows
// its members, and the review sheet is where shares get settled anyway.
func normalizeSplitDraft(entry map[string]any) {
	candidate, ok := entry["split_candidate_details"].(map[string]any)
	if !ok || candidate == nil {
		// Split intent with no detail block still has to reach the sheet with
		// the split toggle on and a question attached, rather than as a plain
		// expense that silently forgot the second half of the sentence.
		if wantsSplit, _ := entry["split_candidate"].(bool); wantsSplit {
			entry["split_candidate_details"] = map[string]any{
				"group_name":     nil,
				"participants":   []any{},
				"missing_fields": []any{"friend_or_group"},
			}
		}
		return
	}
	pruneKeys(candidate, allowedSplitCandidateFields)
	normalizeOptionalString(candidate, "group_name")
	participants := normalizeSplitParticipants(candidate["participants"])
	candidate["participants"] = participants
	candidate["missing_fields"] = filterAliasedList(candidate["missing_fields"], splitMissingFieldAliases)

	groupName, _ := candidate["group_name"].(string)
	if len(participants) > 0 || groupName != "" {
		entry["split_candidate"] = true
	}
	wantsSplit, _ := entry["split_candidate"].(bool)
	if !wantsSplit {
		return
	}
	if groupName == "" && len(participants) == 0 {
		candidate["missing_fields"] = appendUniqueAnyString(candidate["missing_fields"], "friend_or_group")
		return
	}
	// A group or a name is on the draft, so the open question is no longer who
	// to split with — leaving it there would have the sheet ask about a group
	// the user already named.
	candidate["missing_fields"] = removeAnyString(candidate["missing_fields"], "friend_or_group")
}

func normalizeParseAliases(entry map[string]any) {
	if _, ok := entry["note"]; !ok {
		if notes, ok := entry["notes"]; ok {
			entry["note"] = notes
		}
	}
	if _, ok := entry["mode"]; !ok {
		if mode, ok := entry["payment_mode"]; ok {
			entry["mode"] = mode
		}
	}
	if _, ok := entry["account_hint"]; !ok {
		for _, field := range []string{"account", "payment_account", "issuer", "bank"} {
			if hint, ok := entry[field].(string); ok && strings.TrimSpace(hint) != "" {
				entry["account_hint"] = strings.TrimSpace(hint)
				break
			}
		}
	}
}

func normalizeCardNetwork(entry map[string]any) {
	raw, ok := entry["card_network"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		entry["card_network"] = nil
		return
	}
	if normalized, ok := canonicalCardNetwork(raw); ok {
		entry["card_network"] = normalized
		return
	}
	if accountHint, ok := entry["account_hint"].(string); !ok || strings.TrimSpace(accountHint) == "" {
		entry["account_hint"] = strings.TrimSpace(raw)
	}
	entry["card_network"] = nil
}

func canonicalCardNetwork(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "visa":
		return "Visa", true
	case "mastercard", "master card":
		return "Mastercard", true
	case "amex", "american express":
		return "Amex", true
	case "rupay", "ru pay":
		return "Rupay", true
	default:
		return "", false
	}
}

func normalizeType(entry map[string]any) {
	raw, ok := entry["type"].(string)
	if !ok {
		return
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "expense":
		entry["type"] = "expense"
	case "income":
		entry["type"] = "income"
	default:
		entry["type"] = nil
	}
}

func normalizeCategory(
	entry map[string]any,
	needsConfirmation map[string]any,
	missingSet map[string]bool,
	hasTransactionSignal bool,
) {
	entryType, _ := entry["type"].(string)
	fallback := defaultParseCategory
	if strings.EqualFold(entryType, "income") {
		fallback = defaultIncomeCategory
	}
	raw, ok := entry["category"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		if !hasTransactionSignal {
			return
		}
		entry["category"] = fallback
		missingSet["category"] = true
		needsConfirmation["category"] = true
		return
	}
	if normalized, ok := canonicalCategoryForType(raw, entryType); ok {
		entry["category"] = normalized
		return
	}
	entry["category"] = fallback
	missingSet["category"] = true
	needsConfirmation["category"] = true
}

func normalizePurposeAndTags(entry map[string]any) {
	normalizeTags(entry)
	normalizePrimaryTag(entry)
	raw, ok := entry["purpose_type"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		entry["purpose_type"] = nil
		return
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "normal_spend":
		entry["purpose_type"] = "normal_spend"
	case "investment":
		entry["purpose_type"] = "investment"
	case "lending":
		entry["purpose_type"] = "lending"
	case "refund":
		entry["purpose_type"] = "refund"
	case "reimbursable":
		entry["purpose_type"] = "reimbursable"
		appendTag(entry, "Refundable")
		setPrimaryTagIfEmpty(entry, "Refundable")
	case "donation":
		entry["purpose_type"] = "donation"
	// Settling a card bill, not buying anything. Kept out of spending and
	// income totals by notCardPaymentClause in insights.go.
	case "card_payment", "credit_card_payment", "bill_payment":
		entry["purpose_type"] = cardPaymentPurposeType
	case "emi", "no_cost_emi", "no-cost emi", "installment", "instalment":
		entry["purpose_type"] = "normal_spend"
		appendTag(entry, "EMI")
		setPrimaryTagIfEmpty(entry, "EMI")
	default:
		entry["purpose_type"] = nil
	}
}

func normalizePrimaryTag(entry map[string]any) {
	if tag, ok := entry["tag"].(string); ok && strings.TrimSpace(tag) != "" {
		if normalized, ok := canonicalParseTag(tag); ok {
			entry["tag"] = normalized
		} else {
			entry["tag"] = strings.TrimSpace(tag)
		}
		return
	}
	if tags, ok := entry["tags"].([]any); ok {
		for _, value := range tags {
			tag, ok := value.(string)
			if !ok {
				continue
			}
			if normalized, ok := canonicalParseTag(tag); ok {
				entry["tag"] = normalized
				return
			}
		}
	}
	entry["tag"] = nil
}

func canonicalParseTag(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "investment":
		return "Investment", true
	case "lending":
		return "Lending", true
	case "emi":
		return "EMI", true
	case "subscription":
		return "Subscription", true
	case "refundable", "reimbursable":
		return "Refundable", true
	case "general":
		return "General", true
	default:
		return "", false
	}
}

func normalizeTags(entry map[string]any) {
	values, ok := entry["tags"].([]any)
	if !ok {
		if tag, ok := entry["tags"].(string); ok && strings.TrimSpace(tag) != "" {
			entry["tags"] = []any{strings.TrimSpace(tag)}
		} else {
			entry["tags"] = []any{}
		}
		return
	}
	tags := make([]any, 0, len(values))
	for _, value := range values {
		if tag, ok := value.(string); ok && strings.TrimSpace(tag) != "" {
			tags = append(tags, strings.TrimSpace(tag))
		}
	}
	entry["tags"] = tags
}

func setPrimaryTagIfEmpty(entry map[string]any, tag string) {
	if current, ok := entry["tag"].(string); ok && strings.TrimSpace(current) != "" {
		return
	}
	entry["tag"] = tag
}

func appendTag(entry map[string]any, tag string) {
	tags, _ := entry["tags"].([]any)
	for _, value := range tags {
		if existing, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(existing), tag) {
			entry["tags"] = tags
			return
		}
	}
	entry["tags"] = append(tags, tag)
}

func normalizeConfidence(confidence map[string]any) {
	for field, value := range confidence {
		score, ok := value.(float64)
		if !allowedConfidenceFields[field] || !ok || score < 0 || score > 1 {
			delete(confidence, field)
		}
	}
}

func normalizeNeedsConfirmation(needsConfirmation map[string]any) {
	for field, value := range needsConfirmation {
		_, ok := value.(bool)
		if !allowedNeedsConfirmationFields[field] || !ok {
			delete(needsConfirmation, field)
		}
	}
}

func pruneKeys(entry map[string]any, allowed map[string]bool) {
	for field := range entry {
		if !allowed[field] {
			delete(entry, field)
		}
	}
}
