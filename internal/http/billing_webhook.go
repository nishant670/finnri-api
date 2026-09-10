package http

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
	"finnri/internal/payments"
)

// maxWebhookBodyBytes caps what will be read from an unauthenticated endpoint.
// Razorpay events run to a few kilobytes; anything approaching this is not one.
const maxWebhookBodyBytes = 512 * 1024

// handleBillingWebhook is the only thing in this codebase permitted to move a
// subscription to active or write a credit grant. The checkout endpoint
// creates an order and nothing more, and the browser is never believed — it
// can be driven by anyone.
//
// Three properties have to hold together:
//
//   - Signed. The HMAC is computed over the exact bytes received. Re-encoding
//     parsed JSON reorders keys and changes the digest, so the raw body is read
//     once and passed around as bytes.
//   - Idempotent. The event id is inserted under a unique index before the
//     event is acted on, so a redelivery — which providers do routinely, and
//     which an attacker can force by replaying a body whose signature still
//     verifies — loses the insert and grants nothing.
//   - Retryable. A failure to process is recorded and answered with a 5xx so
//     the provider retries, and the recorded failure is allowed to be
//     reprocessed rather than mistaken for a duplicate.
func (s *Server) handleBillingWebhook(c *gin.Context) {
	config := s.razorpayConfig()
	if !config.Configured() {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "payment_webhook_not_configured"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBodyBytes))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable_body"})
		return
	}

	signature := c.GetHeader("X-Razorpay-Signature")
	eventID := strings.TrimSpace(c.GetHeader("X-Razorpay-Event-Id"))
	payloadHash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(payloadHash[:])

	if !payments.VerifyWebhookSignature(config.WebhookSecret, body, signature) {
		// Recorded, not silently dropped: a run of these is the only visible
		// sign of either a rotated secret or somebody probing the endpoint.
		recordRejectedWebhook(eventID, hashHex, requestClientIP(c))
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_signature"})
		return
	}

	envelope, err := payments.ParseWebhook(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payload"})
		return
	}
	if eventID == "" {
		// Razorpay always sends the header, but a body hash is a sound
		// fallback identity: an identical replay dedupes, and a genuinely
		// different event hashes differently.
		eventID = "sha256:" + hashHex
	}

	event, proceed, err := claimWebhookEvent(eventID, envelope.Event, hashHex)
	if err != nil {
		log.Printf("[ERROR] webhook dedupe failed event=%s: %v", eventID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook_dedupe_failed"})
		return
	}
	if !proceed {
		// Already handled. 200 stops the provider retrying something that is
		// done, which is the whole point of the ledger.
		c.JSON(http.StatusOK, gin.H{"status": "duplicate", "event": envelope.Event})
		return
	}

	status, detail, err := s.applyWebhookEvent(envelope)
	if err != nil {
		// Leave the row as failed so the retry is allowed to reprocess it,
		// and answer 5xx so the provider actually sends one.
		finishWebhookEvent(event, models.WebhookStatusFailed, err.Error())
		log.Printf("[ERROR] webhook processing failed event=%s type=%s: %v", eventID, envelope.Event, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook_processing_failed"})
		return
	}

	finishWebhookEvent(event, status, detail)
	c.JSON(http.StatusOK, gin.H{"status": status, "event": envelope.Event})
}

// claimWebhookEvent takes ownership of an event id, returning false when the
// event has already been handled.
//
// The insert is the lock: two concurrent deliveries race on the unique index
// and exactly one wins. A row left behind by an earlier failure is reclaimed,
// because a failed attempt is not a completed one.
func claimWebhookEvent(eventID, eventType, payloadHash string) (models.PaymentWebhookEvent, bool, error) {
	event := models.PaymentWebhookEvent{
		Provider:    payments.ProviderRazorpay,
		EventID:     eventID,
		EventType:   eventType,
		PayloadHash: payloadHash,
		Status:      "received",
	}
	err := database.DB.Create(&event).Error
	if err == nil {
		return event, true, nil
	}

	var existing models.PaymentWebhookEvent
	lookupErr := database.DB.
		Where("provider = ? AND event_id = ?", payments.ProviderRazorpay, eventID).
		First(&existing).Error
	if lookupErr != nil {
		// The insert failed for a reason other than the unique index.
		return models.PaymentWebhookEvent{}, false, err
	}
	if existing.Status == models.WebhookStatusFailed {
		return existing, true, nil
	}
	return existing, false, nil
}

func finishWebhookEvent(event models.PaymentWebhookEvent, status, detail string) {
	now := time.Now().UTC()
	if len(detail) > 255 {
		detail = detail[:255]
	}
	if err := database.DB.Model(&models.PaymentWebhookEvent{}).
		Where("id = ?", event.ID).
		Updates(map[string]any{
			"status":       status,
			"detail":       detail,
			"processed_at": now,
			"updated_at":   now,
		}).Error; err != nil {
		log.Printf("[ERROR] could not finalise webhook event %d: %v", event.ID, err)
	}
}

func recordRejectedWebhook(eventID, payloadHash, clientIP string) {
	if eventID == "" {
		eventID = "unsigned:" + payloadHash
	}
	event := models.PaymentWebhookEvent{
		Provider:    payments.ProviderRazorpay,
		EventID:     eventID,
		PayloadHash: payloadHash,
		Status:      models.WebhookStatusRejected,
		Detail:      "signature verification failed",
	}
	// A repeated replay collides on the unique index; that is fine, the first
	// rejection is already on record.
	_ = database.DB.Create(&event).Error
	log.Printf("[WARN] rejected billing webhook with bad signature from %s", clientIP)
}

// applyWebhookEvent turns a verified event into ledger changes. It returns the
// status to record, a short human detail, and an error only when the event
// should be retried.
func (s *Server) applyWebhookEvent(envelope payments.WebhookEnvelope) (string, string, error) {
	switch envelope.Event {
	case "payment.captured", "order.paid":
		return s.applyPaymentCaptured(envelope.Payload.Payment.Entity)
	case "payment.failed":
		return applyPaymentFailed(envelope.Payload.Payment.Entity)
	case "refund.created", "refund.processed":
		return applyRefund(envelope.Payload.Refund.Entity)
	default:
		// Razorpay lets an account subscribe to more events than this handles.
		// An unknown event is not an error — acknowledging it stops a pointless
		// retry loop — but it must not be mistaken for something acted on.
		return models.WebhookStatusIgnored, "unhandled event " + envelope.Event, nil
	}
}

// applyPaymentCaptured is the money-in path: it marks the payment captured,
// grants the subscription period, and issues the credits that come with it.
func (s *Server) applyPaymentCaptured(entity payments.WebhookPayment) (string, string, error) {
	if entity.OrderID == "" {
		return models.WebhookStatusIgnored, "capture event carried no order id", nil
	}

	var payment models.Payment
	err := database.DB.Where("provider = ? AND provider_order_id = ?", payments.ProviderRazorpay, entity.OrderID).
		First(&payment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// An order this deployment never created. Retrying will not conjure
		// one, so acknowledge and record it rather than looping forever.
		return models.WebhookStatusIgnored, "no local payment for order " + entity.OrderID, nil
	}
	if err != nil {
		return "", "", err
	}
	if payment.Status == models.PaymentStatusCaptured {
		return models.WebhookStatusIgnored, "payment already captured", nil
	}

	// Defence in depth. Razorpay enforces the order amount at its end, but a
	// capture for less than the plan's price must never buy the plan.
	if entity.AmountMinor > 0 && entity.AmountMinor < payment.AmountMinor {
		log.Printf("[WARN] capture underpays order=%s paid=%d expected=%d", entity.OrderID, entity.AmountMinor, payment.AmountMinor)
		return models.WebhookStatusRejected, fmt.Sprintf("amount mismatch: paid %d, expected %d", entity.AmountMinor, payment.AmountMinor), nil
	}

	var plan models.Plan
	if err := database.DB.First(&plan, payment.PlanID).Error; err != nil {
		return "", "", err
	}

	now := time.Now().UTC()
	var subscription models.UserSubscription
	err = database.DB.Transaction(func(tx *gorm.DB) error {
		created, err := activateSubscriptionForPayment(tx, &payment, plan, now)
		if err != nil {
			return err
		}
		subscription = created

		return tx.Model(&models.Payment{}).Where("id = ?", payment.ID).Updates(map[string]any{
			"status":              models.PaymentStatusCaptured,
			"provider_payment_id": entity.ID,
			"method":              entity.Method,
			"subscription_id":     created.ID,
			"captured_at":         now,
			"updated_at":          now,
		}).Error
	})
	if err != nil {
		return "", "", err
	}

	// Credits are granted outside the activation transaction on purpose: the
	// credit service opens transactions of its own, and its grants are keyed
	// idempotently on (subscription, valid_from), so a retry after a failure
	// here re-grants nothing.
	subscription.Plan = plan
	if err := grantCreditsForSubscription(billing.NewCreditService(database.DB), subscription, plan, now); err != nil {
		return "", "", err
	}

	log.Printf("[INFO] payment captured order=%s user=%d plan=%s subscription=%d period=%s..%s",
		entity.OrderID, payment.UserID, plan.Code, subscription.ID,
		subscription.CurrentPeriodStart.Format(time.RFC3339), subscription.CurrentPeriodEnd.Format(time.RFC3339))

	return models.WebhookStatusProcessed, "activated subscription " + fmt.Sprint(subscription.ID), nil
}

func applyPaymentFailed(entity payments.WebhookPayment) (string, string, error) {
	if entity.OrderID == "" {
		return models.WebhookStatusIgnored, "failure event carried no order id", nil
	}
	reason := strings.TrimSpace(entity.ErrorDescription)
	if reason == "" {
		reason = strings.TrimSpace(entity.ErrorCode)
	}
	if len(reason) > 255 {
		reason = reason[:255]
	}

	now := time.Now().UTC()
	// Only a payment still waiting is failed. A capture that already succeeded
	// must not be undone by a late failure event for an earlier attempt on the
	// same order.
	result := database.DB.Model(&models.Payment{}).
		Where("provider = ? AND provider_order_id = ? AND status = ?",
			payments.ProviderRazorpay, entity.OrderID, models.PaymentStatusCreated).
		Updates(map[string]any{
			"status":              models.PaymentStatusFailed,
			"provider_payment_id": entity.ID,
			"failure_reason":      reason,
			"updated_at":          now,
		})
	if result.Error != nil {
		return "", "", result.Error
	}
	if result.RowsAffected == 0 {
		return models.WebhookStatusIgnored, "no open payment for order " + entity.OrderID, nil
	}
	return models.WebhookStatusProcessed, "marked payment failed: " + reason, nil
}

// applyRefund records money going back out and ends the subscription the
// refunded payment bought.
//
// Credits already spent are not clawed back. They were consumed against real
// provider cost, the ledger is append-only by design, and driving a balance
// negative would break every allowance check that reads it. Ending the period
// stops further entitlement, which is the part that matters.
func applyRefund(entity payments.WebhookRefund) (string, string, error) {
	if entity.PaymentID == "" {
		return models.WebhookStatusIgnored, "refund event carried no payment id", nil
	}

	var payment models.Payment
	err := database.DB.Where("provider = ? AND provider_payment_id = ?", payments.ProviderRazorpay, entity.PaymentID).
		First(&payment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.WebhookStatusIgnored, "no local payment for refund " + entity.PaymentID, nil
	}
	if err != nil {
		return "", "", err
	}

	refunded := payment.AmountRefundedMinor + entity.AmountMinor
	if refunded > payment.AmountMinor {
		refunded = payment.AmountMinor
	}
	fullyRefunded := refunded >= payment.AmountMinor
	now := time.Now().UTC()

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"amount_refunded_minor": refunded,
			"updated_at":            now,
		}
		if fullyRefunded {
			updates["status"] = models.PaymentStatusRefunded
			updates["refunded_at"] = now
		}
		if err := tx.Model(&models.Payment{}).Where("id = ?", payment.ID).Updates(updates).Error; err != nil {
			return err
		}
		// A partial refund leaves the subscription running: the person still
		// paid for some of it, and deciding how much of a period a partial
		// refund buys is a support judgement, not an automatic one.
		if !fullyRefunded || payment.SubscriptionID == nil {
			return nil
		}
		return tx.Model(&models.UserSubscription{}).
			Where("id = ? AND status = ?", *payment.SubscriptionID, "active").
			Updates(map[string]any{
				"status":               "cancelled",
				"current_period_end":   now,
				"cancel_at_period_end": true,
				"updated_at":           now,
			}).Error
	})
	if err != nil {
		return "", "", err
	}

	if fullyRefunded {
		return models.WebhookStatusProcessed, "refunded in full, subscription ended", nil
	}
	return models.WebhookStatusProcessed, fmt.Sprintf("partial refund recorded: %d of %d", refunded, payment.AmountMinor), nil
}
