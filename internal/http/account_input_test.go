package http

import (
	"encoding/json"
	"testing"

	"finnri/internal/models"
)

func TestAccountInputValidation(t *testing.T) {
	tests := []struct {
		name  string
		input accountInput
		valid bool
	}{
		{"valid", accountInput{Name: "Cash", Type: "cash"}, true},
		{"missing name", accountInput{Type: "cash"}, false},
		{"invalid type", accountInput{Name: "Broker", Type: "investment"}, false},
		{"canonical credit card", accountInput{Name: "Card", Type: "credit_card"}, true},
		{"legacy credit alias", accountInput{Name: "Card", Type: "credit"}, true},
		{"legacy wallets alias", accountInput{Name: "Wallet", Type: "wallets"}, true},
		{"invalid due day", accountInput{Name: "Card", Type: "credit_card", DueDay: 32}, false},
		{"negative limit", accountInput{Name: "Card", Type: "credit_card", CreditLimit: -1}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := len(test.input.validate()) == 0; got != test.valid {
				t.Fatalf("valid = %v, fields = %v", got, test.input.validate())
			}
		})
	}
}

func TestAccountInputDoesNotOverwriteOwnership(t *testing.T) {
	account := models.Account{ID: 7, UserID: 9}
	(accountInput{Name: " Daily Cash ", Type: "CASH"}).apply(&account)
	if account.ID != 7 || account.UserID != 9 {
		t.Fatalf("ownership changed: %#v", account)
	}
	if account.Name != "Daily Cash" || account.Type != "cash" {
		t.Fatalf("input was not normalized: %#v", account)
	}
}

func TestAccountInputCanonicalizesLegacyTypeAliases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"credit", "credit_card"},
		{" CREDIT_CARD ", "credit_card"},
		{"debit", "debit_card"},
		{"wallets", "wallet"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			account := models.Account{}
			(accountInput{Name: "Alias", Type: test.input}).apply(&account)
			if account.Type != test.want {
				t.Fatalf("Type = %q, want %q", account.Type, test.want)
			}
		})
	}
}

func TestAccountInputUsesFixedPointMoney(t *testing.T) {
	var input accountInput
	if err := json.Unmarshal([]byte(`{
		"name":"Card",
		"type":"credit_card",
		"credit_limit":12345.67,
		"balance":"100.50"
	}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.CreditLimit.String() != "12345.67" || input.Balance.String() != "100.50" {
		t.Fatalf("money fields were not fixed-point: %#v", input)
	}

	if err := json.Unmarshal([]byte(`{
		"name":"Card",
		"type":"credit_card",
		"credit_limit":1.234
	}`), &input); err == nil {
		t.Fatal("expected excess precision to fail")
	}
}

// The app sends the whole account back on every edit. Card statement settings
// must survive an edit that says nothing about them — StatementDay is inferred
// once from the user's first bill, and losing it takes the card's billing
// cycle with it.
func TestAccountUpdateLeavesStatementSettingsAloneWhenOmitted(t *testing.T) {
	account := models.Account{
		Type: "credit_card", Name: "HDFC Regalia",
		StatementDay: 5, ReminderDaysBefore: 7, AutopayEnabled: true,
	}

	input := accountInput{Type: "credit_card", Name: "HDFC Regalia Gold"}
	input.apply(&account)

	if account.StatementDay != 5 {
		t.Errorf("statement day = %d, want 5", account.StatementDay)
	}
	if account.ReminderDaysBefore != 7 {
		t.Errorf("reminder days = %d, want 7", account.ReminderDaysBefore)
	}
	if !account.AutopayEnabled {
		t.Error("autopay was switched off by an unrelated edit")
	}
}

func TestAccountUpdateAppliesStatementSettingsWhenGiven(t *testing.T) {
	account := models.Account{Type: "credit_card", Name: "HDFC Regalia", StatementDay: 5}

	statementDay := 18
	reminderDays := 2
	autopay := true
	input := accountInput{
		Type: "credit_card", Name: "HDFC Regalia",
		StatementDay: &statementDay, ReminderDaysBefore: &reminderDays, AutopayEnabled: &autopay,
	}
	input.apply(&account)

	if account.StatementDay != 18 || account.ReminderDaysBefore != 2 || !account.AutopayEnabled {
		t.Fatalf("settings not applied: %+v", account)
	}
}

func TestAccountStatementSettingsAreValidated(t *testing.T) {
	badDay := 32
	badReminder := 45
	fields := accountInput{
		Type: "credit_card", Name: "Card",
		StatementDay: &badDay, ReminderDaysBefore: &badReminder,
	}.validate()

	if fields["statement_day"] == "" {
		t.Error("statement day 32 was accepted")
	}
	if fields["reminder_days_before"] == "" {
		t.Error("reminder lead of 45 days was accepted")
	}
}
