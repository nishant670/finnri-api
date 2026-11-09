package http

import (
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestAccountCategoryTotalsKeepToTheAccountAndTheMonth(t *testing.T) {
	useSmokeDatabase(t)

	user := models.User{UUID: "account-categories", Username: "account-categories", IsGuest: true}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	card := models.Account{UserID: user.ID, Type: "credit_card", Name: "Card"}
	cash := models.Account{UserID: user.ID, Type: "cash", Name: "Cash"}
	if err := database.DB.Create(&[]*models.Account{&card, &cash}).Error; err != nil {
		t.Fatalf("failed to create accounts: %v", err)
	}
	entries := []models.Entry{
		dbDashboardEntry(user.ID, card.ID, "2026-10-03", "expense", "3928", "Transport", "Petrol", "Credit Card", "2026-10-03T09:00:00Z"),
		dbDashboardEntry(user.ID, card.ID, "2026-10-09", "expense", "500", "Food & Drinks", "Cafe", "Credit Card", "2026-10-09T09:00:00Z"),
		// Last month, for the comparison.
		dbDashboardEntry(user.ID, card.ID, "2026-09-12", "expense", "1000", "Transport", "Petrol", "Credit Card", "2026-09-12T09:00:00Z"),
		// Another account, and income: neither belongs in this card's spend.
		dbDashboardEntry(user.ID, cash.ID, "2026-10-04", "expense", "250", "Transport", "Auto", "Cash", "2026-10-04T09:00:00Z"),
		dbDashboardEntry(user.ID, card.ID, "2026-10-05", "income", "200", "Refund", "Shop", "Credit Card", "2026-10-05T09:00:00Z"),
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatalf("failed to create entries: %v", err)
	}

	october := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	current, err := accountCategoryTotals(user.ID, card.ID, october)
	if err != nil {
		t.Fatalf("totals failed: %v", err)
	}
	if current["Transport"] != 3928 || current["Food & Drinks"] != 500 || len(current) != 2 {
		t.Fatalf("october totals = %#v", current)
	}
	before, err := accountCategoryTotals(user.ID, card.ID, october.AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("totals failed: %v", err)
	}
	if before["Transport"] != 1000 || len(before) != 1 {
		t.Fatalf("september totals = %#v", before)
	}
}
