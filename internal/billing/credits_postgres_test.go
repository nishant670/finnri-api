package billing

// These tests cover what SQLite cannot show: its test handle has one
// connection, so two credit transactions never overlap there. They need a real
// PostgreSQL and are skipped without one:
//
//	TEST_POSTGRES_DSN='postgres://user@127.0.0.1:5432/postgres?sslmode=disable' \
//		go test ./internal/billing -run Postgres
//
// Each test creates its own database on that server, builds it with the real
// migration, and drops it afterwards. The database named in the DSN is only
// used to do that; nothing in it is read or written.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"finnri/internal/ai"
	"finnri/internal/database"
	"finnri/internal/models"
)

const postgresTestDSNEnv = "TEST_POSTGRES_DSN"

func setupPostgresCreditDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(postgresTestDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL credit tests", postgresTestDSNEnv)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL", postgresTestDSNEnv)
	}

	config := func() *gorm.Config { return &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)} }
	admin, err := gorm.Open(postgres.Open(dsn), config())
	if err != nil {
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("finnri_credit_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)").Error; err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
		_ = adminSQL.Close()
	})

	parsed.Path = "/" + name
	db, err := gorm.Open(postgres.Open(parsed.String()), config())
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(40)
	sqlDB.SetMaxIdleConns(40)
	t.Cleanup(func() { _ = sqlDB.Close() })

	previous := database.DB
	database.DB = db
	defer func() { database.DB = previous }()
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

