package http

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

// The monthly review is the app's only closed retention loop: one message a
// month, on a month that has finished, that is worth opening because it says
// something the user does not already know.
//
// Everything here is arranged around not becoming the thing S1 deleted. One
// notification per user per month, enforced by a unique index rather than by
// the job being careful; nothing sent for a month too thin to describe; and no
// percentage published against a base too small to divide by, which is S5's
// floor, reused rather than reinvented.

const (
	// A review is emitted in the first few days of the month, not only on the
	// 1st. A ticker that has to fire on one specific day is a ticker that
	// silently skips a month whenever the process happens to be restarting, and
	// the unique index already makes a late run harmless. Three days keeps a
	// review from ever arriving stale enough to feel like a mistake.
	monthlyReviewWindowDays = 3

	// Below this the month cannot carry a story. "₹450 across 1 transaction"
	// is a notification that costs more trust than it earns, and it is the same
	// count floor a comparison needs to be publishable.
	monthlyReviewMinTransactions = comparisonMinBaseCount

	// A category has to move by at least this much to be called the month's
	// biggest change. Without it, a quiet month headlines a ₹40 swing.
	monthlyReviewMinCategoryMove = comparisonMinBaseAmount

	monthlyReviewNotificationType = "monthly_review"
)

type MonthlyReviewChange struct {
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
	// PreviousAmount and Change describe the same movement two ways. Change is
	// only meaningful when Comparable is true.
	PreviousAmount float64 `json:"previous_amount"`
	Change         float64 `json:"change"`
	Comparable     bool    `json:"comparable"`
	Direction      string  `json:"direction"`
}

type MonthlyReviewResponse struct {
	Month string `json:"month"`
	// Label and PreviousLabel are month names, not money, so the server is
	// allowed to write them. Every figure below travels as a number and is
	// formatted by the app.
	Label         string `json:"label"`
	PreviousLabel string `json:"previous_label"`
	StartDate     string `json:"start_date"`
	EndDate       string `json:"end_date"`

	// Available is false when the month holds too little to review. The screen
	// says so rather than rendering a story about ₹0.
	Available bool `json:"available"`

	Summary       DashboardSummary      `json:"summary"`
	TopCategories []DashboardCategory   `json:"top_categories"`
	TopMerchants  []DashboardMerchant   `json:"top_merchants"`
	DailySpending []DashboardDailySpend `json:"daily_spending"`
	BiggestChange *MonthlyReviewChange  `json:"biggest_change"`
	BusiestDay    *DashboardDailySpend  `json:"busiest_day"`

	// NotifiedAt is set when the review notification for this month has already
	// been sent, so the screen can be honest about whether it is showing
	// something the user was told or something they went looking for.
	NotifiedAt *time.Time `json:"notified_at,omitempty"`
}

// monthlyReviewRange turns "2026-08" into the whole of that calendar month.
//
// The previous window comes from previousComparisonWindow, unchanged: a range
// starting on the 1st and ending inside the same month is exactly the
// month-to-date shape S5 taught it, and for a full month it resolves to the
// full previous month — including the short-February case, where March is
// compared against all 28 days of February rather than a date February does
// not have.
func monthlyReviewRange(month string, location *time.Location) (dashboardRange, error) {
	parsed, err := time.ParseInLocation("2006-01", strings.TrimSpace(month), location)
	if err != nil {
		return dashboardRange{}, fmt.Errorf("month must use YYYY-MM")
	}
	start := time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, location)
	end := start.AddDate(0, 1, -1)
	previousStart, previousEnd, kind := previousComparisonWindow(start, end)
	return dashboardRange{
		Start:          start,
		End:            end,
		PreviousStart:  previousStart,
		PreviousEnd:    previousEnd,
		Days:           end.Day(),
		ComparisonKind: kind,
	}, nil
}

// mostRecentClosedMonth is the only month a review may ever describe. A review
// of a month still being lived in would be wrong by whatever happens next.
func mostRecentClosedMonth(now time.Time) string {
	return now.AddDate(0, 0, -now.Day()).Format("2006-01")
}

