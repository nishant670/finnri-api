package http

import (
	"strings"
	"time"

	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// resolveNotifications marks the notifications a query selects as done (P49):
// resolved now, and read if they were not. The app clears resolved items from
// "Needs you", so a bill marked paid or a settle-up answered stops asking.
// Best effort: a failure here never fails the action that did the job.
func resolveNotifications(query *gorm.DB) {
	now := time.Now()
	_ = query.Model(&models.Notification{}).
		Where("resolved_at IS NULL").
		Updates(map[string]any{
			"resolved_at": now,
			"read_at":     gorm.Expr("COALESCE(read_at, ?)", now),
		}).Error
}

// moneyPointer and datePointer fill a reminder's amount and date (P48).
func moneyPointer(value models.Money) *models.Money {
	if value <= 0 {
		return nil
	}
	return &value
}

func datePointer(value string) *string {
	if len(value) < 10 {
		return nil
	}
	day := value[:10]
	return &day
}

// accountHasIdentifier says whether an account can be told apart from others
// of its kind: last 4 digits, a UPI handle, a wallet nickname or an identifier.
func accountHasIdentifier(account models.Account) bool {
	for _, value := range []string{account.Last4, account.UPIHandle, account.WalletNickname, account.Identifier} {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// cardBillNotificationTypes are the reminders a paid card bill answers.
var cardBillNotificationTypes = []string{
	"card_statement.due_soon",
	"card_statement.due_today",
	"card_statement.overdue",
}

// resolveCardBillReminders clears the due and overdue reminders of a
// statement once it is paid (P49).
func resolveCardBillReminders(userID, statementID uint) {
	resolveCardBillRemindersIn(database.DB, userID, statementID)
}

// resolveCardBillRemindersIn is resolveCardBillReminders on a given connection,
// for a caller inside a transaction.
func resolveCardBillRemindersIn(db *gorm.DB, userID, statementID uint) {
	resolveNotifications(db.
		Where("user_id = ? AND type IN ?", userID, cardBillNotificationTypes).
		Where("id IN (?)", db.Model(&models.CardStatementReminder{}).
			Select("notification_id").
			Where("user_id = ? AND statement_id = ? AND notification_id IS NOT NULL", userID, statementID)))
}