func postgresTestUser(t *testing.T, db *gorm.DB, label string) models.User {
	t.Helper()
	user := models.User{UUID: label + "-uuid", Username: label}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func postgresTestGrant(t *testing.T, db *gorm.DB, userID uint, source string, credits int, expiresIn time.Duration) models.CreditGrant {
	t.Helper()
	now := time.Now().UTC()
	expiresAt := now.Add(expiresIn)
	grant := models.CreditGrant{
		UserID:           &userID,
		Source:           source,
		CreditsGranted:   credits,
		CreditsRemaining: credits,
		ValidFrom:        now.Add(-time.Hour),
		ExpiresAt:        &expiresAt,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	return grant
}

// together runs every call at the same moment and returns their errors in order.
func together(calls ...func() error) []error {
	start := make(chan struct{})
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = call()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

type creditTotals struct {
	balance int // what the grants say is left
	spent   int // granted minus that
	ledger  int // debits minus refunds in the ledger
	charged int // what the usage events say was charged
	daily   int // today's daily counter
}

func postgresTotals(t *testing.T, db *gorm.DB, subject CreditSubject) creditTotals {
	t.Helper()
	var totals creditTotals
	var grants []models.CreditGrant
	if err := scopeGrants(db, subject).Find(&grants).Error; err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		totals.balance += grant.CreditsRemaining
		totals.spent += grant.CreditsGranted - grant.CreditsRemaining
	}
	if err := scopeGrants(db.Model(&models.CreditLedger{}), subject).
		Select("COALESCE(SUM(CASE direction WHEN 'debit' THEN credits WHEN 'refund' THEN -credits ELSE 0 END), 0)").
		Scan(&totals.ledger).Error; err != nil {
		t.Fatal(err)
	}
	if err := scopeUsageEvents(db.Model(&models.AIUsageEvent{}), subject).
		Select("COALESCE(SUM(CASE WHEN status = 'reserved' THEN reserved_credits ELSE final_credits END), 0)").
		Scan(&totals.charged).Error; err != nil {
		t.Fatal(err)
	}
	if err := scopeDailyUsage(db.Model(&models.DailyCreditUsage{}), subject).
		Select("COALESCE(SUM(credits_used), 0)").Scan(&totals.daily).Error; err != nil {
		t.Fatal(err)
	}
	return totals
}

// SEC-16: a read that is refunded while another is being charged must not
// erase that charge, from the balance or from the daily count.
func TestPostgresRefundNeverErasesAConcurrentCharge(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)
	zero := 0

	refunds := []struct {
		name   string
		refund func(eventID uint) error
	}{
		{"cancel", func(id uint) error { _, err := service.CancelReservation(id); return err }},
		{"free failed read", func(id uint) error {
			_, err := service.FinalizeUsage(id, ProviderUsage{Status: UsageStatusFailedAfterProvider, FinalCredits: &zero})
			return err
		}},
	}
	for _, tc := range refunds {
		t.Run(tc.name, func(t *testing.T) {
			const rounds = 200
			lostBalance, lostDaily, failed := 0, 0, 0
			var firstErr error
			for round := range rounds {
				user := postgresTestUser(t, db, fmt.Sprintf("refund_%s_%d", strings.ReplaceAll(tc.name, " ", "_"), round))
				postgresTestGrant(t, db, user.ID, "promo", 1000, 24*time.Hour)
				subject := SubjectForUser(user.ID)
				refunded, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
				if err != nil {
					t.Fatal(err)
				}

				errs := together(
					func() error { return tc.refund(refunded.ID) },
					func() error {
						_, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
						return err
					},
				)
				if err := errors.Join(errs...); err != nil {
					failed++
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				totals := postgresTotals(t, db, subject)
				if totals.spent != 5 {
					lostBalance++
				}
				if totals.daily != 5 {
					lostDaily++
				}
			}
			if lostBalance > 0 || lostDaily > 0 || failed > 0 {
				t.Errorf("of %d rounds: balance wrong in %d, daily count wrong in %d, a request failed in %d (first: %v)",
					rounds, lostBalance, lostDaily, failed, firstErr)
			}
		})
	}
}

// The same, when the refund and the charge touch different grants: the first
// grant is used up by the read being refunded, so the two share no grant row
// and only the daily count is common to them.
func TestPostgresRefundKeepsTheDailyCountAcrossGrants(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)

	const rounds = 200
	lostBalance, lostDaily, failed := 0, 0, 0
	var firstErr error
	for round := range rounds {
		user := postgresTestUser(t, db, fmt.Sprintf("split_%d", round))
		postgresTestGrant(t, db, user.ID, "promo", 5, time.Hour)
		postgresTestGrant(t, db, user.ID, "manual_adjustment", 1000, 24*time.Hour)
		subject := SubjectForUser(user.ID)
		refunded, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
		if err != nil {
			t.Fatal(err)
		}

		errs := together(
			func() error { _, err := service.CancelReservation(refunded.ID); return err },
			func() error {
				_, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
				return err
			},
		)
		if err := errors.Join(errs...); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		totals := postgresTotals(t, db, subject)
		if totals.spent != 5 {
			lostBalance++
		}
		if totals.daily != 5 {
			lostDaily++
		}
	}
	if lostBalance > 0 || lostDaily > 0 || failed > 0 {
		t.Errorf("of %d rounds: balance wrong in %d, daily count wrong in %d, a request failed in %d (first: %v)",
			rounds, lostBalance, lostDaily, failed, firstErr)
	}
}

// reserveTogether sorts the results of parallel reservations. Anything that is neither a
// success nor a refusal with the expected reason would reach the user as a 500.
func reserveTogether(service *CreditService, subject CreditSubject, requests int, wantReason string) (succeeded, refused int, unexpected []string) {
	reasons := make([]string, requests)
	calls := make([]func() error, requests)
	for i := range calls {
		calls[i] = func() error {
			_, allowance, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
			reasons[i] = allowance.Reason
			return err
		}
	}
	for i, err := range together(calls...) {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAllowanceDenied) && reasons[i] == wantReason:
			refused++
		default:
			unexpected = append(unexpected, fmt.Sprintf("%v (reason %q)", err, reasons[i]))
		}
	}
	return succeeded, refused, unexpected
}

