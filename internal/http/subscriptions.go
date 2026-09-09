package http

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	subscriptionReminderDueKind     = "due"
	subscriptionReminderOverdueKind = "overdue"
	subscriptionReminderCancelKind  = "cancel"
)

type subscriptionResponse struct {
	models.Subscription
	DaysUntilDue int    `json:"days_until_due"`
	DueState     string `json:"due_state"`
}

func (s *Server) createSubscription(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input subscriptionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": fields})
		return
	}
	if input.AccountID != nil {
		if ok, err := userOwnsAccount(userID, *input.AccountID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "account_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": gin.H{"account_id": "must belong to the current user"}})
			return
		}
	}

	subscription := models.Subscription{UserID: userID}
	input.apply(&subscription)
	if err := database.DB.Create(&subscription).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_subscription"})
		return
	}
	_ = database.DB.Preload("Account").First(&subscription, subscription.ID).Error
	c.JSON(http.StatusCreated, buildSubscriptionResponse(subscription, time.Now()))
}

func (s *Server) listSubscriptions(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	query := database.DB.Preload("Account").Where("user_id = ?", userID)
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", "all"))) {
	case "", "all":
	case subscriptionStatusActive, subscriptionStatusPaused, subscriptionStatusCancelled:
		query = query.Where("status = ?", strings.ToLower(strings.TrimSpace(c.Query("status"))))
	default:
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_filters",
			"fields": gin.H{"status": "must be all, active, paused, or cancelled"},
		})
		return
	}

	var subscriptions []models.Subscription
	if err := query.Order("next_due_date asc, name asc").Find(&subscriptions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_subscriptions"})
		return
	}
	c.JSON(http.StatusOK, buildSubscriptionResponses(subscriptions, time.Now()))
}

func (s *Server) updateSubscription(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var subscription models.Subscription
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&subscription).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription not found"})
		return
	}

	var input subscriptionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": fields})
		return
	}
	if input.AccountID != nil {
		if ok, err := userOwnsAccount(userID, *input.AccountID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "account_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": gin.H{"account_id": "must belong to the current user"}})
			return
		}
	}

	input.apply(&subscription)
	if err := database.DB.Save(&subscription).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_subscription"})
		return
	}
	_ = database.DB.Preload("Account").First(&subscription, subscription.ID).Error
	c.JSON(http.StatusOK, buildSubscriptionResponse(subscription, time.Now()))
}

func (s *Server) deleteSubscription(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND subscription_id = ?", userID, id).Delete(&models.SubscriptionReminder{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ? AND user_id = ?", id, userID).Delete(&models.Subscription{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_subscription"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "subscription deleted"})
}

func (s *Server) markSubscriptionPaid(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var input markSubscriptionPaidInput
	if err := c.ShouldBindJSON(&input); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	paidDate := time.Now()
	if strings.TrimSpace(input.PaidDate) != "" {
		parsed, err := parseStrictAPIDate(input.PaidDate)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": gin.H{"paid_date": "must use YYYY-MM-DD"}})
			return
		}
		paidDate = parsed
	}

	var subscription models.Subscription
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&subscription).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription not found"})
		return
	}
	nextDue, err := advanceSubscriptionDueDate(subscription.NextDueDate, subscription.BillingInterval, paidDate)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_subscription", "fields": gin.H{"next_due_date": "must use YYYY-MM-DD"}})
		return
	}

	subscription.LastChargedDate = paidDate.Format("2006-01-02")
	subscription.NextDueDate = nextDue
	subscription.Status = subscriptionStatusActive
	if err := database.DB.Save(&subscription).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_mark_subscription_paid"})
		return
	}
	_ = database.DB.Preload("Account").First(&subscription, subscription.ID).Error
	c.JSON(http.StatusOK, buildSubscriptionResponse(subscription, paidDate))
}

func (s *Server) createSubscriptionReminders(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	created, err := syncSubscriptionReminders(userID, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_subscription_reminders"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"created": created})
}

func syncSubscriptionReminders(userID uint, today time.Time) (int, error) {
	var subscriptions []models.Subscription
	if err := database.DB.
		Where("user_id = ? AND status = ?", userID, subscriptionStatusActive).
		Order("next_due_date asc").
		Find(&subscriptions).Error; err != nil {
		return 0, err
	}

	created := 0
	for _, subscription := range subscriptions {
		if subscription.CancelBeforeDue && strings.TrimSpace(subscription.CancelOnDate) != "" {
			cancelDate, err := parseAPIDate(subscription.CancelOnDate)
			if err == nil && !truncateDate(cancelDate).After(truncateDate(today)) {
				didCreate, createErr := createSubscriptionReminderIfNeeded(database.DB, subscription, subscription.CancelOnDate, subscriptionReminderCancelKind)
				if createErr != nil {
					return created, createErr
				}
				if didCreate {
					created++
				}
			}
		}
		if subscription.AutoPay && (subscription.BillingInterval == subscriptionIntervalDaily || subscription.BillingInterval == subscriptionIntervalBusinessDaily) {
			continue
		}
		dueDate, err := parseAPIDate(subscription.NextDueDate)
		if err != nil {
			continue
		}
		kind := subscriptionReminderKind(subscription, dueDate, today)
		if kind == "" {
			continue
		}
		didCreate, err := createSubscriptionReminderIfNeeded(database.DB, subscription, dueDate.Format("2006-01-02"), kind)
		if err != nil {
			return created, err
		}
		if didCreate {
			created++
		}
	}
	return created, nil
}

func subscriptionReminderKind(subscription models.Subscription, dueDate, today time.Time) string {
	today = truncateDate(today)
	dueDate = truncateDate(dueDate)
	if dueDate.Before(today) {
		return subscriptionReminderOverdueKind
	}
	daysUntil := int(dueDate.Sub(today).Hours() / 24)
	if daysUntil <= subscription.ReminderDays {
		return subscriptionReminderDueKind
	}
	return ""
}

func createSubscriptionReminderIfNeeded(db *gorm.DB, subscription models.Subscription, dueDate, kind string) (bool, error) {
	created := false
	err := db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.SubscriptionReminder{}).
			Where("user_id = ? AND subscription_id = ? AND due_date = ? AND kind = ?",
				subscription.UserID, subscription.ID, dueDate, kind).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}
		title, body := subscriptionReminderCopy(subscription, dueDate, kind)
		notification := models.Notification{
			UserID: subscription.UserID, Type: "subscription." + kind,
			Title: title, Body: body, ActionURL: "/subscriptions",
		}
		if err := tx.Create(&notification).Error; err != nil {
			return err
		}
		reminder := models.SubscriptionReminder{
			UserID: subscription.UserID, SubscriptionID: subscription.ID,
			DueDate: dueDate, Kind: kind, NotificationID: &notification.ID,
		}
		if err := tx.Create(&reminder).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err == nil && created {
		title, body := subscriptionReminderCopy(subscription, dueDate, kind)
		go sendUserPush(database.DB, subscription.UserID, title, body, map[string]any{"action_url": "/subscriptions", "subscription_id": subscription.ID})
	}
	return created, err
}

