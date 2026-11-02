package http

import (
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The report: deleting the account of anyone who had ever paid failed. The
// live foreign keys are GORM's NO ACTION ones, so Postgres refused to drop a
// user that payments, card statements, EMI plans, monthly reviews or feedback
// still pointed at, and refused to drop a pass a payment still pointed at.
//
// SQLite ignores foreign keys unless asked, which is how this hid from
// TestDeleteUserDataDeletesBillingAndAIRecords.
func TestDeleteUserDataWithPaymentsAndCardData(t *testing.T) {
	useSmokeDatabase(t)
	if err := database.DB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.DB.Exec("PRAGMA foreign_keys = OFF") })

	seeded := seedPayingUser(t)
	if _, err := deleteUserData(database.DB, seeded.user); err != nil {
		t.Fatalf("deleteUserData failed: %v", err)
	}
	assertPayingUserDeleted(t, seeded)
}

// The same delete against real PostgreSQL, migrated the way production is.
// SQLite builds its foreign keys from the model tags, which is exactly what
// production does not do, so only Postgres proves the live constraints allow
// it. Set FINNRI_SCRATCH_DATABASE_URL to a throwaway database: the test runs
// every migration against it and leaves rows behind.
func TestDeleteUserDataWithPaymentsOnPostgres(t *testing.T) {
	dsn := os.Getenv("FINNRI_SCRATCH_DATABASE_URL")
	if dsn == "" {
		t.Skip("FINNRI_SCRATCH_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	previous := database.DB
	database.DB = db
	t.Cleanup(func() { database.DB = previous })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seeded := seedPayingUser(t)
	// payment_refunds arrives with the refund fix; when it is there, a refund
	// row must neither block the delete nor be lost with it.
	var refunds *string
	db.Raw("SELECT to_regclass('payment_refunds')::text").Scan(&refunds)
	if refunds != nil {
		if err := db.Exec(`INSERT INTO payment_refunds (payment_id, provider, provider_refund_id, amount_minor, status, created_at, updated_at)
			VALUES (?, 'razorpay', ?, 1000, 'processed', NOW(), NOW())`, seeded.payment.ID, "rfnd_"+generateUUID()[:12]).Error; err != nil {
			t.Fatal(err)
		}
	}

	if _, err := deleteUserData(database.DB, seeded.user); err != nil {
		t.Fatalf("deleteUserData failed: %v", err)
	}
	assertPayingUserDeleted(t, seeded)
	if refunds != nil {
		var count int64
		db.Raw("SELECT COUNT(*) FROM payment_refunds WHERE payment_id = ?", seeded.payment.ID).Scan(&count)
		if count != 1 {
			t.Fatalf("the refund row must stay with the kept payment, got %d", count)
		}
	}
}

type payingUser struct {
	user         models.User
	subscription models.UserSubscription
	payment      models.Payment
}

// seedPayingUser makes a user who bought a pass and used the card features:
// every row that points at the user, or at something the delete removes,
// through a foreign key the delete has to get past.
func seedPayingUser(t *testing.T) payingUser {
	t.Helper()
	create := func(value any) {
		t.Helper()
		if err := database.DB.Create(value).Error; err != nil {
			t.Fatalf("create %T: %v", value, err)
		}
	}

	now := time.Now().UTC()
	user := models.User{UUID: generateUUID(), Username: "paid_delete_" + generateUUID()[:8]}
	create(&user)
	plan := models.Plan{Code: "paid_delete_" + generateUUID()[:8], Name: "Delete Test", BillingInterval: "monthly", IncludedCredits: 3000, DailyCreditLimit: 200}
	create(&plan)
	subscription := models.UserSubscription{
		UserID: user.ID, PlanID: plan.ID, Status: "active", Provider: "razorpay",
		CurrentPeriodStart: now.AddDate(0, 0, -1), CurrentPeriodEnd: now.AddDate(0, 1, 0),
	}
	create(&subscription)
	create(&models.CreditGrant{
		UserID: &user.ID, Source: "subscription_period", CreditsGranted: 3000, CreditsRemaining: 2500,
		ValidFrom: now.AddDate(0, 0, -1), SubscriptionID: &subscription.ID,
	})
	payment := models.Payment{
		UserID: &user.ID, PlanID: plan.ID, Provider: "razorpay",
		ProviderOrderID: "order_" + generateUUID()[:12], ProviderPaymentID: "pay_" + generateUUID()[:12],
		Status: models.PaymentStatusCaptured, AmountMinor: 7900, Currency: "INR",
		SubscriptionID: &subscription.ID, CapturedAt: &now,
	}
	create(&payment)

	card := models.Account{UserID: user.ID, Type: "credit_card", Name: "Regalia", Color: "#8257E5"}
	create(&card)
	notification := models.Notification{UserID: user.ID, Type: "card_statement_due", Title: "Bill due", Body: "Pay it"}
	create(&notification)
	statement := models.CardStatement{
		UserID: user.ID, AccountID: card.ID, CycleStart: "2026-09-01", CycleEnd: "2026-09-30",
		StatementDate: "2026-09-30", DueDate: "2026-10-18", TotalDue: models.Money(500000),
	}
	create(&statement)
	create(&models.CardStatementPayment{UserID: user.ID, StatementID: statement.ID, AccountID: card.ID, Amount: models.Money(100000), PaidOn: "2026-10-10"})
	create(&models.CardStatementReminder{UserID: user.ID, StatementID: statement.ID, Kind: "due_soon", NotificationID: &notification.ID})
	emi := models.CardEMIPlan{
		UserID: user.ID, AccountID: card.ID, Title: "Phone", Principal: models.Money(6000000), TenureMonths: 6,
		MonthlyAmount: models.Money(1000000), PurchasedOn: "2026-09-05", FirstInstallment: "2026-09-30",
	}
	create(&emi)
	create(&models.CardEMIInstallment{
		PlanID: emi.ID, UserID: user.ID, AccountID: card.ID, Seq: 1, DueDate: "2026-09-30",
		Amount: models.Money(1000000), PrincipalPart: models.Money(1000000), InterestPart: 0,
	})
	create(&models.MonthlyReview{UserID: user.ID, Month: "2026-09", NotificationID: &notification.ID})
	create(&models.Feedback{UserID: user.ID, Type: "bug", Title: "Broken", Message: "It broke", Attachments: models.StringArray{}})

	return payingUser{user: user, subscription: subscription, payment: payment}
}

func assertPayingUserDeleted(t *testing.T, seeded payingUser) {
	t.Helper()
	userID := seeded.user.ID
	assertIDCount(t, &models.User{}, userID, 0)
	assertIDCount(t, &models.UserSubscription{}, seeded.subscription.ID, 0)
	for _, model := range []any{
		&models.CardStatement{}, &models.CardStatementPayment{}, &models.CardStatementReminder{},
		&models.CardEMIPlan{}, &models.CardEMIInstallment{}, &models.MonthlyReview{},
		&models.Feedback{}, &models.Notification{}, &models.Account{},
	} {
		assertUserIDCount(t, model, userID, 0)
	}
	assertUserBillingCount(t, &models.CreditGrant{}, userID, "", 0)

	// The payment is the financial record: it stays, minus the person.
	var payment models.Payment
	if err := database.DB.First(&payment, seeded.payment.ID).Error; err != nil {
		t.Fatalf("the payment must be kept: %v", err)
	}
	if payment.UserID != nil || payment.SubscriptionID != nil {
		t.Fatalf("the kept payment must not point at the deleted user or pass: %#v", payment)
	}
	if payment.AmountMinor != 7900 || payment.ProviderPaymentID != seeded.payment.ProviderPaymentID || payment.Status != models.PaymentStatusCaptured {
		t.Fatalf("the kept payment must be unchanged otherwise: %#v", payment)
	}
}