// SEC-17: parallel reads must not spend past the daily limit. Each round starts
// with no daily row, so it also covers creating that row under contention.
func TestPostgresParallelReadsStopAtTheDailyLimit(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)

	subjects := []struct {
		name    string
		limit   int
		subject func(round int) CreditSubject
	}{
		{"signed in", LoggedInFreeDailyLimit, func(round int) CreditSubject {
			user := postgresTestUser(t, db, fmt.Sprintf("daily_%d", round))
			if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
				t.Fatal(err)
			}
			return SubjectForUser(user.ID)
		}},
		{"guest", GuestDailyLimit, func(round int) CreditSubject {
			deviceID := fmt.Sprintf("daily-guest-%d", round)
			if _, _, err := service.EnsureGuestTrialGrant(deviceID, "127.0.0.1"); err != nil {
				t.Fatal(err)
			}
			return SubjectForGuestDeviceID(deviceID)
		}},
	}
	for _, tc := range subjects {
		t.Run(tc.name, func(t *testing.T) {
			const rounds, requests = 10, 30
			want := tc.limit / 5
			for round := range rounds {
				subject := tc.subject(round)
				succeeded, refused, unexpected := reserveTogether(service, subject, requests, AllowanceDailyLimitReached)
				totals := postgresTotals(t, db, subject)
				if succeeded != want || refused != requests-want || len(unexpected) > 0 {
					t.Errorf("round %d: %d succeeded (want %d), %d refused (want %d), %d other errors %s",
						round, succeeded, want, refused, requests-want, len(unexpected), first(unexpected))
				}
				if totals.spent != tc.limit || totals.daily != tc.limit || totals.ledger != tc.limit || totals.charged != tc.limit {
					t.Errorf("round %d: expected %d everywhere, got %+v", round, tc.limit, totals)
				}
				if t.Failed() {
					return
				}
			}
		})
	}
}

// The request that loses the last credits is told it has none left, not 500.
func TestPostgresParallelReadsOnTheLastCreditsAreRefused(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)

	const rounds, requests = 10, 20
	for round := range rounds {
		user := postgresTestUser(t, db, fmt.Sprintf("last_%d", round))
		// Two grants, so a debit has to take from both.
		postgresTestGrant(t, db, user.ID, "promo", 3, time.Hour)
		postgresTestGrant(t, db, user.ID, "manual_adjustment", 7, 24*time.Hour)
		subject := SubjectForUser(user.ID)

		succeeded, refused, unexpected := reserveTogether(service, subject, requests, AllowanceInsufficientCredits)
		totals := postgresTotals(t, db, subject)
		if succeeded != 2 || refused != requests-2 || len(unexpected) > 0 {
			t.Errorf("round %d: %d succeeded (want 2), %d refused (want %d), %d other errors %s",
				round, succeeded, refused, requests-2, len(unexpected), first(unexpected))
		}
		if totals.balance != 0 || totals.spent != 10 || totals.daily != 10 || totals.ledger != 10 || totals.charged != 10 {
			t.Errorf("round %d: expected 10 spent and nothing left, got %+v", round, totals)
		}
		if t.Failed() {
			return
		}
	}
}

// Two settlements of one read, such as the app cancelling just as the provider
// answers, must settle it once and neither may fail.
func TestPostgresOneReadSettlesOnce(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)

	const rounds = 100
	cancel := func(id uint) func() error {
		return func() error { _, err := service.CancelReservation(id); return err }
	}
	succeed := func(id uint) func() error {
		return func() error {
			_, err := service.FinalizeUsage(id, ProviderUsage{Status: UsageStatusSucceeded})
			return err
		}
	}
	failed := 0
	var firstErr error
	for round := range rounds {
		user := postgresTestUser(t, db, fmt.Sprintf("settle_%d", round))
		postgresTestGrant(t, db, user.ID, "promo", 1000, 24*time.Hour)
		subject := SubjectForUser(user.ID)
		event, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
		if err != nil {
			t.Fatal(err)
		}
		calls := []func() error{cancel(event.ID), cancel(event.ID)}
		if round%2 == 1 {
			calls[1] = succeed(event.ID)
		}
		if err := errors.Join(together(calls...)...); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
		totals := postgresTotals(t, db, subject)
		if totals.spent != totals.charged || totals.daily != totals.charged || totals.ledger != totals.charged {
			t.Fatalf("round %d: balance, ledger, daily count and event disagree: %+v", round, totals)
		}
	}
	if failed > 0 {
		t.Errorf("a settlement failed in %d of %d rounds (first: %v)", failed, rounds, firstErr)
	}
}

