package http

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
The card's annual fee: when it renews, how close this year's spend is to
waiving it, and one reminder a month before.

The card year is taken to end on the 1st of the fee month: the twelve months
before it are the ones whose spend counts toward the waiver. Banks count
from the card's anniversary and exclude some charges (fees, interest, EMIs,
fuel, rent); Finnri counts net spend on the card. So the figure is a guide,
and the copy says "about", never "exactly".
*/

const (
	annualFeeReminderType = "card.annual_fee"
	// annualFeeReminderLeadDays is how early the reminder goes out: enough
	// time to spend toward a waiver or to ask the bank about one.
	annualFeeReminderLeadDays = 30
)

// annualFeeStatus is a card's fee position, for the card screen.
type annualFeeStatus struct {
	Fee      models.Money `json:"fee"`
	FeeMonth string       `json:"fee_month"`
	// RenewalDate is the 1st of the fee month the status is counting toward.
	RenewalDate   string       `json:"renewal_date"`
	CardYearStart string       `json:"card_year_start"`
	Spent         models.Money `json:"spent"`
	// Present only when the card has a waiver threshold.
	WaiverSpend      *models.Money `json:"waiver_spend,omitempty"`
	RemainingToWaive *models.Money `json:"remaining_to_waive,omitempty"`
	Waived           *bool         `json:"waived,omitempty"`
}

func feeMonthNumber(name string) (time.Month, bool) {
	for index, month := range monthNames {
		if strings.EqualFold(strings.TrimSpace(name), month) {
			return time.Month(index + 1), true
		}
	}
	return 0, false
}

// annualFeeWindow is the renewal the card is heading toward and the card year
// before it. Through the whole fee month the renewal is this month's — the
// fee is being charged now — and it rolls to next year only once the month
// has ended.
func annualFeeWindow(feeMonth time.Month, today time.Time) (renewal, yearStart time.Time) {
	today = truncateDate(today)
	renewal = time.Date(today.Year(), feeMonth, 1, 0, 0, 0, 0, today.Location())
	if !today.Before(renewal.AddDate(0, 1, 0)) {
		renewal = renewal.AddDate(1, 0, 0)
	}
	return renewal, renewal.AddDate(-1, 0, 0)
}

// cardHasAnnualFee is whether there is anything to track: a fee and a month.
func cardHasAnnualFee(card models.Account) (time.Month, bool) {
	if normalizeAccountType(card.Type) != "credit_card" || card.AnnualFee <= 0 {
		return 0, false
	}
	return feeMonthNumber(card.FeeMonth)
}

func summariseAnnualFee(card models.Account, renewal, yearStart time.Time, spent models.Money) annualFeeStatus {
	status := annualFeeStatus{
		Fee:           card.AnnualFee,
		FeeMonth:      renewal.Month().String(),
		RenewalDate:   renewal.Format(apiDateLayout),
		CardYearStart: yearStart.Format(apiDateLayout),
		Spent:         spent,
	}
	if card.FeeWaiverSpend > 0 {
		waiver := card.FeeWaiverSpend
		remaining := waiver - spent
		if remaining < 0 {
			remaining = 0
		}
		waived := remaining == 0
		status.WaiverSpend = &waiver
		status.RemainingToWaive = &remaining
		status.Waived = &waived
	}
	return status
}

// loadAnnualFeeStatus works out one card's position; nil when the card has
// no fee or no fee month to track.
func loadAnnualFeeStatus(card models.Account, today time.Time) (*annualFeeStatus, error) {
	feeMonth, ok := cardHasAnnualFee(card)
	if !ok {
		return nil, nil
	}
	renewal, yearStart := annualFeeWindow(feeMonth, today)
	yearEnd := renewal.AddDate(0, 0, -1)
	spent, _, err := loadCycleItemizedTotal(card.UserID, card.ID,
		yearStart.Format(apiDateLayout), yearEnd.Format(apiDateLayout), nil)
	if err != nil {
		return nil, err
	}
	status := summariseAnnualFee(card, renewal, yearStart, spent)
	return &status, nil
}

// attachAnnualFeeStatus adds fee progress to the cards that track one. A
// card without a fee costs nothing; a card with one costs one sum.
func attachAnnualFeeStatus(accounts []accountWithSummary, today time.Time) error {
	for index := range accounts {
		status, err := loadAnnualFeeStatus(accounts[index].Account, today)
		if err != nil {
			return err
		}
		accounts[index].Summary.AnnualFee = status
	}
	return nil
}