func monthlyReviewLabel(start time.Time) string {
	return start.Format("January 2006")
}

func buildMonthlyReview(userID uint, month string, dateRange dashboardRange) (MonthlyReviewResponse, error) {
	review := MonthlyReviewResponse{
		Month:         month,
		Label:         monthlyReviewLabel(dateRange.Start),
		PreviousLabel: dateRange.PreviousStart.Format("January"),
		StartDate:     dateRange.Start.Format("2006-01-02"),
		EndDate:       dateRange.End.Format("2006-01-02"),
		TopCategories: []DashboardCategory{},
		TopMerchants:  []DashboardMerchant{},
		DailySpending: []DashboardDailySpend{},
	}

	dashboard, err := buildDashboardFromDB(userID, dateRange)
	if err != nil {
		return MonthlyReviewResponse{}, err
	}
	review.Summary = dashboard.Summary
	review.TopCategories = dashboard.TopCategories
	review.TopMerchants = dashboard.TopMerchants
	review.DailySpending = dashboard.DailySpending
	// Gated on *expenses*, not every entry. A month holding one spend and four
	// salary rows used to clear the floor and then describe itself as
	// "₹450 across 1 transaction" — precisely the review this floor exists to
	// suppress, because it costs more trust than it earns.
	review.Available = dashboard.Summary.ExpenseCount >= monthlyReviewMinTransactions

	previousCategories, err := loadDashboardPreviousCategoryTotals(
		userID,
		dateRange.PreviousStart.Format("2006-01-02"),
		dateRange.PreviousEnd.Format("2006-01-02"),
	)
	if err != nil {
		return MonthlyReviewResponse{}, err
	}
	review.BiggestChange = biggestCategoryChange(dashboard.TopCategories, previousCategories)
	review.BusiestDay = busiestDay(dashboard.DailySpending)

	return review, nil
}

// biggestCategoryChange ranks by rupees moved, not by percentage.
//
// Percentage ranking picks the smallest category every time: ₹600 becoming
// ₹3,000 is +400% and ₹20,000 becoming ₹26,000 is +30%, and the second is the
// one that changed the month. Ranking on the absolute movement puts the real
// story first, and the percentage still travels for the screen to show —
// but only when the previous period was substantial enough to divide by, which
// is S5's floor rather than a second opinion about it.
//
// A category that is new this month has no previous figure and therefore no
// honest percentage, and it is still allowed to win: starting to spend on
// something is a change worth naming, and the copy names the category rather
// than quoting a number at it.
func biggestCategoryChange(
	categories []DashboardCategory,
	previous map[string]comparisonBase,
) *MonthlyReviewChange {
	var best *MonthlyReviewChange
	bestMove := 0.0

	for _, category := range categories {
		base := previous[category.Category]
		move := math.Abs(category.Amount - base.Amount)
		if move < monthlyReviewMinCategoryMove || move <= bestMove {
			continue
		}
		direction := "higher"
		if category.Amount < base.Amount {
			direction = "lower"
		}
		change := MonthlyReviewChange{
			Category:       category.Category,
			Amount:         category.Amount,
			PreviousAmount: base.Amount,
			Direction:      direction,
		}
		if base.publishable() {
			change.Change = percentageChange(category.Amount, base.Amount)
			change.Comparable = true
		}
		bestMove = move
		best = &change
	}

	return best
}

func busiestDay(days []DashboardDailySpend) *DashboardDailySpend {
	var best *DashboardDailySpend
	for index := range days {
		if best == nil || days[index].Amount > best.Amount {
			best = &days[index]
		}
	}
	if best == nil || best.Amount <= 0 {
		return nil
	}
	return best
}

