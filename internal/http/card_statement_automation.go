package http

import (
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Statement reminders, and the draft a cycle opens with.

Two jobs, one ticker:

 1. On a card's statement day, open the next cycle's statement as a draft, so
    the reminder has somewhere to point and the card screen has a slot to fill.
 2. Nudge the user along the life of a bill — the amount is probably out, the
    bill is not fully itemised, it is due soon, it is due today, it is late.

Nothing here ever records a payment. An autopay that silently failed but shows
as paid in Finnri costs the user a late fee and interest while telling them
they are fine, so autopay only changes the wording of the question — the
answer still comes from the user. This is deliberately unlike
subscription_automation.go, which does create occurrences, because a
subscription charge that fails is a missed film, and a card payment that fails
is a credit score.
*/

const (
	statementReminderExpected = "statement_expected"
	statementReminderItemize  = "itemize_pending"
	statementReminderDueSoon  = "due_soon"
	statementReminderDueToday = "due_today"
	statementReminderOverdue  = "overdue"
)

// itemizeReminderDelayDays is how long after a bill lands before nagging about
// its missing detail. Immediately would collide with the "add the amount"
// prompt the user has just answered.
const itemizeReminderDelayDays = 2

// StartCardStatementAutomation runs the draft-opening and reminder sweeps on
// the same one-minute cadence as the subscription job.
func StartCardStatementAutomation(_ *config.Config) {
	go func() {
		run := func() {
			if err := syncAllCardStatements(time.Now()); err != nil {
				log.Printf("card statement automation failed: %v", err)
			}
		}
		run()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			run()
		}
	}()
}

func syncAllCardStatements(now time.Time) error {
	var userIDs []uint
	if err := database.DB.Model(&models.Account{}).
		Where("LOWER(type) IN ?", []string{"credit_card", "credit"}).
		Distinct("user_id").Pluck("user_id", &userIDs).Error; err != nil {
		return err
	}
	for _, userID := range userIDs {
		// Instalments first: billing one writes a card expense inside the
		// closing cycle, and the reminders that follow read a statement's
		// reconciliation. Doing it the other way round would nag about an
		// unitemised gap that this sweep was about to close.
		if _, err := syncCardEMIInstallments(userID, now); err != nil {
			return err
		}
		if _, err := openDueCardStatementDrafts(userID, now); err != nil {
			return err
		}
		if _, err := syncCardStatementReminders(userID, now); err != nil {
			return err
		}
	}
	return nil
}

// openDueCardStatementDrafts creates the placeholder for a cycle that has just
// closed. Cards with no statement day set are skipped: without one there is no
// way to know when a cycle ends, and inventing a date would be worse than
// waiting for the user's first bill to tell us.
func openDueCardStatementDrafts(userID uint, now time.Time) ([]models.CardStatement, error) {
	today := truncateDate(now)

	var cards []models.Account
	if err := database.DB.
		Where("user_id = ? AND LOWER(type) IN ? AND statement_day BETWEEN 1 AND 31",
			userID, []string{"credit_card", "credit"}).
		Find(&cards).Error; err != nil {
		return nil, err
	}

	created := []models.CardStatement{}
	for _, card := range cards {
		statementDate := clampDayToMonth(today.Year(), today.Month(), card.StatementDay)
		// The cycle has not closed yet this month; last month's is the one
		// that should already exist.
		if statementDate.After(today) {
			statementDate = clampDayToMonth(today.Year(), today.Month()-1, card.StatementDay)
		}

		statement, didCreate, err := createDraftStatement(card, statementDate)
		if err != nil {
			return created, err
		}
		if didCreate {
			created = append(created, statement)
		}
	}
	return created, nil
}

// createDraftStatement inserts a placeholder unless one already exists for
// that date. The unique index on (user_id, account_id, statement_date) is the
// real guarantee; the count is just an early exit.
func createDraftStatement(card models.Account, statementDate time.Time) (models.CardStatement, bool, error) {
	formatted := statementDate.Format(apiDateLayout)

	var existing models.CardStatement
	err := database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date = ?", card.UserID, card.ID, formatted).
		First(&existing).Error
	if err == nil {
		return existing, false, nil
	}

	cycleStart, cycleEnd := statementCycle(statementDate, card.StatementDay)
	statement := models.CardStatement{
		UserID:        card.UserID,
		AccountID:     card.ID,
		CycleStart:    cycleStart.Format(apiDateLayout),
		CycleEnd:      cycleEnd.Format(apiDateLayout),
		StatementDate: formatted,
		DueDate:       dueDateFor(statementDate, effectiveDueDay(card, statementDate)).Format(apiDateLayout),
		Currency:      "INR",
		Status:        statementStatusDraft,
		Source:        "manual",
	}
	if err := database.DB.Create(&statement).Error; err != nil {
		// Another tick won the race. Not an error.
		return statement, false, nil
	}
	return statement, true, nil
}

// syncCardStatementReminders emits at most one reminder of each kind per
// statement, ever.
func syncCardStatementReminders(userID uint, now time.Time) (int, error) {
	today := truncateDate(now)
	todayString := today.Format(apiDateLayout)

	var statements []models.CardStatement
	if err := database.DB.
		Where("user_id = ? AND status <> ?", userID, statementStatusPaid).
		Order("due_date ASC").
		Find(&statements).Error; err != nil {
		return 0, err
	}

	cards, err := loadCardsByID(userID)
	if err != nil {
		return 0, err
	}

	created := 0
	for index := range statements {
		statement := statements[index]
		card, ok := cards[statement.AccountID]
		if !ok || !card.ReminderEnabled {
			continue
		}

		for _, kind := range dueCardStatementReminderKinds(statement, card, todayString, today) {
			didCreate, err := createCardStatementReminderIfNeeded(database.DB, statement, card, kind)
			if err != nil {
				return created, err
			}
			if didCreate {
				created++
			}
		}
	}
	return created, nil
}

