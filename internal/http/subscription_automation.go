package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	subscriptionOccurrencePending   = "pending"
	subscriptionOccurrenceConfirmed = "confirmed"
	subscriptionOccurrenceReverted  = "reverted"
	expoPushURL                     = "https://exp.host/--/api/v2/push/send"
)

func StartSubscriptionAutomation(_ *config.Config) {
	go func() {
		run := func() {
			if _, err := syncAllSubscriptionAutomation(time.Now()); err != nil {
				log.Printf("subscription automation sync failed: %v", err)
			}
			if _, err := syncPaidSubscriptionCreditTranches(time.Now().UTC()); err != nil {
				log.Printf("paid subscription credit tranche sync failed: %v", err)
			}
			if _, err := billing.NewCreditService(database.DB).ExpireCredits(); err != nil {
				log.Printf("credit expiry sync failed: %v", err)
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

func syncPaidSubscriptionCreditTranches(now time.Time) (int, error) {
	var subscriptions []models.UserSubscription
	if err := database.DB.Preload("Plan").
		Where("status = ? AND current_period_start <= ?", "active", now).
		Find(&subscriptions).Error; err != nil {
		return 0, err
	}

	service := billing.NewCreditService(database.DB)
	created := 0
	for _, subscription := range subscriptions {
		count, err := service.SyncSubscriptionTranches(subscription, now)
		if err != nil {
			return created, err
		}
		created += count
	}
	return created, nil
}

func syncAllSubscriptionAutomation(now time.Time) (int, error) {
	var userIDs []uint
	if err := database.DB.Model(&models.Subscription{}).
		Where("status = ? AND autopay = ? AND next_due_date <= ?", subscriptionStatusActive, true, truncateDate(now).Format("2006-01-02")).
		Distinct("user_id").Pluck("user_id", &userIDs).Error; err != nil {
		return 0, err
	}
	total := 0
	for _, userID := range userIDs {
		created, err := syncSubscriptionAutomation(userID, now)
		if err != nil {
			return total, err
		}
		total += len(created)
	}
	return total, nil
}

func syncSubscriptionAutomation(userID uint, today time.Time) ([]models.SubscriptionOccurrence, error) {
	var subscriptions []models.Subscription
	if err := database.DB.Preload("Account").
		Where("user_id = ? AND status = ? AND autopay = ? AND next_due_date <= ?", userID, subscriptionStatusActive, true, truncateDate(today).Format("2006-01-02")).
		Order("next_due_date asc").Find(&subscriptions).Error; err != nil {
		return nil, err
	}

	created := []models.SubscriptionOccurrence{}
	for index := range subscriptions {
		subscription := &subscriptions[index]
		for attempts := 0; attempts < 366; attempts++ {
			dueDate, err := parseAPIDate(subscription.NextDueDate)
			if err != nil || dueDate.After(truncateDate(today)) {
				break
			}
			occurrence, didCreate, err := createSubscriptionOccurrence(subscription, dueDate)
			if err != nil {
				return created, err
			}
			if didCreate {
				created = append(created, occurrence)
				go sendOccurrencePush(occurrence)
			}
			subscription.LastChargedDate = dueDate.Format("2006-01-02")
			subscription.NextDueDate = addSubscriptionInterval(dueDate, subscription.BillingInterval).Format("2006-01-02")
			if err := database.DB.Model(&models.Subscription{}).Where("id = ?", subscription.ID).Updates(map[string]any{
				"last_charged_date": subscription.LastChargedDate,
				"next_due_date":     subscription.NextDueDate,
			}).Error; err != nil {
				return created, err
			}
		}
	}
	return created, nil
}

func createSubscriptionOccurrence(subscription *models.Subscription, dueDate time.Time) (models.SubscriptionOccurrence, bool, error) {
	var occurrence models.SubscriptionOccurrence
	due := dueDate.Format("2006-01-02")
	didCreate := false
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Preload("Entry").Preload("Subscription").
			Where("user_id = ? AND subscription_id = ? AND due_date = ?", subscription.UserID, subscription.ID, due).
			First(&occurrence).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		tag := strings.TrimSpace(subscription.TransactionTag)
		if tag == "" {
			tag = "Subscription"
		}
		purpose := strings.TrimSpace(subscription.PurposeType)
		if purpose == "" {
			purpose = "normal_spend"
		}
		// Autopay writes an entry directly, so it never passes through the entry
		// input validation that canonicalizes categories. Route it through the
		// same resolver, otherwise a subscription filed under the old
		// subscription-only vocabulary ("Cloud", "Membership") would put that
		// value straight into the ledger and fragment the category rollups.
		category, ok := categoryForSave(subscription.Category, "expense")
		if !ok {
			category = defaultCategory
		}
		mode := normalizedOccurrencePaymentMode(*subscription)
		idempotencyKey := fmt.Sprintf("subscription:%d:%s", subscription.ID, due)
		entry := models.Entry{
			UserID: subscription.UserID, AccountID: subscription.AccountID,
			Title: subscription.Name, Type: "expense", Amount: subscription.Amount,
			Currency: "INR", Source: "recurring", Mode: mode, Category: category,
			Merchant: subscription.Merchant, PurposeType: purpose, Tag: tag,
			Tags: models.StringArray{tag}, Notes: subscription.Notes, Date: due,
			Time: "9:00 AM", IdempotencyKey: &idempotencyKey,
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}

		occurrence = models.SubscriptionOccurrence{
			UserID: subscription.UserID, SubscriptionID: subscription.ID,
			EntryID: entry.ID, DueDate: due, Status: subscriptionOccurrencePending,
		}
		if err := tx.Create(&occurrence).Error; err != nil {
			return err
		}
		notification := models.Notification{
			UserID: subscription.UserID, Type: "subscription.autopay",
			Title:     "Autopay transaction added",
			Body:      fmt.Sprintf("Added %s for ₹%s from %s. Confirm it or open it to make changes.", subscription.Name, subscription.Amount.String(), occurrenceAccountLabel(*subscription)),
			ActionURL: fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID),
		}
		if err := tx.Create(&notification).Error; err != nil {
			return err
		}
		if err := tx.Model(&occurrence).Update("notification_id", notification.ID).Error; err != nil {
			return err
		}
		occurrence.NotificationID = &notification.ID
		occurrence.Entry = entry
		occurrence.Subscription = *subscription
		didCreate = true
		return nil
	})
	if err == nil && didCreate {
		_ = maybeCreateBudgetAlertsForEntry(occurrence.Entry)
	}
	return occurrence, didCreate, err
}

func normalizedOccurrencePaymentMode(subscription models.Subscription) string {
	if mode := normalizeSubscriptionPaymentMode(subscription.PaymentMode); mode != "" {
		return mode
	}
	if subscription.Account != nil {
		switch subscription.Account.Type {
		case "bank", "debit_card":
			return "Bank Account"
		case "upi":
			return "UPI"
		case "credit_card":
			return "Credit Card"
		case "wallet":
			return "Wallets"
		}
	}
	return "Cash"
}

func occurrenceAccountLabel(subscription models.Subscription) string {
	if subscription.Account != nil && strings.TrimSpace(subscription.Account.Name) != "" {
		return subscription.Account.Name
	}
	return normalizedOccurrencePaymentMode(subscription)
}

func (s *Server) syncSubscriptionAutomationNow(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	created, err := syncSubscriptionAutomation(userID, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_sync_subscription_automation"})
		return
	}
	reminders, err := syncSubscriptionReminders(userID, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_subscription_reminders"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"created": len(created), "reminders_created": reminders})
}

func (s *Server) listSubscriptionOccurrences(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	status := strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", subscriptionOccurrencePending)))
	if status != "all" && status != subscriptionOccurrencePending && status != subscriptionOccurrenceConfirmed && status != subscriptionOccurrenceReverted {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": gin.H{"status": "must be all, pending, confirmed, or reverted"}})
		return
	}
	query := database.DB.Preload("Entry.Account").Preload("Subscription.Account").Where("user_id = ?", userID)
	if status != "all" {
		query = query.Where("status = ?", status)
	}
	var occurrences []models.SubscriptionOccurrence
	if err := query.Order("created_at desc").Find(&occurrences).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_subscription_occurrences"})
		return
	}
	c.JSON(http.StatusOK, occurrences)
}

func (s *Server) updateSubscriptionOccurrenceStatus(c *gin.Context, status string) {
	userID := c.MustGet("userID").(uint)
	id, err := parsePositiveUint(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var occurrence models.SubscriptionOccurrence
	if err := database.DB.Preload("Entry.Account").Preload("Subscription.Account").Where("id = ? AND user_id = ?", id, userID).First(&occurrence).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription occurrence not found"})
		return
	}
	now := time.Now().UTC()
	updates := map[string]any{"status": status}
	if status == subscriptionOccurrenceConfirmed {
		updates["confirmed_at"] = now
	} else {
		updates["reverted_at"] = now
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&occurrence).Updates(updates).Error; err != nil {
			return err
		}
		if occurrence.NotificationID != nil {
			return tx.Model(&models.Notification{}).Where("id = ? AND user_id = ?", *occurrence.NotificationID, userID).Update("read_at", now).Error
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_subscription_occurrence"})
		return
	}
	occurrence.Status = status
	if status == subscriptionOccurrenceConfirmed {
		occurrence.ConfirmedAt = &now
	} else {
		occurrence.RevertedAt = &now
	}
	c.JSON(http.StatusOK, occurrence)
}

func (s *Server) confirmSubscriptionOccurrence(c *gin.Context) {
	s.updateSubscriptionOccurrenceStatus(c, subscriptionOccurrenceConfirmed)
}

func (s *Server) revertSubscriptionOccurrence(c *gin.Context) {
	s.updateSubscriptionOccurrenceStatus(c, subscriptionOccurrenceReverted)
}

func parsePositiveUint(value string) (uint, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, errors.New("invalid id")
	}
	return uint(parsed), nil
}

func sendOccurrencePush(occurrence models.SubscriptionOccurrence) {
	sendUserPush(database.DB, occurrence.UserID, "Autopay transaction added", fmt.Sprintf("%s was added for ₹%s. Confirm or review it in Finnri.", occurrence.Entry.Title, occurrence.Entry.Amount.String()), map[string]any{
		"action_url": fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID), "occurrence_id": occurrence.ID, "entry_id": occurrence.EntryID,
	})
}

func sendUserPush(db *gorm.DB, userID uint, title, body string, data map[string]any) {
	if db == nil {
		return
	}
	var devices []models.PushDevice
	if err := db.Where("user_id = ? AND active = ?", userID, true).Find(&devices).Error; err != nil || len(devices) == 0 {
		return
	}
	messages := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		messages = append(messages, map[string]any{
			"to": device.Token, "sound": "default", "title": title, "body": body, "data": data,
		})
	}
	payload, _ := json.Marshal(messages)
	req, err := http.NewRequest(http.MethodPost, expoPushURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	if response, err := client.Do(req); err == nil {
		_ = response.Body.Close()
	}
}
