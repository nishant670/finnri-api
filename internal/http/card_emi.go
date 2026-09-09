package http

import (
	"time"

	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Card EMI plans: the schedule, and what it does to the limit.

Everything here rests on one rule that is easy to state and easy to get wrong:

	Blocked principal counts SCHEDULED instalments only.

An instalment that has been billed onto a statement already sits inside that
statement's total_due, which is already inside the card's outstanding. Counting
it as blocked at the same time would reduce the available limit twice for the
same rupee, for the whole month between billing and payment. The status field
exists for this reason and no other.

The arithmetic itself is not reimplemented — emi.go has computed amortisation
schedules since before this feature, and buildEMICalculation returns exactly
the per-month principal/interest split the limit release needs.
*/

const (
	emiPlanActive     = "active"
	emiPlanClosed     = "closed"
	emiPlanForeclosed = "foreclosed"
)

const (
	emiInstallmentScheduled = "scheduled"
	emiInstallmentBilled    = "billed"
	emiInstallmentPaid      = "paid"
)

// emiInstallmentTag marks the ledger entries a plan generates, so a user can
// tell an instalment apart from an ordinary card spend in their transactions.
const emiInstallmentTag = "EMI"

// buildInstallments turns a plan into its months.
//
// Due dates step one month at a time from the first instalment, clamped to
// month length so a plan starting on the 31st does not skip February. The
// amounts come straight from the amortisation schedule, so the principal parts
// sum to exactly the principal — buildEMISchedule gives the final month
// whatever balance is left, which is where the rounding goes.
func buildInstallments(plan models.CardEMIPlan, schedule []emiScheduleMonth) []models.CardEMIInstallment {
	first, err := parseAPIDate(plan.FirstInstallment)
	if err != nil {
		return nil
	}
	anchorDay := first.Day()

	installments := make([]models.CardEMIInstallment, 0, len(schedule))
	for _, month := range schedule {
		due := clampDayToMonth(first.Year(), first.Month()+time.Month(month.Month-1), anchorDay)
		installments = append(installments, models.CardEMIInstallment{
			PlanID:        plan.ID,
			UserID:        plan.UserID,
			AccountID:     plan.AccountID,
			Seq:           month.Month,
			DueDate:       due.Format(apiDateLayout),
			Amount:        month.PaymentAmount,
			PrincipalPart: month.PrincipalAmount,
			InterestPart:  month.InterestAmount,
			Status:        emiInstallmentScheduled,
		})
	}
	return installments
}

// loadBlockedPrincipal is how much limit each of a user's cards has held
// against EMI plans that have not been billed yet.
//
// Cards with nothing blocked are simply absent from the map, and zero is the
// right answer for them.
func loadBlockedPrincipal(userID uint, cardIDs []uint) (map[uint]models.Money, error) {
	blocked := make(map[uint]models.Money, len(cardIDs))
	if len(cardIDs) == 0 {
		return blocked, nil
	}

	var rows []struct {
		AccountID uint
		Blocked   models.Money
	}
	if err := database.DB.Model(&models.CardEMIInstallment{}).
		Select("card_emi_installments.account_id AS account_id, COALESCE(SUM(card_emi_installments.principal_part), 0) AS blocked").
		Joins("JOIN card_emi_plans ON card_emi_plans.id = card_emi_installments.plan_id").
		Where("card_emi_installments.user_id = ? AND card_emi_installments.account_id IN ?", userID, cardIDs).
		Where("card_emi_installments.status = ?", emiInstallmentScheduled).
		Where("card_emi_plans.status = ?", emiPlanActive).
		Group("card_emi_installments.account_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	for _, row := range rows {
		blocked[row.AccountID] = row.Blocked
	}
	return blocked, nil
}

// emiPlanProgress is what a plan has left to run, for the app to render.
type emiPlanProgress struct {
	InstallmentsTotal  int          `json:"installments_total"`
	InstallmentsPaid   int          `json:"installments_paid"`
	PrincipalRemaining models.Money `json:"principal_remaining"`
	PrincipalRepaid    models.Money `json:"principal_repaid"`
	// BlockedPrincipal is the part still holding limit — scheduled only, for
	// the same reason the limit calculation counts only scheduled.
	BlockedPrincipal models.Money `json:"blocked_principal"`
	AmountRemaining  models.Money `json:"amount_remaining"`
	NextDueDate      string       `json:"next_due_date,omitempty"`
	NextAmount       models.Money `json:"next_amount,omitempty"`
}

// summariseEMIPlan derives a plan's progress from its instalments.
func summariseEMIPlan(installments []models.CardEMIInstallment) emiPlanProgress {
	progress := emiPlanProgress{InstallmentsTotal: len(installments)}

	for _, installment := range installments {
		switch installment.Status {
		case emiInstallmentPaid:
			progress.InstallmentsPaid++
			progress.PrincipalRepaid += installment.PrincipalPart
			continue
		case emiInstallmentScheduled:
			progress.BlockedPrincipal += installment.PrincipalPart
		}

		// Scheduled and billed alike are still owed.
		progress.PrincipalRemaining += installment.PrincipalPart
		progress.AmountRemaining += installment.Amount

		if progress.NextDueDate == "" || installment.DueDate < progress.NextDueDate {
			progress.NextDueDate = installment.DueDate
			progress.NextAmount = installment.Amount
		}
	}

	return progress
}

// cardEMIPlanResponse is a plan with its derived progress, so no screen has to
// add up a schedule to say what is left.
type cardEMIPlanResponse struct {
	models.CardEMIPlan
	Progress emiPlanProgress `json:"progress"`
}

func loadEMIPlanWithInstallments(userID, planID uint) (models.CardEMIPlan, error) {
	var plan models.CardEMIPlan
	err := database.DB.
		Preload("Installments", func(db *gorm.DB) *gorm.DB { return db.Order("seq ASC") }).
		Where("id = ? AND user_id = ?", planID, userID).
		First(&plan).Error
	return plan, err
}
