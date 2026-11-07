package http

import (
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestPaidCardBillClearsOnlyItsOwnReminders(t *testing.T) {
	useSmokeDatabase(t)

	user := models.User{UUID: "resolve-user", Username: "resolve-user", IsGuest: true}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	card := models.Account{UserID: user.ID, Type: "credit_card", Name: "Card"}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatalf("failed to create card: %v", err)
	}
	statements := []models.CardStatement{
		{UserID: user.ID, AccountID: card.ID, StatementDate: "2026-09-20", DueDate: "2026-10-10", TotalDue: 39900},
		{UserID: user.ID, AccountID: card.ID, StatementDate: "2026-10-20", DueDate: "2026-11-10", TotalDue: 50000},
	}
	if err := database.DB.Create(&statements).Error; err != nil {
		t.Fatalf("failed to create statements: %v", err)
	}
	notifications := []models.Notification{
		{UserID: user.ID, Type: "card_statement.overdue", Title: "Bill", Body: "Past due", ActionURL: "/accounts/1"},
		{UserID: user.ID, Type: "card_statement.due_soon", Title: "Bill", Body: "Due soon", ActionURL: "/accounts/1"},
	}
	if err := database.DB.Create(&notifications).Error; err != nil {
		t.Fatalf("failed to create notifications: %v", err)
	}
	reminders := []models.CardStatementReminder{
		{UserID: user.ID, StatementID: statements[0].ID, Kind: "overdue", NotificationID: &notifications[0].ID},
		{UserID: user.ID, StatementID: statements[1].ID, Kind: "due_soon", NotificationID: &notifications[1].ID},
	}
	if err := database.DB.Create(&reminders).Error; err != nil {
		t.Fatalf("failed to create reminders: %v", err)
	}

	resolveCardBillReminders(user.ID, statements[0].ID)

	var paid, open models.Notification
	database.DB.First(&paid, notifications[0].ID)
	database.DB.First(&open, notifications[1].ID)
	if paid.ResolvedAt == nil || paid.ReadAt == nil {
		t.Fatalf("the paid statement's reminder should be resolved and read: %#v", paid)
	}
	if open.ResolvedAt != nil {
		t.Fatalf("another statement's reminder must stay: %#v", open)
	}
}

func TestAccountIdentifierCountsAnyWayToTellItApart(t *testing.T) {
	if accountHasIdentifier(models.Account{}) {
		t.Fatal("an empty account has no identifier")
	}
	if !accountHasIdentifier(models.Account{UPIHandle: "me@okhdfc"}) || !accountHasIdentifier(models.Account{Last4: "1234"}) {
		t.Fatal("a UPI handle or last 4 digits tell an account apart")
	}
}
