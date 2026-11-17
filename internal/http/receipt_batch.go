package http

import (
	"log"
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

 1. The failure cooldown counts a batch's failed reads at a third of their
    number (rounded up), so three blurry photos in one run are one strike and
    do not lock the person out of AI for ten minutes.
 2. A batch is capped: at most receiptBatchMaxBills charged reads, and at most
    twice that many reads in all (one retry per bill). The cap is re-checked
    after each read is reserved and tagged, so reads sent together cannot
    slip past it, and with (1) it keeps a batch from becoming a way to send
    unlimited free failures.
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
	query := database.DB.Model(&models.AIUsageEvent{}).
		Where("batch_id = ?", batchID).
		Where("status <> ?", billing.UsageStatusCancelled)
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

// receiptBatchOverCap reports whether a batch has more reads than it may.
//
// `held` counts reads that are charged or still being read (succeeded or
// reserved), so two bills read at the same time both count before either
// finishes. Called twice per read: before reserving, where the read about to be
// made is not yet counted (`includesSelf` false), and again after reserving and
// tagging, where it is. The second check is what makes the cap hold when
// several reads of one batch arrive together — each one sees the others once
// they are tagged, and an extra one is cancelled rather than read.
func receiptBatchOverCap(subject billing.CreditSubject, batchID string, includesSelf bool) (bool, error) {
	var reads, held int64
	if err := receiptBatchEvents(subject, batchID).Count(&reads).Error; err != nil {
		return false, err
	}
	if err := receiptBatchEvents(subject, batchID).
		Where("status IN ?", []string{billing.UsageStatusSucceeded, billing.UsageStatusReserved}).
		Count(&held).Error; err != nil {
		return false, err
	}
	if includesSelf {
		return held > receiptBatchMaxBills || reads > receiptBatchMaxReads, nil
	}
	return held >= receiptBatchMaxBills || reads >= receiptBatchMaxReads, nil
}

func writeReceiptBatchFull(c *gin.Context) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":     "receipt_batch_full",
		"message":   "That scan has read all the bills it can. Start a new scan for more.",
		"max_bills": receiptBatchMaxBills,
	})
}

// rejectIfReceiptBatchFull answers 422 receipt_batch_full when this batch has
// already read its five bills, or used up its retries, before anything is
// reserved.
func rejectIfReceiptBatchFull(c *gin.Context, subject billing.CreditSubject, batchID string) bool {
	if batchID == "" {
		return false
	}
	over, err := receiptBatchOverCap(subject, batchID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "receipt_batch_lookup_failed"})
		return true
	}
	if over {
		writeReceiptBatchFull(c)
	}
	return over
}

// claimReceiptBatchSlot tags a reserved read with its batch and re-checks the
// cap with that read counted. Over the cap, the reservation is cancelled (the
// credits go back) and the read is refused. True means the read may go ahead.
func claimReceiptBatchSlot(c *gin.Context, credits *billing.CreditService, subject billing.CreditSubject, eventID uint, batchID string) bool {
	if batchID == "" || eventID == 0 {
		return true
	}
	if err := database.DB.Model(&models.AIUsageEvent{}).Where("id = ?", eventID).Update("batch_id", batchID).Error; err != nil {
		log.Printf("receipt batch tag failed for usage event %d: %v", eventID, err)
		_, _ = credits.CancelReservation(eventID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "receipt_batch_lookup_failed"})
		return false
	}
	over, err := receiptBatchOverCap(subject, batchID, true)
	if err != nil || over {
		_, _ = credits.CancelReservation(eventID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "receipt_batch_lookup_failed"})
		} else {
			writeReceiptBatchFull(c)
		}
		return false
	}
	return true
}

// receiptBatchFailureWeight is how much one batch's failed reads count toward
// the failure cooldown: three unreadable photos in one scan are one strike,
// not three, but a batch never counts as less than a third of its failures —
// so starting a new batch for every attempt does not buy more free reads than
// a third more.
func receiptBatchFailureWeight(failures int64) int64 {
	if failures <= 0 {
		return 0
	}
	return (failures + 2) / 3
}
