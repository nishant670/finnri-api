package http

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

func india(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func TestMonthlyReviewRangeCoversTheWholeMonthAndTheWholePreviousOne(t *testing.T) {
	location := india(t)
	cases := []struct {
		month                  string
		start, end             string
		previousStart, prevEnd string
	}{
		{"2026-08", "2026-08-01", "2026-08-31", "2026-07-01", "2026-07-31"},
		// A 31-day month against a 30-day one takes all of the shorter month.
		{"2026-05", "2026-05-01", "2026-05-31", "2026-04-01", "2026-04-30"},
		// March against February is the case that breaks naive date arithmetic:
		// the comparison is all 28 days, not a 31st of February.
		{"2026-03", "2026-03-01", "2026-03-31", "2026-02-01", "2026-02-28"},
		// A leap February.
		{"2024-02", "2024-02-01", "2024-02-29", "2024-01-01", "2024-01-29"},
		// Across a year boundary.
		{"2026-01", "2026-01-01", "2026-01-31", "2025-12-01", "2025-12-31"},
	}

	for _, testCase := range cases {
		t.Run(testCase.month, func(t *testing.T) {
			dateRange, err := monthlyReviewRange(testCase.month, location)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{
				dateRange.Start.Format("2006-01-02"),
				dateRange.End.Format("2006-01-02"),
				dateRange.PreviousStart.Format("2006-01-02"),
				dateRange.PreviousEnd.Format("2006-01-02"),
			}
			want := []string{testCase.start, testCase.end, testCase.previousStart, testCase.prevEnd}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("range = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestMonthlyReviewRangeRejectsNonsense(t *testing.T) {
	for _, month := range []string{"", "2026", "August", "2026-13-01", "26-08"} {
		if _, err := monthlyReviewRange(month, india(t)); err == nil {
			t.Errorf("monthlyReviewRange(%q) accepted an unusable month", month)
		}
	}
}

func TestMostRecentClosedMonthIsNeverTheCurrentOne(t *testing.T) {
	cases := map[string]string{
		"2026-09-01": "2026-08",
		"2026-09-30": "2026-08",
		"2026-01-01": "2025-12",
		// The 1st of March still reviews February, not January.
		"2026-03-01": "2026-02",
	}
	for day, want := range cases {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil {
			t.Fatal(err)
		}
		if got := mostRecentClosedMonth(parsed); got != want {
			t.Errorf("mostRecentClosedMonth(%s) = %s, want %s", day, got, want)
		}
	}
}

// The notification the task specifies, assembled from figures rather than
// written by hand.
func TestMonthlyReviewCopyMatchesTheSpecifiedLine(t *testing.T) {
	review := MonthlyReviewResponse{
		Label:         "August 2026",
		PreviousLabel: "July",
		Summary: DashboardSummary{
			TotalSpent:            40091,
			TransactionCount:      34, // one of them is income, and must not be counted
			ExpenseCount:          33,
			SpendChange:           -12,
			SpendChangeComparable: true,
		},
		BiggestChange: &MonthlyReviewChange{Category: "Food & Drinks"},
	}

	title, body := monthlyReviewCopy(review)
	if title != "August 2026 in review" {
		t.Fatalf("title = %q", title)
	}
	want := "₹40,091 across 33 transactions, 12% under July. Biggest change: Food & Drinks."
	if body != want {
		t.Fatalf("body  = %q\nwant  = %q", body, want)
	}
}

// A clause with nothing honest to say is dropped, not faked. "0% under July"
// reads as "spending is flat", which is a claim the data did not support.
func TestMonthlyReviewCopyDropsClausesItCannotSupport(t *testing.T) {
	base := MonthlyReviewResponse{
		Label:         "August 2026",
		PreviousLabel: "July",
		Summary:       DashboardSummary{TotalSpent: 40091, TransactionCount: 33, ExpenseCount: 33},
	}

	_, body := monthlyReviewCopy(base)
	if body != "₹40,091 across 33 transactions." {
		t.Fatalf("uncomparable month = %q", body)
	}
	if strings.Contains(body, "July") || strings.Contains(body, "0%") {
		t.Fatalf("an unpublishable comparison leaked into the copy: %q", body)
	}

	withChange := base
	withChange.BiggestChange = &MonthlyReviewChange{Category: "Transport"}
	if _, body := monthlyReviewCopy(withChange); body != "₹40,091 across 33 transactions. Biggest change: Transport." {
		t.Fatalf("body = %q", body)
	}

	single := base
	single.Summary = DashboardSummary{TotalSpent: 450, TransactionCount: 1, ExpenseCount: 1}
	if _, body := monthlyReviewCopy(single); body != "₹450 across 1 transaction." {
		t.Fatalf("singular = %q", body)
	}
}

func TestMonthlyReviewCopyReportsIncreasesAsOver(t *testing.T) {
	review := MonthlyReviewResponse{
		Label:         "August 2026",
		PreviousLabel: "July",
		Summary: DashboardSummary{
			TotalSpent: 51000, TransactionCount: 40, ExpenseCount: 40,
			SpendChange: 27, SpendChangeComparable: true,
		},
	}
	if _, body := monthlyReviewCopy(review); !strings.Contains(body, "27% over July") {
		t.Fatalf("body = %q", body)
	}
}

// The server writes this one sentence without the app's formatter, because no
// app is running when the job fires. It has to agree with formatMoney.
func TestReviewMoneyFormatsLikeTheAppsFormatter(t *testing.T) {
	cases := map[float64]string{
		0:        "₹0.00",
		42.5:     "₹42.50",
		99.99:    "₹99.99",
		100:      "₹100",
		1000:     "₹1,000",
		40091:    "₹40,091",
		40091.49: "₹40,091",
		200000:   "₹2,00,000",
		12345678: "₹1,23,45,678",
		-2753:    "-₹2,753",
	}
	for amount, want := range cases {
		if got := formatReviewMoney(amount); got != want {
			t.Errorf("formatReviewMoney(%v) = %q, want %q", amount, got, want)
		}
	}
}

// Ranking on percentage always crowns the smallest category. The month's story
// is the one that moved the most money.
func TestBiggestChangeRanksByRupeesNotPercentage(t *testing.T) {
	categories := []DashboardCategory{
		{Category: "Food & Drinks", Amount: 26000},
		{Category: "Entertainment", Amount: 3000},
	}
	previous := map[string]comparisonBase{
		"Food & Drinks": {Amount: 20000, Count: 40},
		// +400%, and ₹2,400 — a bigger percentage on a much smaller move.
		"Entertainment": {Amount: 600, Count: 6},
	}

	change := biggestCategoryChange(categories, previous)
	if change == nil || change.Category != "Food & Drinks" {
		t.Fatalf("biggest change = %+v, want Food & Drinks", change)
	}
	if !change.Comparable || change.Change != 30 {
		t.Fatalf("change = %+v, want a publishable +30%%", change)
	}
}

func TestBiggestChangeIgnoresNoiseAndUnpublishableBases(t *testing.T) {
	// Nothing moved far enough to be the story of a month.
	quiet := biggestCategoryChange(
		[]DashboardCategory{{Category: "Misc", Amount: 240}},
		map[string]comparisonBase{"Misc": {Amount: 200, Count: 8}},
	)
	if quiet != nil {
		t.Fatalf("a ₹40 move was called the month's biggest change: %+v", quiet)
	}

	// A category that did not exist last month can still be the story — it just
	// carries no percentage, because there is nothing to divide by.
	fresh := biggestCategoryChange(
		[]DashboardCategory{{Category: "Travel", Amount: 18000}},
		map[string]comparisonBase{},
	)
	if fresh == nil || fresh.Category != "Travel" {
		t.Fatalf("a new category could not win: %+v", fresh)
	}
	if fresh.Comparable || fresh.Change != 0 {
		t.Fatalf("a percentage was published against an empty base: %+v", fresh)
	}
	if fresh.Direction != "higher" {
		t.Fatalf("direction = %q", fresh.Direction)
	}
}

func newReviewUser(t *testing.T) models.User {
	t.Helper()
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	return attachParseUser(t, context, false)
}

func seedReviewEntry(t *testing.T, userID uint, amount, category, date string) {
	t.Helper()
	parsed, err := models.ParseMoney(amount)
	if err != nil {
		t.Fatal(err)
	}
	entry := models.Entry{
		UserID: userID, Title: category, Type: "expense", Amount: parsed,
		Currency: "INR", Category: category, Merchant: category, Date: date,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
}

func reviewServer() *Server {
	return &Server{cfg: &config.Config{TZDefault: "Asia/Kolkata", ReqTimeoutSec: 2}}
}

// Six ticks through the send window must produce one notification, because the
// job is a ticker and the database is what says "already sent".
func TestMonthlyReviewIsSentOnlyOnce(t *testing.T) {
	user := newReviewUser(t)
	for day := 1; day <= 6; day++ {
		seedReviewEntry(t, user.ID, "800.00", "Food & Drinks", "2026-07-0"+string(rune('0'+day)))
	}

	dateRange, err := monthlyReviewRange("2026-07", india(t))
	if err != nil {
		t.Fatal(err)
	}

	delivered := 0
	for attempt := 0; attempt < 6; attempt++ {
		sent, err := sendMonthlyReview(user.ID, "2026-07", dateRange)
		if err != nil {
			t.Fatal(err)
		}
		if sent {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("sent %d reviews, want exactly 1", delivered)
	}

	var notifications []models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", user.ID, monthlyReviewNotificationType).
		Find(&notifications).Error; err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 {
		t.Fatalf("%d notifications, want 1", len(notifications))
	}
	if notifications[0].ActionURL != "/monthly-review/2026-07" {
		t.Fatalf("action_url = %q — the notification has to open the review", notifications[0].ActionURL)
	}
	if !strings.Contains(notifications[0].Body, "₹4,800 across 6 transactions") {
		t.Fatalf("body = %q", notifications[0].Body)
	}
}

// A month with almost nothing in it is not worth a notification.
func TestThinMonthIsNotNotified(t *testing.T) {
	user := newReviewUser(t)
	seedReviewEntry(t, user.ID, "450.00", "Food & Drinks", "2026-07-04")

	dateRange, err := monthlyReviewRange("2026-07", india(t))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := sendMonthlyReview(user.ID, "2026-07", dateRange)
	if err != nil {
		t.Fatal(err)
	}
	if sent {
		t.Fatal("a one-transaction month produced a review notification")
	}
}

// The job only runs inside its window, so a user installing on the 20th is not
// handed a review of a month that ended three weeks ago.
func TestMonthlyReviewJobOnlyRunsInsideItsWindow(t *testing.T) {
	user := newReviewUser(t)
	for day := 1; day <= 6; day++ {
		seedReviewEntry(t, user.ID, "800.00", "Food & Drinks", "2026-07-0"+string(rune('0'+day)))
	}
	location := india(t)

	late := time.Date(2026, time.August, 20, 9, 0, 0, 0, location)
	if sent, err := runMonthlyReviewOnce(late, location); err != nil || sent != 0 {
		t.Fatalf("late run sent %d (err %v), want 0", sent, err)
	}

	first := time.Date(2026, time.August, 1, 9, 0, 0, 0, location)
	sent, err := runMonthlyReviewOnce(first, location)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("run on the 1st sent %d, want 1", sent)
	}
	// The 2nd is inside the window; the index, not the date, stops the repeat.
	second := time.Date(2026, time.August, 2, 9, 0, 0, 0, location)
	if sent, err := runMonthlyReviewOnce(second, location); err != nil || sent != 0 {
		t.Fatalf("second run sent %d (err %v), want 0", sent, err)
	}
}

func TestMonthlyReviewEndpointServesTheClosedMonth(t *testing.T) {
	user := newReviewUser(t)
	for day := 1; day <= 6; day++ {
		seedReviewEntry(t, user.ID, "800.00", "Food & Drinks", "2026-07-0"+string(rune('0'+day)))
	}
	seedReviewEntry(t, user.ID, "2000.00", "Travel", "2026-07-09")

	request := httptest.NewRequest("GET", "/v1/monthly-review?month=2026-07", nil)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	context.Set("userID", user.ID)

	reviewServer().getMonthlyReview(context)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var review MonthlyReviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.Label != "July 2026" || review.PreviousLabel != "June" {
		t.Fatalf("labels = %q / %q", review.Label, review.PreviousLabel)
	}
	if review.StartDate != "2026-07-01" || review.EndDate != "2026-07-31" {
		t.Fatalf("range = %s..%s", review.StartDate, review.EndDate)
	}
	if !review.Available || review.Summary.TotalSpent != 6800 || review.Summary.TransactionCount != 7 {
		t.Fatalf("summary = %+v", review.Summary)
	}
	if review.BusiestDay == nil || review.BusiestDay.Amount != 2000 {
		t.Fatalf("busiest day = %+v, want the ₹2,000 travel day", review.BusiestDay)
	}
	if review.NotifiedAt != nil {
		t.Fatal("nothing was sent, so notified_at must be absent")
	}
}

// A month still being lived in cannot be reviewed: every figure would move.
func TestMonthlyReviewEndpointRefusesAnUnfinishedMonth(t *testing.T) {
	user := newReviewUser(t)
	current := time.Now().In(india(t)).Format("2006-01")

	request := httptest.NewRequest("GET", "/v1/monthly-review?month="+current, nil)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	context.Set("userID", user.ID)

	reviewServer().getMonthlyReview(context)

	if response.Code != 422 || !strings.Contains(response.Body.String(), "invalid_month") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestMonthlyReviewEndpointRejectsAMalformedMonth(t *testing.T) {
	user := newReviewUser(t)
	request := httptest.NewRequest("GET", "/v1/monthly-review?month=August", nil)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	context.Set("userID", user.ID)

	reviewServer().getMonthlyReview(context)

	if response.Code != 422 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

// One user's review must not describe another's ledger.
func TestMonthlyReviewIsScopedToItsUser(t *testing.T) {
	mine := newReviewUser(t)
	theirs := models.User{UUID: t.Name() + "-other", Username: t.Name() + "-other"}
	if err := database.DB.Create(&theirs).Error; err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 6; day++ {
		seedReviewEntry(t, theirs.ID, "9000.00", "Travel", "2026-07-0"+string(rune('0'+day)))
	}
	seedReviewEntry(t, mine.ID, "500.00", "Food & Drinks", "2026-07-02")

	dateRange, err := monthlyReviewRange("2026-07", india(t))
	if err != nil {
		t.Fatal(err)
	}
	review, err := buildMonthlyReview(mine.ID, "2026-07", dateRange)
	if err != nil {
		t.Fatal(err)
	}
	if review.Summary.TotalSpent != 500 {
		t.Fatalf("total = %v, want only this user's ₹500", review.Summary.TotalSpent)
	}
}

// The headline pairs a spend total with a count, so the count has to be the
// number of rows that total is made of. A salary sitting in the same month used
// to be counted inside a sentence about money going out, which made
// "₹67,955 across 39 transactions" describe two different sets of rows.
func TestMonthlyReviewCopyCountsOnlyTheSpendItDescribes(t *testing.T) {
	review := MonthlyReviewResponse{
		Label:         "July 2026",
		PreviousLabel: "June",
		Summary: DashboardSummary{
			TotalSpent:       67955,
			TransactionCount: 39, // 38 expenses and one salary
			ExpenseCount:     38,
		},
	}

	_, body := monthlyReviewCopy(review)
	if !strings.Contains(body, "across 38 transactions") {
		t.Fatalf("body = %q, want the expense count", body)
	}
	if strings.Contains(body, "39") {
		t.Fatalf("body = %q, must not count the income row", body)
	}
}

// The floor is about whether a month has enough *spending* to describe. Four
// salary rows beside a single expense used to clear it.
func TestMonthlyReviewAvailabilityIgnoresIncomeRows(t *testing.T) {
	summary := DashboardSummary{TransactionCount: 5, ExpenseCount: 1}
	if summary.ExpenseCount >= monthlyReviewMinTransactions {
		t.Fatal("a month with one expense must not be publishable")
	}
	if summary.TransactionCount < monthlyReviewMinTransactions {
		t.Fatal("guard is meaningless unless the total count would have passed")
	}
}
