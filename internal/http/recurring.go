package http

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Recurring payments, in one place.

Every kind — subscription, loan EMI, investment, bill — is a Subscription row
on the same schedule machinery (autopay occurrences, reminders, instalment
counts). This file adds what the Recurring tab needs on top: each item's
schedule in plain figures, and an overview that also lists the card EMI plans,
which stay their own system because statements and card limits depend on them
and are shown read-only.
*/

// recurringSchedule is an item's schedule worked out for display. Loan figures
// appear only when the user gave what they need.
type recurringSchedule struct {
	// MonthlyEquivalent puts weekly, quarterly and yearly payments on one
	// scale, so the tab can total what a month commits.
	MonthlyEquivalent models.Money `json:"monthly_equivalent"`

	// Instalment-limited schedules (loans) only.
	TotalInstalments     int    `json:"total_instalments,omitempty"`
	InstalmentsPaid      int    `json:"instalments_paid,omitempty"`
	RemainingInstalments int    `json:"remaining_instalments,omitempty"`
	EndDate              string `json:"end_date,omitempty"`
	Completed            bool   `json:"completed,omitempty"`

	AmountPaid           *models.Money `json:"amount_paid,omitempty"`
	OutstandingPrincipal *models.Money `json:"outstanding_principal,omitempty"`
	TotalInterest        *models.Money `json:"total_interest,omitempty"`

	// Investments with a start date: about what has gone in so far, counted
	// from the schedule (step-ups are not projected backwards).
	InvestedSoFar *models.Money `json:"invested_so_far,omitempty"`
}

// monthlyEquivalent scales one payment to an average month.
func monthlyEquivalent(amount models.Money, interval string) models.Money {
	perYear := map[string]float64{
		subscriptionIntervalDaily:         365,
		subscriptionIntervalBusinessDaily: 252,
		subscriptionIntervalWeekly:        52,
		subscriptionIntervalBiweekly:      26,
		subscriptionIntervalMonthly:       12,
		subscriptionIntervalQuarterly:     4,
		subscriptionIntervalYearly:        1,
	}[interval]
	if perYear == 0 {
		perYear = 12
	}
	return models.Money(math.Round(float64(amount) * perYear / 12))
}

func computeRecurringSchedule(subscription models.Subscription, today time.Time) recurringSchedule {
	schedule := recurringSchedule{
		MonthlyEquivalent: monthlyEquivalent(subscription.Amount, subscription.BillingInterval),
	}

	if subscription.TotalInstalments > 0 {
		total, paid := subscription.TotalInstalments, subscription.InstalmentsPaid
		if paid > total {
			paid = total
		}
		schedule.TotalInstalments = total
		schedule.InstalmentsPaid = paid
		schedule.RemainingInstalments = total - paid
		schedule.Completed = paid >= total
		amountPaid := subscription.Amount * models.Money(paid)
		schedule.AmountPaid = &amountPaid

		if schedule.Completed {
			schedule.EndDate = subscription.LastChargedDate
		} else if next, err := parseAPIDate(subscription.NextDueDate); err == nil {
			end := next
			for step := 1; step < schedule.RemainingInstalments; step++ {
				end = addSubscriptionInterval(end, subscription.BillingInterval)
			}
			schedule.EndDate = end.Format(apiDateLayout)
		}

		if subscription.Principal > 0 {
			interest := subscription.Amount*models.Money(total) - subscription.Principal
			if interest < 0 {
				interest = 0
			}
			schedule.TotalInterest = &interest
			if subscription.BillingInterval == subscriptionIntervalMonthly {
				outstanding := outstandingPrincipal(subscription.Principal, subscription.AnnualRatePct, subscription.Amount, paid)
				if schedule.Completed {
					outstanding = 0
				}
				schedule.OutstandingPrincipal = &outstanding
			}
		}
	}

	if subscription.Kind == recurringKindInvestment && subscription.StartDate != "" {
		if count := paymentsSince(subscription, today); count > 0 {
			invested := subscription.Amount * models.Money(count)
			schedule.InvestedSoFar = &invested
		}
	}
	return schedule
}