// Reserving, cancelling, refunding and charging extra all at once, across two
// grants, must neither deadlock nor leave the four records disagreeing.
func TestPostgresMixedCreditTrafficStaysConsistent(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)

	zero, extra := 0, 18
	const rounds, workers = 40, 12
	for round := range rounds {
		user := postgresTestUser(t, db, fmt.Sprintf("mixed_%d", round))
		postgresTestGrant(t, db, user.ID, "promo", 20, time.Hour)
		postgresTestGrant(t, db, user.ID, "manual_adjustment", 1000, 24*time.Hour)
		subject := SubjectForUser(user.ID)

		calls := make([]func() error, workers)
		for i := range calls {
			calls[i] = func() error {
				event, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseVoiceShort, "")
				if errors.Is(err, ErrAllowanceDenied) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("reserve: %w", err)
				}
				switch i % 3 {
				case 0:
					_, err = service.CancelReservation(event.ID)
				case 1:
					_, err = service.FinalizeUsage(event.ID, ProviderUsage{Status: UsageStatusFailedAfterProvider, FinalCredits: &zero})
				default:
					_, err = service.FinalizeUsage(event.ID, ProviderUsage{Status: UsageStatusSucceeded, FinalCredits: &extra})
				}
				if err != nil {
					return fmt.Errorf("settle: %w", err)
				}
				return nil
			}
		}
		if err := errors.Join(together(calls...)...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		totals := postgresTotals(t, db, subject)
		if totals.spent != totals.charged || totals.daily != totals.charged || totals.ledger != totals.charged {
			t.Fatalf("round %d: balance, ledger, daily count and events disagree: %+v", round, totals)
		}
		if totals.daily > LoggedInFreeDailyLimit {
			t.Fatalf("round %d: daily limit passed: %+v", round, totals)
		}
	}
}

// Expiry and pass revocation change grants without the subject lock, so a
// refund has to lock the grant it reads. Here such a write is still open when
// the refund starts.
func TestPostgresRefundWaitsForAWriterWithoutTheSubjectLock(t *testing.T) {
	db := setupPostgresCreditDB(t)
	service := NewCreditService(db)
	user := postgresTestUser(t, db, "unlocked_writer")
	grant := postgresTestGrant(t, db, user.ID, "promo", 1000, 24*time.Hour)
	subject := SubjectForUser(user.ID)
	event, _, err := service.ReserveCredits(subject, ai.ActionTransactionParseText, "")
	if err != nil {
		t.Fatal(err)
	}

	writer := db.Begin()
	defer writer.Rollback()
	if err := writer.Model(&models.CreditGrant{}).Where("id = ?", grant.ID).
		Update("credits_remaining", gorm.Expr("credits_remaining - ?", 100)).Error; err != nil {
		t.Fatal(err)
	}

	cancelled := make(chan error, 1)
	go func() {
		_, err := service.CancelReservation(event.ID)
		cancelled <- err
	}()
	waitForBlockedBackend(t, db)
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}

	if totals := postgresTotals(t, db, subject); totals.balance != 900 {
		t.Fatalf("expected the other write to survive the refund and leave 900, got %d", totals.balance)
	}
}

// waitForBlockedBackend returns once a query in this database is waiting on a
// lock.
func waitForBlockedBackend(t *testing.T, db *gorm.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int64
		if err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").
			Scan(&blocked).Error; err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing waited on a lock")
}

// The daily count does not rely on the subject lock: writers that race to
// create the day's row, or to change it, must all be counted.
func TestPostgresDailyUsageCountsEveryParallelWriter(t *testing.T) {
	db := setupPostgresCreditDB(t)
	now := time.Now().UTC()

	const rounds, writers = 20, 20
	for round := range rounds {
		user := postgresTestUser(t, db, fmt.Sprintf("upsert_%d", round))
		for _, subject := range []CreditSubject{
			SubjectForUser(user.ID),
			SubjectForGuestDeviceID(fmt.Sprintf("upsert-guest-%d", round)),
		} {
			calls := make([]func() error, writers)
			for i := range calls {
				calls[i] = func() error {
					return db.Transaction(func(tx *gorm.DB) error { return adjustDailyUsage(tx, subject, now, 5) })
				}
			}
			if err := errors.Join(together(calls...)...); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if totals := postgresTotals(t, db, subject); totals.daily != 5*writers {
				t.Fatalf("round %d: expected %d counted, got %d", round, 5*writers, totals.daily)
			}
		}
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return "(first: " + values[0] + ")"
}