func subscriptionReminderCopy(subscription models.Subscription, dueDate, kind string) (string, string) {
	name := strings.TrimSpace(subscription.Name)
	if name == "" {
		name = strings.TrimSpace(subscription.Merchant)
	}
	if name == "" {
		name = "Subscription"
	}
	if kind == subscriptionReminderOverdueKind {
		return "Subscription overdue", fmt.Sprintf("%s was due on %s for ₹%s.", name, dueDate, subscription.Amount.String())
	}
	if kind == subscriptionReminderCancelKind {
		return "Cancellation reminder", fmt.Sprintf("You asked to review cancelling %s today. Its next payment is %s for ₹%s.", name, subscription.NextDueDate, subscription.Amount.String())
	}
	return "Subscription due soon", fmt.Sprintf("%s is due on %s for ₹%s.", name, dueDate, subscription.Amount.String())
}

func advanceSubscriptionDueDate(currentDueDate, interval string, paidDate time.Time) (string, error) {
	next, err := parseAPIDate(currentDueDate)
	if err != nil {
		return "", err
	}
	next = addSubscriptionInterval(next, interval)
	paid := truncateDate(paidDate)
	for !next.After(paid) {
		next = addSubscriptionInterval(next, interval)
	}
	return next.Format("2006-01-02"), nil
}

func addSubscriptionInterval(date time.Time, interval string) time.Time {
	switch interval {
	case subscriptionIntervalDaily:
		return date.AddDate(0, 0, 1)
	case subscriptionIntervalBusinessDaily:
		return nextNSEMutualFundDay(date)
	case subscriptionIntervalWeekly:
		return date.AddDate(0, 0, 7)
	case subscriptionIntervalBiweekly:
		return date.AddDate(0, 0, 14)
	case subscriptionIntervalQuarterly:
		return date.AddDate(0, 3, 0)
	case subscriptionIntervalYearly:
		return date.AddDate(1, 0, 0)
	default:
		return date.AddDate(0, 1, 0)
	}
}

// The NSE MF Invest calendar is the closest schedule to platform-based mutual-fund
// orders. Weekends are always excluded; exchange holidays are maintained by year.
func nextNSEMutualFundDay(date time.Time) time.Time {
	next := date.AddDate(0, 0, 1)
	for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday || nseMutualFundHolidays[next.Format("2006-01-02")] {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

var nseMutualFundHolidays = map[string]bool{
	// NSE/NMFTM/71897, including the 2026 NSE MF Invest trading holidays.
	"2026-01-26": true,
	"2026-03-03": true,
	"2026-03-26": true,
	"2026-03-31": true,
	"2026-04-03": true,
	"2026-04-14": true,
	"2026-05-01": true,
	"2026-05-28": true,
	"2026-06-26": true,
	"2026-09-14": true,
	"2026-10-02": true,
	"2026-10-20": true,
	"2026-11-10": true,
	"2026-11-24": true,
	"2026-12-25": true,
}

func buildSubscriptionResponses(subscriptions []models.Subscription, now time.Time) []subscriptionResponse {
	responses := make([]subscriptionResponse, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		responses = append(responses, buildSubscriptionResponse(subscription, now))
	}
	return responses
}

func buildSubscriptionResponse(subscription models.Subscription, now time.Time) subscriptionResponse {
	daysUntilDue := 0
	dueState := "unknown"
	if dueDate, err := parseAPIDate(subscription.NextDueDate); err == nil {
		daysUntilDue = int(truncateDate(dueDate).Sub(truncateDate(now)).Hours() / 24)
		switch {
		case subscription.Status != subscriptionStatusActive:
			dueState = subscription.Status
		case daysUntilDue < 0:
			dueState = "overdue"
		case daysUntilDue <= subscription.ReminderDays:
			dueState = "due_soon"
		default:
			dueState = "scheduled"
		}
	}
	return subscriptionResponse{Subscription: subscription, DaysUntilDue: daysUntilDue, DueState: dueState}
}

func truncateDate(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}