// outstandingPrincipal is a reducing-balance loan's principal after `paid`
// monthly payments of `emi`. It uses the EMI the user pays, not one computed
// from the rate, so it stays consistent with what actually leaves the account.
func outstandingPrincipal(principal models.Money, annualRatePct float64, emi models.Money, paid int) models.Money {
	p, a := float64(principal), float64(emi)
	var remaining float64
	if annualRatePct <= 0 {
		remaining = p - a*float64(paid)
	} else {
		r := annualRatePct / 12 / 100
		growth := math.Pow(1+r, float64(paid))
		remaining = p*growth - a*(growth-1)/r
	}
	if remaining < 0 {
		return 0
	}
	return models.Money(math.Round(remaining))
}

// paymentsSince counts the scheduled payments from the start date up to the
// last one taken (or today, when nothing has been recorded yet).
func paymentsSince(subscription models.Subscription, today time.Time) int {
	start, err := parseAPIDate(subscription.StartDate)
	if err != nil {
		return 0
	}
	until := truncateDate(today)
	if last, err := parseAPIDate(subscription.LastChargedDate); err == nil {
		until = last
	}
	count := 0
	for date := start; !date.After(until) && count < 10000; date = addSubscriptionInterval(date, subscription.BillingInterval) {
		count++
	}
	return count
}

// recurringCardEMI is a card EMI plan as the Recurring tab lists it: read-only,
// opening the plan's own screen.
type recurringCardEMI struct {
	PlanID           uint         `json:"plan_id"`
	AccountID        uint         `json:"account_id"`
	CardName         string       `json:"card_name"`
	Title            string       `json:"title"`
	MonthlyAmount    models.Money `json:"monthly_amount"`
	NextDueDate      string       `json:"next_due_date,omitempty"`
	TotalInstalments int          `json:"total_instalments"`
	InstalmentsPaid  int          `json:"instalments_paid"`
}

type recurringNextDue struct {
	Name   string       `json:"name"`
	Kind   string       `json:"kind"`
	Amount models.Money `json:"amount"`
	Date   string       `json:"date"`
}

type recurringSummary struct {
	// MonthlyTotal is what active recurring payments commit an average month
	// to — including card EMIs and investments, which leave the account too.
	MonthlyTotal models.Money            `json:"monthly_total"`
	ByKind       map[string]models.Money `json:"by_kind"`
	ActiveCount  int                     `json:"active_count"`
	NextDue      *recurringNextDue       `json:"next_due,omitempty"`
}

type recurringOverview struct {
	Summary  recurringSummary       `json:"summary"`
	Items    []subscriptionResponse `json:"items"`
	CardEMIs []recurringCardEMI     `json:"card_emis"`
}

func (s *Server) listRecurring(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	now := time.Now().In(indiaLocation())

	var subscriptions []models.Subscription
	if err := database.DB.Preload("Account").Where("user_id = ?", userID).
		Order("next_due_date asc, name asc").Find(&subscriptions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_recurring"})
		return
	}
	cardEMIs, err := loadRecurringCardEMIs(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_card_emis"})
		return
	}
	items := buildSubscriptionResponses(subscriptions, now)
	c.JSON(http.StatusOK, recurringOverview{
		Summary:  summariseRecurring(items, cardEMIs),
		Items:    items,
		CardEMIs: cardEMIs,
	})
}

