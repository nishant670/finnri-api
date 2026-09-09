package http

import (
	"strconv"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
The month-to-month life of an EMI plan.

	scheduled --(due date arrives)--> billed --(statement settled)--> paid
	    |                                |                              |
	blocks limit                  inside the bill                 limit released

Billing is driven by the instalment's own due date rather than by a statement
existing, because a user who has not entered this month's bill yet still has an
instalment that the bank has charged them. The statement link is attached when
a covering statement is available and left null when it is not; the release
step handles both.

Every instalment produces a real expense entry on the card for its **full**
amount, principal and interest together. That is what the statement bills, so
it is what the ledger should show — and it means an EMI shrinks a statement's
reconciliation gap rather than causing one.
*/

// syncCardEMIInstallments bills every instalment whose due date has arrived.
// Safe to call repeatedly: only `scheduled` rows are touched, and billing
// moves them out of that state.
func syncCardEMIInstallments(userID uint, now time.Time) ([]models.CardEMIInstallment, error) {
	today := truncateDate(now).Format(apiDateLayout)

	var due []models.CardEMIInstallment
	if err := database.DB.
		// Explicit: a bare SELECT * across this join returns both tables'
		// columns, and the two share `status`, `id` and the timestamps. The
		// plan's "active" would land in the instalment's status field and its
		// dates in the instalment's dates — every row scanned into a lie.
		Select("card_emi_installments.*").
		Model(&models.CardEMIInstallment{}).
		Joins("JOIN card_emi_plans ON card_emi_plans.id = card_emi_installments.plan_id").
		Where("card_emi_installments.user_id = ? AND card_emi_installments.status = ?",
			userID, emiInstallmentScheduled).
		Where("card_emi_installments.due_date <= ?", today).
		Where("card_emi_plans.status = ?", emiPlanActive).
		Order("card_emi_installments.due_date ASC, card_emi_installments.seq ASC").
		Find(&due).Error; err != nil {
		return nil, err
	}

	billed := make([]models.CardEMIInstallment, 0, len(due))
	for index := range due {
		installment := due[index]
		if err := billEMIInstallment(&installment); err != nil {
			return billed, err
		}
		billed = append(billed, installment)
	}

	// A plan whose last instalment has been billed has nothing left to
	// schedule, but it is not closed until the money is actually paid.
	if err := closeFullyPaidEMIPlans(userID); err != nil {
		return billed, err
	}
	return billed, nil
}

// dateOnly trims a stored date back to YYYY-MM-DD.
//
// A `type:date` column round-trips through the SQLite driver used by the smoke
// suite as a full RFC3339 timestamp, while Postgres hands back a plain date.
// parseAPIDate has always truncated for the same reason. It matters here
// because the value is copied onto an Entry, whose `date` column is ordinary
// text compared directly against a statement's cycle bounds — a stray
// "T00:00:00Z" would sort outside the cycle it belongs to.
func dateOnly(value string) string {
	if len(value) >= len(apiDateLayout) {
		return value[:len(apiDateLayout)]
	}
	return value
}

// billEMIInstallment writes the month's expense onto the card and links the
// instalment to whichever statement covers its due date.
func billEMIInstallment(installment *models.CardEMIInstallment) error {
	var plan models.CardEMIPlan
	if err := database.DB.First(&plan, installment.PlanID).Error; err != nil {
		return err
	}
	dueDate := dateOnly(installment.DueDate)

	entry := models.Entry{
		Title:     plan.Title,
		Type:      "expense",
		Amount:    installment.Amount,
		Currency:  plan.Currency,
		Source:    "manual",
		Category:  emiEntryCategory(plan.Category),
		Merchant:  plan.Merchant,
		Tag:       emiInstallmentTag,
		Notes:     emiInstallmentNote(plan, *installment),
		Date:      dueDate,
		AccountID: &installment.AccountID,
		UserID:    installment.UserID,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		return err
	}

	statementID, err := findCoveringStatementID(installment.UserID, installment.AccountID, dueDate)
	if err != nil {
		return err
	}

	installment.Status = emiInstallmentBilled
	installment.EntryID = &entry.ID
	installment.StatementID = statementID

	return database.DB.Model(installment).Updates(map[string]any{
		"status":       installment.Status,
		"entry_id":     installment.EntryID,
		"statement_id": installment.StatementID,
	}).Error
}

// findCoveringStatementID is the statement whose cycle contains a date, when
// one exists. Nil is an ordinary answer: the bill for the cycle an instalment
// just landed in usually has not been entered yet.
func findCoveringStatementID(userID, accountID uint, date string) (*uint, error) {
	var statement models.CardStatement
	err := database.DB.
		Where("user_id = ? AND account_id = ? AND cycle_start <= ? AND cycle_end >= ?",
			userID, accountID, date, date).
		Order("statement_date DESC").
		First(&statement).Error
	if err != nil {
		return nil, nil
	}
	return &statement.ID, nil
}

// releaseEMIPrincipalForStatement is called when a statement is settled. Every
// instalment that bill covered becomes paid, and its principal stops blocking
// the limit — which is the moment the user gets that headroom back.
//
// Instalments are matched by cycle rather than only by the statement link,
// because an instalment billed before its statement existed carries no link.
// Both are covered by the same query.
func releaseEMIPrincipalForStatement(statement *models.CardStatement) error {
	if statement.Status != statementStatusPaid {
		return nil
	}
	return database.DB.Model(&models.CardEMIInstallment{}).
		Where("user_id = ? AND account_id = ? AND status = ?",
			statement.UserID, statement.AccountID, emiInstallmentBilled).
		Where("statement_id = ? OR due_date <= ?", statement.ID, statement.CycleEnd).
		Update("status", emiInstallmentPaid).Error
}

// reblockEMIPrincipalForStatement is the reverse, for when a payment is
// removed and the bill goes back to being unpaid. Without it, deleting a
// payment would leave the limit permanently released for instalments that are
// owed again.
func reblockEMIPrincipalForStatement(statement *models.CardStatement) error {
	if statement.Status == statementStatusPaid {
		return nil
	}
	return database.DB.Model(&models.CardEMIInstallment{}).
		Where("user_id = ? AND account_id = ? AND status = ?",
			statement.UserID, statement.AccountID, emiInstallmentPaid).
		Where("statement_id = ? OR due_date <= ?", statement.ID, statement.CycleEnd).
		Update("status", emiInstallmentBilled).Error
}

// closeFullyPaidEMIPlans retires plans with nothing left owing.
func closeFullyPaidEMIPlans(userID uint) error {
	var planIDs []uint
	if err := database.DB.Model(&models.CardEMIPlan{}).
		Where("user_id = ? AND status = ?", userID, emiPlanActive).
		Pluck("id", &planIDs).Error; err != nil {
		return err
	}

	for _, planID := range planIDs {
		var outstanding int64
		if err := database.DB.Model(&models.CardEMIInstallment{}).
			Where("plan_id = ? AND status <> ?", planID, emiInstallmentPaid).
			Count(&outstanding).Error; err != nil {
			return err
		}
		if outstanding > 0 {
			continue
		}
		if err := database.DB.Model(&models.CardEMIPlan{}).
			Where("id = ?", planID).
			Update("status", emiPlanClosed).Error; err != nil {
			return err
		}
	}
	return nil
}

// forecloseEMIPlan pays a plan off early: every instalment still scheduled is
// dropped, releasing its principal in one go.
//
// Instalments already billed are left alone. They are on a statement the user
// still owes, and cancelling them would erase a charge the bank has made.
func forecloseEMIPlan(plan *models.CardEMIPlan) error {
	if err := database.DB.Model(&models.CardEMIInstallment{}).
		Where("plan_id = ? AND status = ?", plan.ID, emiInstallmentScheduled).
		Delete(&models.CardEMIInstallment{}).Error; err != nil {
		return err
	}
	plan.Status = emiPlanForeclosed
	return database.DB.Model(plan).Update("status", emiPlanForeclosed).Error
}

// emiEntryCategory keeps a plan's category if it is one Finnri recognises, and
// otherwise files the instalment under the confirm-first fallback rather than
// inventing a category the rest of the app cannot group by.
func emiEntryCategory(category string) string {
	if resolved, ok := categoryForSave(category, "expense"); ok {
		return resolved
	}
	return defaultCategory
}

func emiInstallmentNote(plan models.CardEMIPlan, installment models.CardEMIInstallment) string {
	return "EMI " + strconv.Itoa(installment.Seq) + " of " + strconv.Itoa(plan.TenureMonths) +
		" · ₹" + installment.PrincipalPart.String() + " principal, ₹" +
		installment.InterestPart.String() + " interest"
}