// dueCardStatementReminderKinds decides what, if anything, to say about one
// statement today. More than one kind can be due at once — a bill can go
// unpriced and then land its due date — and each is emitted once.
func dueCardStatementReminderKinds(statement models.CardStatement, card models.Account, todayString string, today time.Time) []string {
	kinds := []string{}

	statementDate, err := parseAPIDate(statement.StatementDate)
	if err != nil {
		return kinds
	}

	// A draft is a bill we expect but do not have. Ask for the amount.
	if statement.Status == statementStatusDraft {
		if !today.Before(statementDate) {
			kinds = append(kinds, statementReminderExpected)
		}
		// Everything below is about an amount this statement does not have.
		return kinds
	}

	if statement.UnitemizedEntryID != nil &&
		!today.Before(statementDate.AddDate(0, 0, itemizeReminderDelayDays)) {
		kinds = append(kinds, statementReminderItemize)
	}

	dueDate, err := parseAPIDate(statement.DueDate)
	if err != nil {
		return kinds
	}
	daysUntilDue := int(truncateDate(dueDate).Sub(today).Hours() / 24)

	switch {
	case statement.DueDate < todayString:
		kinds = append(kinds, statementReminderOverdue)
	case daysUntilDue == 0:
		kinds = append(kinds, statementReminderDueToday)
	case daysUntilDue > 0 && daysUntilDue <= reminderLeadDays(card):
		kinds = append(kinds, statementReminderDueSoon)
	}

	return kinds
}

// reminderLeadDays is how far ahead of the due date to warn, defaulting to
// three days for cards added before the field existed.
func reminderLeadDays(card models.Account) int {
	if card.ReminderDaysBefore > 0 {
		return card.ReminderDaysBefore
	}
	return 3
}

func loadCardsByID(userID uint) (map[uint]models.Account, error) {
	var cards []models.Account
	if err := database.DB.
		Where("user_id = ? AND LOWER(type) IN ?", userID, []string{"credit_card", "credit"}).
		Find(&cards).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint]models.Account, len(cards))
	for _, card := range cards {
		byID[card.ID] = card
	}
	return byID, nil
}

// createCardStatementReminderIfNeeded writes the notification and the reminder
// row together. The unique index on (user_id, statement_id, kind) is what
// makes this safe to call on every tick.
func createCardStatementReminderIfNeeded(db *gorm.DB, statement models.CardStatement, card models.Account, kind string) (bool, error) {
	created := false
	actionURL := fmt.Sprintf("/accounts/%d", card.ID)

	err := db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.CardStatementReminder{}).
			Where("user_id = ? AND statement_id = ? AND kind = ?", statement.UserID, statement.ID, kind).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}

		title, body := cardStatementReminderCopy(statement, card, kind)
		notification := models.Notification{
			UserID: statement.UserID, Type: "card_statement." + kind,
			Title: title, Body: body, ActionURL: actionURL,
		}
		if err := tx.Create(&notification).Error; err != nil {
			return err
		}
		reminder := models.CardStatementReminder{
			UserID: statement.UserID, StatementID: statement.ID,
			Kind: kind, NotificationID: &notification.ID,
		}
		if err := tx.Create(&reminder).Error; err != nil {
			return err
		}
		created = true
		return nil
	})

	if err == nil && created {
		title, body := cardStatementReminderCopy(statement, card, kind)
		go sendUserPush(database.DB, statement.UserID, title, body, map[string]any{
			"action_url": actionURL, "statement_id": statement.ID,
		})
	}
	return created, err
}

// cardStatementReminderCopy writes the nudge.
//
// Nothing here offers to pay. Finnri does not move money, and a button that
// implies otherwise would be a lie the moment someone tapped it — so the verbs
// are "add", "confirm" and "record", never "pay now".
func cardStatementReminderCopy(statement models.CardStatement, card models.Account, kind string) (string, string) {
	name := strings.TrimSpace(card.Name)
	if name == "" {
		name = "your credit card"
	}
	remaining := remainingDue(statement)

	switch kind {
	case statementReminderExpected:
		return "Statement expected",
			fmt.Sprintf("Your %s statement should be out. Add the bill amount to track it.", name)

	case statementReminderItemize:
		return "Bill not fully itemised",
			fmt.Sprintf("Part of your %s bill isn't itemised in Finnri yet, so your category breakdown is incomplete.", name)

	case statementReminderDueToday:
		// Autopay changes the question, not the answer: Finnri still asks
		// rather than assuming the debit went through.
		if card.AutopayEnabled {
			return "Autopay due today",
				fmt.Sprintf("Your %s autopay should take ₹%s today. Did it go through?", name, remaining.String())
		}
		return "Bill due today",
			fmt.Sprintf("₹%s is due on your %s today.", remaining.String(), name)

	case statementReminderDueSoon:
		days := daysBetween(truncateDate(time.Now()).Format(apiDateLayout), statement.DueDate)
		unit := "days"
		if days == 1 {
			unit = "day"
		}
		return "Bill due soon",
			fmt.Sprintf("₹%s is due on your %s in %d %s.", remaining.String(), name, days, unit)

	default: // statementReminderOverdue
		return "Bill past due",
			fmt.Sprintf("₹%s on your %s was due on %s. Record the payment once it's done.",
				remaining.String(), name, statement.DueDate)
	}
}
