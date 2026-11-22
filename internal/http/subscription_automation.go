package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	subscriptionAutopayNotificationType = "subscription.autopay"
)

// expoPushURL is Expo's push service. A variable so tests can point it at a
// local server.
var expoPushURL = "https://exp.host/--/api/v2/push/send"

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
			updates := map[string]any{
				"last_charged_date": subscription.LastChargedDate,
				"next_due_date":     subscription.NextDueDate,
			}
			finished := false
			if didCreate && subscription.TotalInstalments > 0 {
				subscription.InstalmentsPaid++
				updates["instalments_paid"] = subscription.InstalmentsPaid
				// The last instalment closes the schedule so nothing is
				// generated past the end of the loan.
				if subscription.InstalmentsPaid >= subscription.TotalInstalments {
					updates["status"] = subscriptionStatusCancelled
					finished = true
				}
			}
			if err := database.DB.Model(&models.Subscription{}).Where("id = ?", subscription.ID).Updates(updates).Error; err != nil {
				return created, err
			}
			if finished {
				notifyRecurringCompleted(*subscription)
				break
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

		entry := newSubscriptionEntry(*subscription, due)
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
			UserID: subscription.UserID, Type: subscriptionAutopayNotificationType,
			Title:     recurringAutopayTitle(subscription.Kind),
			Body:      recurringAutopayBody(*subscription),
			ActionURL: fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID),
			// The charge and its day, so a stack of autopays can list each one
			// with its figure instead of re-reading the sentence (P48).
			Amount:  moneyPointer(subscription.Amount),
			DueDate: datePointer(due),
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

// newSubscriptionEntry is the transaction one payment of a recurring item
// writes, whether autopay added it or the user logged it.
func newSubscriptionEntry(subscription models.Subscription, due string) models.Entry {
	entryType := recurringEntryType(subscription.Kind)
	tag := strings.TrimSpace(subscription.TransactionTag)
	if tag == "" && entryType == "expense" {
		tag = "Subscription"
	}
	tags := models.StringArray{tag}
	if entryType == "income" {
		// Income carries no tags anywhere in the app.
		tag = ""
		tags = models.StringArray{}
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
	category, ok := categoryForSave(subscription.Category, entryType)
	if !ok {
		category = defaultCategory
	}
	idempotencyKey := fmt.Sprintf("subscription:%d:%s", subscription.ID, due)
	return models.Entry{
		UserID: subscription.UserID, AccountID: subscription.AccountID,
		Title: subscription.Name, Type: entryType, Amount: subscription.Amount,
		Currency: "INR", Source: models.EntrySourceRecurring, Mode: normalizedOccurrencePaymentMode(subscription),
		Category: category, Merchant: subscription.Merchant, PurposeType: purpose, Tag: tag,
		Tags: tags, Notes: subscription.Notes, Date: due,
		Time: "9:00 AM", IdempotencyKey: &idempotencyKey,
	}
}

// logSubscriptionPayment records a payment the user says is already made: a
// confirmed transaction, filed against the schedule the way an autopay one is,
// so autopay can never add the same date again. When it is the payment that
// was due, the schedule moves past it, and a loan counts the EMI, exactly as
// marking it paid does. An earlier payment — last month's, while the next is
// still to come — leaves the schedule alone.
func logSubscriptionPayment(tx *gorm.DB, subscription *models.Subscription, paid, now time.Time) (*models.Entry, error) {
	due := paid.Format("2006-01-02")
	entry, err := recordSubscriptionPayment(tx, *subscription, due, due, now)
	if err != nil {
		return nil, err
	}

	subscription.LastChargedDate = due
	updates := map[string]any{"last_charged_date": due}
	if nextDue, err := parseAPIDate(subscription.NextDueDate); err == nil && !paid.Before(nextDue) {
		next, err := advanceSubscriptionDueDate(subscription.NextDueDate, subscription.BillingInterval, paid)
		if err != nil {
			return entry, err
		}
		subscription.NextDueDate = next
		updates["next_due_date"] = next
		if subscription.TotalInstalments > 0 {
			subscription.InstalmentsPaid++
			updates["instalments_paid"] = subscription.InstalmentsPaid
			if subscription.InstalmentsPaid >= subscription.TotalInstalments {
				subscription.Status = subscriptionStatusCancelled
				updates["status"] = subscriptionStatusCancelled
			}
		}
	}
	return entry, tx.Model(&models.Subscription{}).Where("id = ?", subscription.ID).Updates(updates).Error
}

// recordSubscriptionPayment writes one payment the user made: a confirmed
// entry dated when it was paid, and the occurrence that files it against the
// due date it settled. That occurrence is what keeps a payment from being
// written twice — autopay, logging and marking paid all check it — so when
// the due date already has one, nothing is written and the entry is nil.
func recordSubscriptionPayment(tx *gorm.DB, subscription models.Subscription, paidOn, dueDate string, now time.Time) (*models.Entry, error) {
	var existing int64
	if err := tx.Model(&models.SubscriptionOccurrence{}).
		Where("user_id = ? AND subscription_id = ? AND due_date = ?", subscription.UserID, subscription.ID, dueDate).
		Count(&existing).Error; err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, nil
	}
	entry := newSubscriptionEntry(subscription, dueDate)
	entry.Date = paidOn
	if err := tx.Create(&entry).Error; err != nil {
		return nil, err
	}
	confirmedAt := now.UTC()
	occurrence := models.SubscriptionOccurrence{
		UserID: subscription.UserID, SubscriptionID: subscription.ID, EntryID: entry.ID,
		DueDate: dueDate, Status: subscriptionOccurrenceConfirmed, ConfirmedAt: &confirmedAt,
	}
	if err := tx.Create(&occurrence).Error; err != nil {
		return nil, err
	}
	return &entry, nil
}