func loadRecurringCardEMIs(userID uint) ([]recurringCardEMI, error) {
	var plans []models.CardEMIPlan
	if err := database.DB.Preload("Account").Preload("Installments").
		Where("user_id = ? AND status = ?", userID, emiPlanActive).
		Order("first_installment asc").Find(&plans).Error; err != nil {
		return nil, err
	}
	rows := make([]recurringCardEMI, 0, len(plans))
	for _, plan := range plans {
		row := recurringCardEMI{
			PlanID: plan.ID, AccountID: plan.AccountID, Title: plan.Title,
			MonthlyAmount: plan.MonthlyAmount, TotalInstalments: plan.TenureMonths,
		}
		if plan.Account != nil {
			row.CardName = plan.Account.Name
		}
		installments := append([]models.CardEMIInstallment(nil), plan.Installments...)
		sort.Slice(installments, func(i, j int) bool { return installments[i].Seq < installments[j].Seq })
		for _, installment := range installments {
			if installment.Status == emiInstallmentScheduled {
				if row.NextDueDate == "" {
					row.NextDueDate = installment.DueDate
				}
			} else {
				row.InstalmentsPaid++
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func summariseRecurring(items []subscriptionResponse, cardEMIs []recurringCardEMI) recurringSummary {
	summary := recurringSummary{ByKind: map[string]models.Money{
		recurringKindLoan: 0, recurringKindSubscription: 0, recurringKindInvestment: 0,
		recurringKindBill: 0, "card_emi": 0,
	}}
	consider := func(name, kind string, amount models.Money, date string) {
		if date == "" {
			return
		}
		if summary.NextDue == nil || date < summary.NextDue.Date {
			summary.NextDue = &recurringNextDue{Name: name, Kind: kind, Amount: amount, Date: date}
		}
	}
	for _, item := range items {
		if item.Status != subscriptionStatusActive {
			continue
		}
		kind := item.Kind
		if kind == "" {
			kind = recurringKindSubscription
		}
		summary.ActiveCount++
		summary.MonthlyTotal += item.Schedule.MonthlyEquivalent
		summary.ByKind[kind] += item.Schedule.MonthlyEquivalent
		consider(item.Name, kind, item.Amount, item.NextDueDate)
	}
	for _, emi := range cardEMIs {
		summary.ActiveCount++
		summary.MonthlyTotal += emi.MonthlyAmount
		summary.ByKind["card_emi"] += emi.MonthlyAmount
		consider(emi.Title, "card_emi", emi.MonthlyAmount, emi.NextDueDate)
	}
	return summary
}

// recurringNoun is what a reminder calls the payment.
func recurringNoun(kind string) string {
	switch kind {
	case recurringKindLoan:
		return "EMI"
	case recurringKindInvestment:
		return "Investment"
	case recurringKindBill:
		return "Bill"
	default:
		return "Subscription"
	}
}

func recurringAutopayTitle(kind string) string {
	switch kind {
	case recurringKindLoan:
		return "EMI recorded"
	case recurringKindInvestment:
		return "Investment recorded"
	case recurringKindBill:
		return "Bill payment recorded"
	default:
		return "Autopay transaction added"
	}
}

// recurringCompletedType notifications open the Recurring tab.
const recurringCompletedType = "recurring.completed"

// notifyRecurringCompleted marks the last instalment of a schedule: a loan
// paid off, or a fixed-term investment finished. Best-effort — the schedule is
// already closed, and a missed notification costs a moment of good news, not
// a wrong number.
func notifyRecurringCompleted(subscription models.Subscription) {
	name := strings.TrimSpace(subscription.Name)
	if name == "" {
		name = "Your loan"
	}
	title := "Loan paid off"
	body := fmt.Sprintf("%s: all %d EMIs are done. It has moved out of your monthly commitments.", name, subscription.TotalInstalments)
	if subscription.Kind != recurringKindLoan && subscription.Kind != "" {
		title = "Schedule complete"
		body = fmt.Sprintf("%s: all %d payments are done.", name, subscription.TotalInstalments)
	}
	actionURL := fmt.Sprintf("/recurring/%d", subscription.ID)
	notification := models.Notification{
		UserID: subscription.UserID, Type: recurringCompletedType,
		Title: title, Body: body, ActionURL: actionURL,
	}
	if err := database.DB.Create(&notification).Error; err != nil {
		return
	}
	go sendUserPush(database.DB, subscription.UserID, title, body, map[string]any{
		"action_url": actionURL, "subscription_id": subscription.ID,
	})
}