// monthlyReviewCopy writes the notification.
//
// This is the one place in the app that formats money without `formatMoney`,
// and it has to be: the job composing this sentence runs on a ticker at 03:00
// with no client attached, so there is no app to ask. formatReviewMoney below
// mirrors the app's rules — Indian grouping, paise dropped above ₹100 — and is
// pinned by tests against the same examples.
//
// Clauses are dropped rather than faked. A month with no publishable comparison
// says nothing about last month; a month with no standout category names none.
// "0% under July" and "Biggest change: Misc" are both worse than a shorter
// sentence.
func monthlyReviewCopy(review MonthlyReviewResponse) (string, string) {
	title := fmt.Sprintf("%s in review", review.Label)

	sentence := fmt.Sprintf(
		"%s across %s",
		formatReviewMoney(review.Summary.TotalSpent),
		pluralize(review.Summary.ExpenseCount, "transaction"),
	)
	if review.Summary.SpendChangeComparable {
		sentence += fmt.Sprintf(
			", %s %s %s",
			formatChangeMagnitude(review.Summary.SpendChange),
			overOrUnder(review.Summary.SpendChange),
			review.PreviousLabel,
		)
	}
	sentence += "."

	if review.BiggestChange != nil {
		sentence += fmt.Sprintf(" Biggest change: %s.", review.BiggestChange.Category)
	}
	return title, sentence
}

func overOrUnder(change float64) string {
	if change < 0 {
		return "under"
	}
	return "over"
}

func pluralize(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// formatReviewMoney is the app's formatMoney, in Go, for server-authored copy.
//
// Duplicating a formatter is normally the bug this codebase has a lint rule
// against. The alternative here is worse: the existing notification copy uses
// Money.String(), which renders ₹40,091 as "₹40091.00" — machine output in the
// one sentence whose entire job is to be worth reading. Nothing else may call
// this; it exists for notification bodies, which no client can format.
func formatReviewMoney(amount float64) string {
	negative := amount < 0
	if negative {
		amount = -amount
	}
	// Paise below ₹100, whole rupees above it — the app's PAISE_THRESHOLD.
	text := fmt.Sprintf("%.2f", amount)
	if amount >= 100 {
		text = fmt.Sprintf("%.0f", math.Round(amount))
	}

	whole, fraction, _ := strings.Cut(text, ".")
	grouped := groupIndianDigits(whole)
	if fraction != "" {
		grouped += "." + fraction
	}
	if negative {
		return "-₹" + grouped
	}
	return "₹" + grouped
}

// groupIndianDigits renders 2-2-3 grouping: 200000 becomes 2,00,000.
func groupIndianDigits(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	head := digits[:len(digits)-3]
	tail := digits[len(digits)-3:]

	parts := []string{}
	for len(head) > 2 {
		parts = append([]string{head[len(head)-2:]}, parts...)
		head = head[:len(head)-2]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(parts, ",") + "," + tail
}

// StartMonthlyReviewJob emits reviews for the closed month.
//
// Hourly rather than daily: a daily ticker started at 14:00 fires at 14:00, so
// a review would land in the middle of the afternoon of whichever day the
// process last restarted. Hourly, combined with the send window and the unique
// index, means the review goes out within an hour of the server having a chance
// to send it, once.
func StartMonthlyReviewJob(cfg *config.Config) {
	go func() {
		location := loadLocationOrIndia("", cfg.TZDefault)
		run := func() {
			sent, err := runMonthlyReviewOnce(time.Now().In(location), location)
			if err != nil {
				log.Printf("monthly review job failed: %v", err)
				return
			}
			if sent > 0 {
				log.Printf("monthly review job sent %d reviews", sent)
			}
		}
		run()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			run()
		}
	}()
}

func runMonthlyReviewOnce(now time.Time, location *time.Location) (int, error) {
	if now.Day() > monthlyReviewWindowDays {
		return 0, nil
	}
	month := mostRecentClosedMonth(now)
	dateRange, err := monthlyReviewRange(month, location)
	if err != nil {
		return 0, err
	}

	var userIDs []uint
	if err := database.DB.Model(&models.Entry{}).
		Where("entries.date >= ? AND entries.date <= ?",
			dateRange.Start.Format("2006-01-02"), dateRange.End.Format("2006-01-02")).
		Distinct("user_id").Pluck("user_id", &userIDs).Error; err != nil {
		return 0, err
	}

	sent := 0
	for _, userID := range userIDs {
		delivered, err := sendMonthlyReview(userID, month, dateRange)
		if err != nil {
			// One user's failure is not the cohort's. The unique index means a
			// retry next hour is free, so log and carry on.
			log.Printf("monthly review for user %d failed: %v", userID, err)
			continue
		}
		if delivered {
			sent++
		}
	}
	return sent, nil
}

// sendMonthlyReview writes the review and its notification, or does nothing.
//
// The insert into monthly_reviews is what claims the month. Racing callers —
// two ticks overlapping, or the manual trigger firing while the job runs —
// resolve in the database: the second insert violates the unique index and this
// returns false rather than sending a second notification.
func sendMonthlyReview(userID uint, month string, dateRange dashboardRange) (bool, error) {
	review, err := buildMonthlyReview(userID, month, dateRange)
	if err != nil {
		return false, err
	}
	if !review.Available {
		return false, nil
	}

	title, body := monthlyReviewCopy(review)
	actionURL := monthlyReviewActionURL(month)

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.MonthlyReview{}).
			Where("user_id = ? AND month = ?", userID, month).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return errMonthlyReviewAlreadySent
		}

		notification := models.Notification{
			UserID:    userID,
			Type:      monthlyReviewNotificationType,
			Title:     title,
			Body:      body,
			ActionURL: actionURL,
		}
		if err := tx.Create(&notification).Error; err != nil {
			return err
		}
		record := models.MonthlyReview{
			UserID:           userID,
			Month:            month,
			TotalSpent:       models.Money(math.Round(review.Summary.TotalSpent * 100)),
			TransactionCount: review.Summary.TransactionCount,
			NotificationID:   &notification.ID,
		}
		return tx.Create(&record).Error
	})
	if errors.Is(err, errMonthlyReviewAlreadySent) || isUniqueViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	sendUserPush(database.DB, userID, title, body, map[string]any{"action_url": actionURL})
	return true, nil
}

