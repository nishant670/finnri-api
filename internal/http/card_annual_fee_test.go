package http

import (
	"strings"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestAnnualFeeWindowRollsOverOnlyAfterTheFeeMonth(t *testing.T) {
	cases := []struct {
		today, renewal, yearStart string
	}{
		{"2026-10-06", "2027-03-01", "2026-03-01"}, // months away: next March
		{"2026-03-15", "2026-03-01", "2025-03-01"}, // inside the fee month: this March
		{"2026-03-31", "2026-03-01", "2025-03-01"}, // last day still counts
		{"2026-04-01", "2027-03-01", "2026-03-01"}, // month over: next year
	}
	for _, test := range cases {
		renewal, yearStart := annualFeeWindow(time.March, mustDate(t, test.today))
		if renewal.Format(apiDateLayout) != test.renewal || yearStart.Format(apiDateLayout) != test.yearStart {
			t.Errorf("%s: got %s / %s, want %s / %s", test.today,
				renewal.Format(apiDateLayout), yearStart.Format(apiDateLayout), test.renewal, test.yearStart)
		}
	}
}

func TestSummariseAnnualFeeWaiverProgress(t *testing.T) {
	card := models.Account{AnnualFee: rupees(500), FeeWaiverSpend: rupees(100000)}
	renewal, yearStart := annualFeeWindow(time.March, mustDate(t, "2026-02-10"))

	short := summariseAnnualFee(card, renewal, yearStart, rupees(82000))
	if *short.RemainingToWaive != rupees(18000) || *short.Waived {
		t.Fatalf("unexpected progress: %+v", short)
	}
	over := summariseAnnualFee(card, renewal, yearStart, rupees(120000))
	if *over.RemainingToWaive != 0 || !*over.Waived {
		t.Fatalf("spend above the threshold should waive: %+v", over)
	}
	noWaiver := summariseAnnualFee(models.Account{AnnualFee: rupees(500)}, renewal, yearStart, rupees(1))
	if noWaiver.WaiverSpend != nil || noWaiver.Waived != nil {
		t.Fatalf("no threshold, no waiver figures: %+v", noWaiver)
	}
}

func countAnnualFeeNotifications(t *testing.T, userID uint) []models.Notification {
	t.Helper()
	var notifications []models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", userID, annualFeeReminderType).
		Find(&notifications).Error; err != nil {
		t.Fatal(err)
	}
	return notifications
}

func TestAnnualFeeReminderIsSentOnceWithWaiverProgress(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	card.AnnualFee = rupees(500)
	card.FeeWaiverSpend = rupees(100000)
	card.FeeMonth = "March"
	if err := database.DB.Save(&card).Error; err != nil {
		t.Fatal(err)
	}
	createCardSpend(t, user.ID, card.ID, "2025-06-10", "50000")
	createCardSpend(t, user.ID, card.ID, "2026-01-20", "32000")
	// Before the card year: must not count.
	createCardSpend(t, user.ID, card.ID, "2025-02-20", "40000")

	// Too early: 2 months out.
	if sent, err := syncAnnualFeeReminders(user.ID, mustDate(t, "2026-01-01")); err != nil || sent != 0 {
		t.Fatalf("sent %d, err %v; want nothing two months out", sent, err)
	}

	for run := 0; run < 2; run++ {
		if _, err := syncAnnualFeeReminders(user.ID, mustDate(t, "2026-02-10")); err != nil {
			t.Fatal(err)
		}
	}
	notifications := countAnnualFeeNotifications(t, user.ID)
	if len(notifications) != 1 {
		t.Fatalf("annual fee reminders = %d, want exactly 1", len(notifications))
	}
	body := notifications[0].Body
	if !strings.Contains(body, "₹18,000 more") || !strings.Contains(body, "28 Feb") {
		t.Fatalf("reminder should state the remaining spend and the deadline: %q", body)
	}
	if !strings.Contains(notifications[0].ActionURL, "fee_year=2026") {
		t.Fatalf("action url should carry the renewal year: %q", notifications[0].ActionURL)
	}

	// Next year's renewal is a separate reminder.
	if _, err := syncAnnualFeeReminders(user.ID, mustDate(t, "2027-02-10")); err != nil {
		t.Fatal(err)
	}
	if got := len(countAnnualFeeNotifications(t, user.ID)); got != 2 {
		t.Fatalf("a new renewal year should remind again, got %d", got)
	}
}

func TestAnnualFeeReminderSkipsCardsWithoutAFeeMonth(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	card.AnnualFee = rupees(500)
	if err := database.DB.Save(&card).Error; err != nil {
		t.Fatal(err)
	}
	if sent, err := syncAnnualFeeReminders(user.ID, mustDate(t, "2026-02-10")); err != nil || sent != 0 {
		t.Fatalf("sent %d, err %v; a fee with no month has no date to remind on", sent, err)
	}
}

func TestRupeesForCopyUsesIndianGrouping(t *testing.T) {
	cases := map[models.Money]string{
		rupees(500):           "₹500",
		rupees(18000):         "₹18,000",
		rupees(100000):        "₹1,00,000",
		rupees(12345678):      "₹1,23,45,678",
		models.Money(49950):   "₹499.50",
		models.Money(-150000): "-₹1,500",
	}
	for amount, want := range cases {
		if got := rupeesForCopy(amount); got != want {
			t.Errorf("rupeesForCopy(%d) = %q, want %q", amount, got, want)
		}
	}
}