// deleteSubscriptionOccurrences drops the occurrences a delete would otherwise
// trip over, along with the autopay notification each one raised. The
// database's own foreign keys cannot be trusted to cascade: tables AutoMigrate
// created first carry GORM's constraint, which restricts, so a recurring
// payment's entry — and a schedule that has logged one — refused to delete.
// The notification goes too because its Confirm and Change buttons act on the
// occurrence, and would only fail once it is gone.
func deleteSubscriptionOccurrences(tx *gorm.DB, userID uint, column string, id uint) error {
	var notificationIDs []uint
	if err := tx.Model(&models.SubscriptionOccurrence{}).
		Where("user_id = ? AND "+column+" = ? AND notification_id IS NOT NULL", userID, id).
		Pluck("notification_id", &notificationIDs).Error; err != nil {
		return err
	}
	if err := tx.Where("user_id = ? AND "+column+" = ?", userID, id).
		Delete(&models.SubscriptionOccurrence{}).Error; err != nil {
		return err
	}
	if len(notificationIDs) == 0 {
		return nil
	}
	return tx.Where("user_id = ? AND id IN ?", userID, notificationIDs).
		Delete(&models.Notification{}).Error
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
		// Confirm and Fix it both answer the notification, so it is done, not
		// just read. Marking it read alone left "Autopay transaction added" in
		// Needs you with its buttons still on it (2026-11-11).
		return tx.Model(&models.Notification{}).
			Where("user_id = ? AND resolved_at IS NULL", userID).
			Where("id = ? OR action_url = ?", occurrenceNotificationID(occurrence), fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID)).
			Updates(map[string]any{"resolved_at": now, "read_at": gorm.Expr("COALESCE(read_at, ?)", now)}).Error
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

