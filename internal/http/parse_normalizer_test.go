package http

import (
	"reflect"
	"testing"
)

func TestNormalizeParsedDraftFlagsMissingCategoryWithFallback(t *testing.T) {
	entry := map[string]any{
		"type":     "expense",
		"title":    "Lunch",
		"amount":   float64(250),
		"mode":     "UPI",
		"category": nil,
		"date":     "not-a-date",
	}

	normalizeParsedDraft(entry, "paid 250 for lunch")

	if entry["date"] != nil {
		t.Fatalf("normalizer guessed missing date: %#v", entry)
	}
	if entry["category"] != defaultParseCategory {
		t.Fatalf("category = %#v, want fallback %s", entry["category"], defaultParseCategory)
	}
	wantMissing := []string{"category", "date"}
	if !reflect.DeepEqual(entry["missing_fields"], wantMissing) {
		t.Fatalf("missing_fields = %#v, want %#v", entry["missing_fields"], wantMissing)
	}
	if entry["source_text"] != "paid 250 for lunch" {
		t.Fatal("source_text must be set from the original transcript")
	}
}

func TestNormalizeParsedDraftMapsLodgingToTravel(t *testing.T) {
	entry := map[string]any{
		"type": "expense", "title": "Airbnb", "amount": float64(12000),
		"mode": "Credit Card", "category": "Airbnb", "date": "2026-07-20",
	}

	normalizeParsedDraft(entry, "paid 12000 for Airbnb trip")

	if entry["category"] != "Travel" {
		t.Fatalf("category = %#v, want Travel", entry["category"])
	}
	if missing := entry["missing_fields"].([]string); len(missing) != 0 {
		t.Fatalf("unexpected missing fields: %v", missing)
	}
}

func TestNormalizeParsedDraftTreatsInvestmentAsExpense(t *testing.T) {
	entry := map[string]any{
		"type": "income", "title": "Daily Investment", "amount": float64(100),
		"mode": "Cash", "category": "Misc", "date": "2026-08-01",
		"purpose_type": "investment", "tag": "Subscription", "tags": []any{"Subscription"},
	}

	normalizeParsedDraft(entry, "I invest 100 rupees daily in a small cap fund")

	if entry["type"] != "expense" {
		t.Fatalf("investment type = %#v, want expense", entry["type"])
	}
	if entry["tag"] != "Investment" || entry["purpose_type"] != "investment" {
		t.Fatalf("investment metadata was not preserved: %#v", entry)
	}
	if missing := entry["missing_fields"].([]string); len(missing) != 0 {
		t.Fatalf("unexpected missing fields: %v", missing)
	}
}

func TestNormalizeParsedDraftPreservesEMIInputs(t *testing.T) {
	entry := map[string]any{
		"type": "expense", "title": "Phone", "amount": float64(67518),
		"mode": "Credit Card", "category": "Shopping", "date": "2026-09-11",
		"purpose_type": "emi", "emi_tenure_months": "6", "emi_rate_pct": "0",
	}
	normalizeParsedDraft(entry, "67518 turned into six month no-cost EMI")

	if entry["tag"] != "EMI" || entry["emi_tenure_months"] != float64(6) || entry["emi_rate_pct"] != float64(0) {
		t.Fatalf("EMI inputs were not preserved: %#v", entry)
	}
}

func TestNormalizeParsedDraftPreservesRefundableDetails(t *testing.T) {
	entry := map[string]any{
		"type": "expense", "title": "Deposit", "amount": float64(10000),
		"mode": "UPI", "category": "Misc", "date": "2026-09-11",
		"purpose_type": "reimbursable", "refundable_amount": "5,000",
		"refund_expected_on": "11/12/2026",
	}
	normalizeParsedDraft(entry, "paid 10000 and 5000 is refundable after three months")

	if entry["tag"] != "Refundable" || entry["refundable_amount"] != float64(5000) || entry["refund_expected_on"] != "2026-12-11" {
		t.Fatalf("refundable details were not preserved: %#v", entry)
	}
}

func TestNormalizeParsedDraftDoesNotTurnExplicitBankAccountIntoCash(t *testing.T) {
	entry := map[string]any{
		"type": "expense", "title": "SIP", "amount": float64(100), "mode": "Cash",
		"account_hint": "my savings bank account", "category": "Investment", "date": "2026-08-01",
	}
	normalizeParsedDraft(entry, "100 rupees deducts daily from my savings bank account")
	if entry["mode"] != "Bank Account" {
		t.Fatalf("mode = %#v, want Bank Account", entry["mode"])
	}
	needs, _ := entry["needs_confirmation"].(map[string]any)
	if needs["account_hint"] != true {
		t.Fatalf("bank account should remain a field to check: %#v", entry)
	}
}

func TestNormalizeParsedDraftUsesBankAccountForReceivedIncome(t *testing.T) {
	entry := map[string]any{
		"type": "income", "title": "Salary", "amount": float64(90000),
		"mode": "Credit Card", "card_network": "Visa", "account_hint": "SBI account",
		"category": "Salary", "date": "2026-09-01",
	}

	normalizeParsedDraft(entry, "received 90000 salary in my SBI account")
	if entry["mode"] != "Bank Account" {
		t.Fatalf("mode = %#v, want Bank Account", entry["mode"])
	}
	if entry["card_network"] != nil {
		t.Fatalf("card network leaked onto income: %#v", entry["card_network"])
	}
}