// syncAnnualFeeReminders sends each card's renewal reminder once per year,
// from 30 days before the fee month until the month ends.
func syncAnnualFeeReminders(userID uint, now time.Time) (int, error) {
	var cards []models.Account
	if err := database.DB.
		Where("user_id = ? AND LOWER(type) IN ? AND annual_fee > 0 AND fee_month <> ''",
			userID, []string{"credit_card", "credit"}).
		Find(&cards).Error; err != nil {
		return 0, err
	}
	today := truncateDate(now.In(indiaLocation()))
	created := 0
	for _, card := range cards {
		feeMonth, ok := cardHasAnnualFee(card)
		if !ok {
			continue
		}
		renewal, _ := annualFeeWindow(feeMonth, today)
		if today.Before(renewal.AddDate(0, 0, -annualFeeReminderLeadDays)) {
			continue
		}
		status, err := loadAnnualFeeStatus(card, today)
		if err != nil {
			return created, err
		}
		sent, err := createAnnualFeeReminderIfNeeded(card, *status, renewal)
		if err != nil {
			return created, err
		}
		if sent {
			created++
		}
	}
	return created, nil
}

func createAnnualFeeReminderIfNeeded(card models.Account, status annualFeeStatus, renewal time.Time) (bool, error) {
	// The renewal year is in the URL, so the unique index on
	// (user, type, action_url) allows exactly one reminder per card per year.
	actionURL := fmt.Sprintf("/accounts/%d?fee_year=%d", card.ID, renewal.Year())
	title, body := annualFeeReminderCopy(card, status, renewal)
	notification := models.Notification{
		UserID: card.UserID, Type: annualFeeReminderType,
		Title: title, Body: body, ActionURL: actionURL,
	}
	created := false
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var existing models.Notification
		if err := tx.Where("user_id = ? AND type = ? AND action_url = ?", card.UserID, annualFeeReminderType, actionURL).
			First(&existing).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&notification)
		created = result.RowsAffected == 1
		return result.Error
	})
	if err != nil {
		return false, err
	}
	if created {
		go sendUserPush(database.DB, card.UserID, title, body, map[string]any{
			"action_url": actionURL, "account_id": card.ID,
		})
	}
	return created, nil
}

// annualFeeReminderCopy writes the reminder. Waiver figures are Finnri's
// count of net card spend, so they are "about" — the bank's own rules decide.
func annualFeeReminderCopy(card models.Account, status annualFeeStatus, renewal time.Time) (string, string) {
	name := strings.TrimSpace(card.Name)
	if name == "" {
		name = "your credit card"
	}
	month := renewal.Month().String()
	fee := rupeesForCopy(status.Fee)
	if status.Waived != nil && *status.Waived {
		return "Annual fee should be waived",
			fmt.Sprintf("You spent about %s on %s this card year, above the %s that waives its %s annual fee. Check your %s statement to make sure it isn't charged.",
				rupeesForCopy(status.Spent), name, rupeesForCopy(*status.WaiverSpend), fee, month)
	}
	if status.RemainingToWaive != nil {
		lastDay := renewal.AddDate(0, 0, -1).Format("2 Jan")
		return "Annual fee coming up",
			fmt.Sprintf("%s's %s annual fee renews in %s. About %s more spend by %s should get it waived.",
				name, fee, month, rupeesForCopy(*status.RemainingToWaive), lastDay)
	}
	return "Annual fee coming up",
		fmt.Sprintf("%s's %s annual fee (plus GST) renews in %s. Ask your bank whether it can be waived.", name, fee, month)
}

// indiaLocation is the zone the reminder days are counted in.
func indiaLocation() *time.Location {
	if location, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return location
	}
	return time.FixedZone("IST", 5*3600+1800)
}

// rupeesForCopy writes an amount the way a person reads it in a notification:
// Indian digit grouping, and paise only when there are any. "₹1,00,000",
// "₹18,000", "₹499.50".
func rupeesForCopy(amount models.Money) string {
	negative := amount < 0
	if negative {
		amount = -amount
	}
	whole := int64(amount) / 100
	paise := int64(amount) % 100

	digits := fmt.Sprintf("%d", whole)
	grouped := digits
	if len(digits) > 3 {
		head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
		parts := []string{}
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		grouped = strings.Join(parts, ",") + "," + tail
	}
	if paise != 0 {
		grouped += fmt.Sprintf(".%02d", paise)
	}
	if negative {
		return "-₹" + grouped
	}
	return "₹" + grouped
}