var errMonthlyReviewAlreadySent = errors.New("monthly review already sent")

// isUniqueViolation covers the race the pre-check cannot: two transactions both
// counting zero before either inserts. Postgres and the sqlite used in tests
// word it differently, so this matches on the shared vocabulary rather than a
// driver-specific code.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "unique violation")
}

func monthlyReviewActionURL(month string) string {
	return "/monthly-review/" + month
}

// GET /v1/monthly-review?month=YYYY-MM
func (s *Server) getMonthlyReview(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)

	month := strings.TrimSpace(c.Query("month"))
	if month == "" {
		month = mostRecentClosedMonth(time.Now().In(location))
	}
	dateRange, err := monthlyReviewRange(month, location)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_month",
			"fields": gin.H{"month": err.Error()},
		})
		return
	}
	// A month still being lived in is not reviewable: every figure would change
	// before the month it describes was over.
	if !dateRange.End.Before(time.Now().In(location)) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_month",
			"fields": gin.H{"month": "must be a month that has already ended"},
		})
		return
	}

	review, err := buildMonthlyReview(userID, month, dateRange)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_monthly_review"})
		return
	}

	var record models.MonthlyReview
	if err := database.DB.Where("user_id = ? AND month = ?", userID, month).
		First(&record).Error; err == nil {
		notifiedAt := record.CreatedAt
		review.NotifiedAt = &notifiedAt
	}

	c.JSON(http.StatusOK, review)
}

// POST /v1/monthly-review/send
//
// The same emission the job performs, for the calling user only, and equally
// idempotent. It exists because the job's trigger is a date: without this, the
// only way to see what the 1st of the month produces is to wait for it, or to
// move the server's clock — and a retention notification nobody can look at
// before it ships is one nobody has read.
func (s *Server) sendMonthlyReviewNow(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
	month := mostRecentClosedMonth(time.Now().In(location))

	dateRange, err := monthlyReviewRange(month, location)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_send_monthly_review"})
		return
	}
	sent, err := sendMonthlyReview(userID, month, dateRange)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_send_monthly_review"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"month": month, "sent": sent})
}
