package models

import (
	"time"

	"gorm.io/gorm"
)

// CardEMIPlan is a purchase converted to instalments on a credit card.
//
// The behaviour that governs this model, and which matches how Indian issuers
// actually work: converting a ₹60,000 purchase to twelve instalments blocks
// the **full ₹60,000 principal** against the card's limit straight away, and
// releases it a slice at a time as each instalment's principal is paid. The
// user never gets that limit back in one go, and never has it all available
// while they still owe it.
type CardEMIPlan struct {
	ID        uint     `gorm:"primaryKey" json:"id"`
	UserID    uint     `gorm:"index;not null" json:"user_id"`
	User      User     `json:"-" gorm:"foreignKey:UserID"`
	AccountID uint     `gorm:"index;not null" json:"account_id"`
	Account   *Account `json:"account,omitempty" gorm:"foreignKey:AccountID"`

	Title    string `gorm:"type:varchar(120);not null" json:"title"`
	Merchant string `gorm:"type:varchar(120)" json:"merchant"`
	Category string `gorm:"type:varchar(80)" json:"category"`

	// Principal is the purchase amount — the sum of every instalment's
	// principal component, and the amount blocked at the start.
	Principal     Money   `gorm:"type:numeric(19,2);not null" json:"principal"`
	AnnualRatePct float64 `gorm:"not null;default:0" json:"annual_rate_pct"`
	TenureMonths  int     `gorm:"not null" json:"tenure_months"`
	// MonthlyAmount is principal plus interest — what the statement bills.
	MonthlyAmount Money  `gorm:"type:numeric(19,2);not null" json:"monthly_amount"`
	TotalInterest Money  `gorm:"type:numeric(19,2);not null;default:0" json:"total_interest"`
	Currency      string `gorm:"type:char(3);not null;default:INR" json:"currency"`

	PurchasedOn      string `gorm:"type:date;not null" json:"purchased_on"`
	FirstInstallment string `gorm:"type:date;not null" json:"first_installment"`

	// active -> closed (ran its course) or foreclosed (paid off early).
	Status string `gorm:"type:varchar(16);not null;default:active;index" json:"status"`

	// The original purchase entry, when the plan was created by converting one.
	// The entry itself is removed: a card bills instalments, not the purchase,
	// so leaving it would double-count the spend and blow a hole in the
	// statement's reconciliation.
	SourceEntryID *uint `gorm:"index" json:"source_entry_id,omitempty"`

	Notes     string    `gorm:"type:text" json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Installments []CardEMIInstallment `json:"installments,omitempty" gorm:"foreignKey:PlanID"`
}

// TableName is set explicitly because GORM's snake_case converter mangles the
// EMI acronym: CardEMIPlan pluralises to "card_em_iplans", which does not match
// the "card_emi_plans" the migration creates. The mismatch is silent —
// AutoMigrate simply creates a second, empty table alongside the real one — so
// this is pinned here and asserted in card_emi_test.go.
func (CardEMIPlan) TableName() string { return "card_emi_plans" }

// CardEMIInstallment is one month of a plan, straight out of the amortisation
// schedule `emi.go` already knows how to build.
//
// The split between PrincipalPart and InterestPart is the point of storing the
// schedule at all: **only the principal releases limit.** Interest is a charge,
// not a repayment of what was borrowed, so a card carrying an interest-bearing
// EMI frees up less limit each month than the instalment it pays.
type CardEMIInstallment struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// No back-reference to the plan: pairing it with CardEMIPlan.Installments
	// makes the association circular, and AutoMigrate then creates the
	// instalment table without its parent.
	PlanID uint `gorm:"index;not null" json:"plan_id"`
	UserID uint `gorm:"index;not null" json:"user_id"`
	// Denormalised from the plan so blocked principal is one grouped query.
	AccountID uint `gorm:"index;not null" json:"account_id"`

	Seq           int    `gorm:"not null" json:"seq"`
	DueDate       string `gorm:"type:date;index;not null" json:"due_date"`
	Amount        Money  `gorm:"type:numeric(19,2);not null" json:"amount"`
	PrincipalPart Money  `gorm:"type:numeric(19,2);not null" json:"principal_part"`
	InterestPart  Money  `gorm:"type:numeric(19,2);not null" json:"interest_part"`

	// scheduled: not yet billed — its principal is blocking the limit.
	// billed:    on a statement — its principal is inside that bill's total,
	//            so it must NOT also count as blocked or the limit is
	//            reduced twice for the same money.
	// paid:      the statement was settled — the principal is released.
	Status string `gorm:"type:varchar(16);not null;default:scheduled;index" json:"status"`

	// The statement this instalment landed on, and the expense entry created
	// for it. Both nil until it is billed.
	StatementID *uint `gorm:"index" json:"statement_id,omitempty"`
	EntryID     *uint `gorm:"index" json:"entry_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (CardEMIInstallment) TableName() string { return "card_emi_installments" }

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (plan *CardEMIPlan) AfterFind(_ *gorm.DB) error {
	plan.PurchasedOn = CalendarDay(plan.PurchasedOn)
	plan.FirstInstallment = CalendarDay(plan.FirstInstallment)
	return nil
}

func (installment *CardEMIInstallment) AfterFind(_ *gorm.DB) error {
	installment.DueDate = CalendarDay(installment.DueDate)
	return nil
}
