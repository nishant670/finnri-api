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

func TestReceiptBatchSlotIsRefusedWhenReadsArriveTogether(t *testing.T) {
	ctx, response := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	userID := user.(*models.User).ID
	subject := billing.SubjectForUser(userID)
	credits := billing.NewCreditService(database.DB)
	before, err := credits.CheckAllowance(subject, ai.ActionTransactionParseReceipt)
	if err != nil {
		t.Fatal(err)
	}

	// Five reads of this batch already held (charged or in flight). This one
	// passed the early check before they were tagged, as two reads sent at once
	// would; reserving and tagging it must take it back.
	seedBatchUsage(t, userID, "batch-race-00001", billing.UsageStatusSucceeded, 3)
	seedBatchUsage(t, userID, "batch-race-00001", billing.UsageStatusReserved, 2)
	event, _, err := credits.ReserveCredits(subject, ai.ActionTransactionParseReceipt, "")
	if err != nil {
		t.Fatal(err)
	}

	if claimReceiptBatchSlot(ctx, credits, subject, event.ID, "batch-race-00001") {
		t.Fatal("a sixth read in flight should be refused")
	}
	if response.Code != 422 || !strings.Contains(response.Body.String(), `"receipt_batch_full"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var stored models.AIUsageEvent
	if err := database.DB.First(&stored, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != billing.UsageStatusCancelled {
		t.Fatalf("refused read should be cancelled, got %q", stored.Status)
	}
	after, err := credits.CheckAllowance(subject, ai.ActionTransactionParseReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailableCredits != before.AvailableCredits {
		t.Fatalf("credits not returned: before %d, after %d", before.AvailableCredits, after.AvailableCredits)
	}
}

func TestReceiptBatchFailureWeight(t *testing.T) {
	for failures, want := range map[int64]int64{0: 0, 1: 1, 3: 1, 4: 2, 10: 4} {
		if got := receiptBatchFailureWeight(failures); got != want {
			t.Fatalf("weight(%d) = %d, want %d", failures, got, want)
		}
	}
}

func TestFailureCooldownWeighsABatchOfManyFailures(t *testing.T) {
	ctx, _ := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	userID := user.(*models.User).ID
	cfg := &config.Config{
		AIFailedParseCooldownThreshold: 4,
		AIFailedParseCooldownWindowMin: 15,
		AIFailedParseCooldownMinutes:   10,
	}
	// Ten failures under one batch id count as four, not one.
	seedBatchUsage(t, userID, "batch-many-00001", billing.UsageStatusFailedAfterProvider, 10)
	active, _, failures, err := activeAIFailureCooldown(billing.SubjectForUser(userID), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !active || failures != 4 {
		t.Fatalf("active=%v failures=%d", active, failures)
	}
}
