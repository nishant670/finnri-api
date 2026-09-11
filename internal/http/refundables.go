package http

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	refundStatusPending    = "pending"
	refundStatusReceived   = "received"
	refundStatusWrittenOff = "written_off"
	refundReminderType     = "refund.due"
)

// StartRefundAutomation raises reminders for refundable money the user chose
// to be nudged about. Idempotency is anchored to the entry action URL, so a
// minute ticker can safely revisit overdue rows forever.
func StartRefundAutomation(_ *config.Config) {
	go func() {
		run := func() {
			if _, err := syncRefundReminders(time.Now()); err != nil {
				log.Printf("refund reminder sync failed: %v", err)
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

func syncRefundReminders(now time.Time) (int, error) {
	var due []models.Entry
	if err := database.DB.
		Where("refund_status = ? AND refundable_amount IS NOT NULL AND refund_reminder_at IS NOT NULL AND refund_reminder_at <= ?", refundStatusPending, now).
		Order("refund_reminder_at ASC").
		Find(&due).Error; err != nil {
		return 0, err
	}

	created := 0
	for _, entry := range due {
		actionURL := fmt.Sprintf("/entry/%d", entry.ID)
		notification := models.Notification{
			UserID:    entry.UserID,
			Type:      refundReminderType,
			Title:     "Refund expected",
			Body:      fmt.Sprintf("₹%s from %s was expected back. Mark it received or written off.", entry.RefundableAmount.String(), strings.TrimSpace(entry.Title)),
			ActionURL: actionURL,
		}
		didCreate := false
		err := database.DB.Transaction(func(tx *gorm.DB) error {
			var existing models.Notification
			if err := tx.Where("user_id = ? AND type = ? AND action_url = ?", entry.UserID, refundReminderType, actionURL).
				First(&existing).Error; err == nil {
				return nil
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&notification)
			didCreate = result.RowsAffected == 1
			return result.Error
		})
		if err != nil {
			return created, err
		}
		if didCreate {
			created++
			go sendUserPush(database.DB, entry.UserID, notification.Title, notification.Body, map[string]any{
				"action_url": actionURL,
				"entry_id":   entry.ID,
			})
		}
	}
	return created, nil
}

func (s *Server) listRefundables(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	status := strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", refundStatusPending)))
	if status != "all" && status != refundStatusPending && status != refundStatusReceived && status != refundStatusWrittenOff {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": gin.H{
			"status": "must be pending, received, written_off, or all",
		}})
		return
	}
	query := ownedEntries(database.DB.Preload("Account"), userID).Where("refundable_amount IS NOT NULL")
	if status != "all" {
		query = query.Where("refund_status = ?", status)
	}
	var entries []models.Entry
	if err := query.Order("refund_expected_on ASC, id ASC").Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_refundables"})
		return
	}
	c.JSON(http.StatusOK, entries)
}

type refundStatusInput struct {
	Status string `json:"status"`
}

func (s *Server) updateRefundStatus(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	entryID, err := parsePositiveUint(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var input refundStatusInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if status != refundStatusPending && status != refundStatusReceived && status != refundStatusWrittenOff {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_refund_status", "fields": gin.H{
			"status": "must be pending, received, or written_off",
		}})
		return
	}

	var entry models.Entry
	if err := ownedEntries(database.DB, userID).
		Where("id = ? AND refundable_amount IS NOT NULL", entryID).
		First(&entry).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "refundable entry not found"})
		return
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&entry).Update("refund_status", status).Error; err != nil {
			return err
		}
		if status != refundStatusPending {
			now := time.Now().UTC()
			return tx.Model(&models.Notification{}).
				Where("user_id = ? AND type = ? AND action_url = ?", userID, refundReminderType, fmt.Sprintf("/entry/%d", entry.ID)).
				Where("read_at IS NULL").Update("read_at", now).Error
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_refund_status"})
		return
	}
	entry.RefundStatus = &status
	_ = database.DB.Preload("Account").First(&entry, entry.ID).Error
	c.JSON(http.StatusOK, entry)
}