// occurrenceNotificationID is 0 when the occurrence never recorded its
// notification; no row has id 0, so the action_url match does the work then.
func occurrenceNotificationID(occurrence models.SubscriptionOccurrence) uint {
	if occurrence.NotificationID == nil {
		return 0
	}
	return *occurrence.NotificationID
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

// recurringAutopayBody is the notification line for a payment autopay wrote:
// money in names where it landed, money out where it came from.
func recurringAutopayBody(subscription models.Subscription) string {
	if subscription.Kind == recurringKindIncome {
		return fmt.Sprintf("Added %s of %s to %s. Confirm it arrived, or open it to change the amount.", subscription.Name, rupeesForCopy(subscription.Amount), occurrenceAccountLabel(subscription))
	}
	return fmt.Sprintf("Added %s for %s from %s. Confirm it or open it to make changes.", subscription.Name, rupeesForCopy(subscription.Amount), occurrenceAccountLabel(subscription))
}

func sendOccurrencePush(occurrence models.SubscriptionOccurrence) {
	title := "Autopay transaction added"
	if strings.EqualFold(occurrence.Entry.Type, "income") {
		title = "Income added"
	}
	sendUserPush(database.DB, occurrence.UserID, title, fmt.Sprintf("%s was added for %s. Confirm or review it in Finnri.", occurrence.Entry.Title, rupeesForCopy(occurrence.Entry.Amount)), map[string]any{
		"action_url": fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID), "type": subscriptionAutopayNotificationType, "occurrence_id": occurrence.ID, "entry_id": occurrence.EntryID,
	})
}

// expoPushTickets is Expo's answer to a send: one ticket per message, in the
// order sent, or request-level errors when the whole send was refused.
type expoPushTickets struct {
	Data []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Details struct {
			Error string `json:"error"`
		} `json:"details"`
	} `json:"data"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// sendUserPush sends one push to each of the user's registered phones.
//
// It used to fail without a trace: no device, a refused send and a rejected
// token all looked the same as success, which is how Android pushes went
// missing unnoticed. Each of those now leaves a log line, and a token Expo
// says is no longer registered (the app was uninstalled, say) is switched
// off so it isn't tried again. What happens after Expo accepts a message is
// read later, in checkPushReceipts.
//
// data should carry "type": it picks the Android category here, and the app
// uses it to choose the screen a tap opens.
func sendUserPush(db *gorm.DB, userID uint, title, body string, data map[string]any) {
	if db == nil {
		return
	}
	var devices []models.PushDevice
	if err := db.Where("user_id = ? AND active = ?", userID, true).Find(&devices).Error; err != nil {
		log.Printf("[WARN] push skipped user=%d: loading devices failed: %v", userID, err)
		return
	}
	if len(devices) == 0 {
		log.Printf("[INFO] push skipped user=%d: no registered device", userID)
		return
	}
	messages := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		messages = append(messages, map[string]any{
			"to": device.Token, "sound": "default", "title": title, "body": body, "data": data,
			// Shown at once rather than batched by Android's power saving.
			"priority":  "high",
			"channelId": pushChannelFor(data),
		})
	}
	payload, _ := json.Marshal(messages)
	req, err := http.NewRequest(http.MethodPost, expoPushURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] push send failed user=%d: %v", userID, err)
		return
	}
	defer response.Body.Close()

	var tickets expoPushTickets
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tickets)
	if response.StatusCode >= http.StatusMultipleChoices || len(tickets.Errors) > 0 {
		log.Printf("[WARN] push refused user=%d status=%d errors=%v", userID, response.StatusCode, tickets.Errors)
		return
	}
	pending := make(map[string]models.PushDevice, len(devices))
	for index, ticket := range tickets.Data {
		if index >= len(devices) {
			break
		}
		device := devices[index]
		if ticket.Status == "ok" {
			if ticket.ID != "" {
				pending[ticket.ID] = device
			}
			continue
		}
		log.Printf("[WARN] push rejected user=%d device=%d platform=%s: %s %s",
			userID, device.ID, device.Platform, ticket.Details.Error, ticket.Message)
		deactivateGonePushDevice(db, device, ticket.Details.Error)
	}
	schedulePushReceiptCheck(db, userID, pending)
}
