package http

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

const receiptBatchTestReply = `{"summary":"DMart bill","type":"expense","title":"DMart","amount":1480,"currency":"INR","mode":"UPI","category":"Shopping","merchant":"DMart","date":"2026-11-12"}`

func seedBatchUsage(t *testing.T, userID uint, batchID, status string, count int) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		if err := database.DB.Create(&models.AIUsageEvent{
			UserID:     &userID,
			RequestID:  fmt.Sprintf("%s-%s-%s-%d", t.Name(), batchID, status, i),
			ActionCode: string(ai.ActionTransactionParseReceipt),
			InputKind:  "image",
			Status:     status,
			BatchID:    batchID,
			StartedAt:  now.Add(-time.Duration(i) * time.Second),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestReceiptBatchTagsTheUsageEvent(t *testing.T) {
	server := newReceiptServer(t, receiptBatchTestReply)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	ctx.Request.Header.Set(receiptBatchHeader, "batch-tag-0001")

	server.handleParseReceipt(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	user, _ := ctx.Get("user")
	var event models.AIUsageEvent
	if err := database.DB.Where("user_id = ?", user.(*models.User).ID).Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.BatchID != "batch-tag-0001" || event.Status != billing.UsageStatusSucceeded {
		t.Fatalf("event = %+v", event)
	}
}

func TestReceiptBatchRefusesAMalformedID(t *testing.T) {
	server := newReceiptServer(t, receiptBatchTestReply)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	ctx.Request.Header.Set(receiptBatchHeader, "no spaces allowed!")

	server.handleParseReceipt(ctx)

	if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_batch_id"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestReceiptBatchStopsAfterFiveChargedBills(t *testing.T) {
	server := newReceiptServer(t, receiptBatchTestReply)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	seedBatchUsage(t, user.(*models.User).ID, "batch-full-0001", billing.UsageStatusSucceeded, receiptBatchMaxBills)
	ctx.Request.Header.Set(receiptBatchHeader, "batch-full-0001")

	server.handleParseReceipt(ctx)

	if response.Code != 422 || !strings.Contains(response.Body.String(), `"receipt_batch_full"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestReceiptBatchStopsAfterItsRetriesAreUsed(t *testing.T) {
	server := newReceiptServer(t, receiptBatchTestReply)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	seedBatchUsage(t, user.(*models.User).ID, "batch-retries-01", billing.UsageStatusFailedAfterProvider, receiptBatchMaxReads)
	ctx.Request.Header.Set(receiptBatchHeader, "batch-retries-01")

	server.handleParseReceipt(ctx)

	if response.Code != 422 || !strings.Contains(response.Body.String(), `"receipt_batch_full"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestReceiptBatchStillReadsTheFifthBill(t *testing.T) {
	server := newReceiptServer(t, receiptBatchTestReply)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	userID := user.(*models.User).ID
	seedBatchUsage(t, userID, "batch-fifth-0001", billing.UsageStatusSucceeded, receiptBatchMaxBills-1)
	seedBatchUsage(t, userID, "batch-fifth-0001", billing.UsageStatusFailedAfterProvider, 2)
	ctx.Request.Header.Set(receiptBatchHeader, "batch-fifth-0001")

	server.handleParseReceipt(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestFailureCooldownCountsABatchOnce(t *testing.T) {
	ctx, _ := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	userID := user.(*models.User).ID
	subject := billing.SubjectForUser(userID)
	cfg := &config.Config{
		AIFailedParseCooldownThreshold: 2,
		AIFailedParseCooldownWindowMin: 15,
		AIFailedParseCooldownMinutes:   10,
	}

	// Three unreadable photos in one scan: one failure.
	seedBatchUsage(t, userID, "batch-cool-00001", billing.UsageStatusFailedAfterProvider, 3)
	active, _, failures, err := activeAIFailureCooldown(subject, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if active || failures != 1 {
		t.Fatalf("one batch should count once: active=%v failures=%d", active, failures)
	}

	// One more failed read on its own reaches the threshold of two.
	seedBatchUsage(t, userID, "", billing.UsageStatusFailedAfterProvider, 1)
	active, _, failures, err = activeAIFailureCooldown(subject, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !active || failures != 2 {
		t.Fatalf("batch + single read should trip the cooldown: active=%v failures=%d", active, failures)
	}
}
