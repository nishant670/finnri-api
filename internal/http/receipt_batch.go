package http

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Several bills scanned in one go (P95–P102).

The app reads each bill through the ordinary /v1/parse/receipt call, two at a
time, and sends the same X-Finnri-Batch id with every read of one run. Nothing
about a single read changes: same price, same "an unreadable bill is free".
The id only does three things:

 1. The failure cooldown counts a batch's failed reads as one failure, so three
    blurry photos in one run do not lock the person out of AI for ten minutes.
 2. A batch is capped: at most receiptBatchMaxBills charged reads, and at most
    twice that many reads in all (one retry per bill). The second limit is what
    keeps (1) from becoming a way to send unlimited free failures.
 3. Admin AI usage can group a run's reads.
*/

// receiptBatchMaxBills is how many bills one scan may read. The user set it to
// five on 2026-11-13, to be raised in a later release; the app holds the same
// number in lib/bill-batch.ts.
const receiptBatchMaxBills = 5

// receiptBatchMaxReads allows one retry per bill.
const receiptBatchMaxReads = receiptBatchMaxBills * 2

const receiptBatchHeader = "X-Finnri-Batch"

var receiptBatchIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// receiptBatchID reads the batch header. An absent header is a read on its own
// (""). A present header that is not a plausible id is refused rather than
// ignored, so a client bug cannot quietly turn the batch rules off.
func receiptBatchID(c *gin.Context) (string, bool) {
	raw := strings.TrimSpace(c.GetHeader(receiptBatchHeader))
	if raw == "" {
		return "", true
	}
	if !receiptBatchIDPattern.MatchString(raw) {
		return "", false
	}
	return raw, true
}

func receiptBatchEvents(subject billing.CreditSubject, batchID string) *gorm.DB {
	query := database.DB.Model(&models.AIUsageEvent{}).Where("batch_id = ?", batchID)
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

// rejectIfReceiptBatchFull answers 422 receipt_batch_full when this batch has
// already read its five bills, or used up its retries.
func rejectIfReceiptBatchFull(c *gin.Context, subject billing.CreditSubject, batchID string) bool {
	if batchID == "" {
		return false
	}
	var reads, charged int64
	if err := receiptBatchEvents(subject, batchID).Count(&reads).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "receipt_batch_lookup_failed"})
		return true
	}
	if err := receiptBatchEvents(subject, batchID).
		Where("status = ?", billing.UsageStatusSucceeded).
		Count(&charged).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "receipt_batch_lookup_failed"})
		return true
	}
	if charged < receiptBatchMaxBills && reads < receiptBatchMaxReads {
		return false
	}
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":     "receipt_batch_full",
		"message":   "That scan has read all the bills it can. Start a new scan for more.",
		"max_bills": receiptBatchMaxBills,
	})
	return true
}

// tagReceiptBatch records the batch on a read's usage event.
func tagReceiptBatch(eventID uint, batchID string) {
	if batchID == "" || eventID == 0 {
		return
	}
	_ = database.DB.Model(&models.AIUsageEvent{}).Where("id = ?", eventID).Update("batch_id", batchID).Error
}