func TestNormalizeDailySubscriptionInfersNextDate(t *testing.T) {
	entry := map[string]any{
		"type": "expense", "title": "SIP", "amount": float64(100), "mode": "UPI",
		"category": "Investment", "date": "2026-08-03", "tag": "Investment",
		"subscription_candidate": map[string]any{
			"name": "Small cap SIP", "amount": float64(100), "billing_interval": "business_daily",
			"next_due_date": nil, "missing_fields": []any{"next_due_date"},
		},
	}
	normalizeParsedDraft(entry, "invest 100 daily on market days")
	candidate := entry["subscription_candidate"].(map[string]any)
	if candidate["next_due_date"] != "2026-08-04" || candidate["autopay"] != true {
		t.Fatalf("daily schedule was not inferred: %#v", candidate)
	}
	if missing := candidate["missing_fields"].([]any); len(missing) != 0 {
		t.Fatalf("next date should not need confirmation: %#v", missing)
	}
}

func TestNormalizeParsedDraftPreservesValidValues(t *testing.T) {
	entry := map[string]any{
		"title": "Metro", "amount": float64(45), "type": "expense",
		"mode": "UPI", "category": "Travel", "date": "2026-07-09",
	}
	normalizeParsedDraft(entry, "metro 45 via upi today")

	if missing := entry["missing_fields"].([]string); len(missing) != 0 {
		t.Fatalf("unexpected missing fields: %v", missing)
	}
	if entry["currency"] != "INR" || entry["stage"] != "draft" {
		t.Fatalf("normalization defaults missing: %#v", entry)
	}
}

func TestNormalizeParsedDraftDefaultsMissingPaymentMode(t *testing.T) {
	entry := map[string]any{
		"title": "Uber", "amount": float64(500), "type": "expense",
		"category": "Travel", "date": "2026-07-17",
		"missing_fields":     []any{"mode"},
		"needs_confirmation": map[string]any{"mode": true},
	}

	normalizeParsedDraft(entry, "Today I paid 500 rupees for the Uber cab for Ria")

	if entry["mode"] != defaultParseMode {
		t.Fatalf("mode = %#v, want %s", entry["mode"], defaultParseMode)
	}
	if missing := entry["missing_fields"].([]string); len(missing) != 0 {
		t.Fatalf("mode should not be a missing field: %v", missing)
	}
	if _, ok := entry["needs_confirmation"].(map[string]any)["mode"]; ok {
		t.Fatalf("mode should not need confirmation: %#v", entry["needs_confirmation"])
	}
}

func TestNormalizeParsedDraftDefaultsInvalidPaymentMode(t *testing.T) {
	entry := map[string]any{
		"title": "Uber", "amount": float64(500), "type": "expense",
		"mode": "unknown", "category": "Travel", "date": "2026-07-17",
	}

	normalizeParsedDraft(entry, "Today I paid 500 rupees for the Uber cab")

	if entry["mode"] != defaultParseMode {
		t.Fatalf("mode = %#v, want %s", entry["mode"], defaultParseMode)
	}
}

func TestNormalizeIndianPaymentFixtures(t *testing.T) {
	fixtures := []struct {
		name  string
		entry map[string]any
	}{
		{"upi", map[string]any{"title": "Chai", "amount": float64(80), "type": "expense", "mode": "UPI", "category": "Food", "date": "2026-07-09"}},
		{"rupay card", map[string]any{"title": "Fuel", "amount": float64(2500), "type": "expense", "mode": "Credit Card", "card_network": "Rupay", "category": "Travel", "date": "2026-07-08"}},
		{"cash", map[string]any{"title": "Auto", "amount": float64(120), "type": "expense", "mode": "Cash", "category": "Travel", "date": "2026-07-09"}},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			normalizeParsedDraft(fixture.entry, fixture.name)
			if missing := fixture.entry["missing_fields"].([]string); len(missing) != 0 {
				t.Fatalf("unexpected missing fields: %v", missing)
			}
		})
	}
}

func TestNormalizeSubscriptionCandidateLabelsDraft(t *testing.T) {
	entry := map[string]any{
		"title":    "Sub",
		"amount":   float64(1500),
		"type":     "expense",
		"mode":     "Cash",
		"category": "Bills",
		"date":     "2026-07-17",
		"subscription_candidate": map[string]any{
			"name":             nil,
			"amount":           float64(1500),
			"billing_interval": nil,
			"next_due_date":    nil,
		},
	}

	normalizeParsedDraft(entry, "paid 1500 rupees for my subscription by cash")

	if entry["recurring_candidate"] != true || entry["tag"] != "Subscription" {
		t.Fatalf("subscription label was not normalized: %#v", entry)
	}
	candidate := entry["subscription_candidate"].(map[string]any)
	missing := candidate["missing_fields"].([]any)
	if !reflect.DeepEqual(missing, []any{"name", "billing_interval", "next_due_date"}) {
		t.Fatalf("subscription missing_fields = %#v", missing)
	}
}

func TestNormalizeSplitCandidateDetailsSetsLegacyFlag(t *testing.T) {
	entry := map[string]any{
		"title":    "Dinner",
		"amount":   float64(2500),
		"type":     "expense",
		"mode":     "UPI",
		"category": "Food",
		"date":     "2026-07-17",
		"split_candidate_details": map[string]any{
			"participants": []any{
				map[string]any{
					"friend_name":  "Ria",
					"share_amount": float64(1250),
					"direction":    "friend_owes_user",
				},
			},
			"missing_fields": []any{},
		},
	}

	normalizeParsedDraft(entry, "paid 2500 rupees for dinner split with Ria")

	if entry["split_candidate"] != true {
		t.Fatalf("split candidate flag was not normalized: %#v", entry)
	}
}
