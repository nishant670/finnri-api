package http

import (
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestBudgetAlertCreatesWarningOncePerBudgetPeriod(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)

	budget := models.Budget{
		UserID: user.ID, Name: "Food", Period: budgetPeriodMonthly, Category: "Food",
		LimitAmount: testMoney("1000"), Currency: "INR", AlertThresholdPercent: 80, Active: true,
	}
	if err := database.DB.Create(&budget).Error; err != nil {
		t.Fatal(err)
	}

	first := createBudgetTestEntry(t, user.ID, "Food", "2026-07-05", "750")
	if err := maybeCreateBudgetAlertsForEntry(first); err != nil {
		t.Fatal(err)
	}
	assertBudgetNotificationCount(t, user.ID, "budget.warning", 0)

	second := createBudgetTestEntry(t, user.ID, "Food", "2026-07-10", "50")
	if err := maybeCreateBudgetAlertsForEntry(second); err != nil {
		t.Fatal(err)
	}
	assertBudgetNotificationCount(t, user.ID, "budget.warning", 1)

	if err := maybeCreateBudgetAlertsForEntry(second); err != nil {
		t.Fatal(err)
	}
	assertBudgetNotificationCount(t, user.ID, "budget.warning", 1)
}

func TestBudgetAlertCreatesExceededForLargeJumpAndMatchesCategory(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)

	budget := models.Budget{
		UserID: user.ID, Name: "Food", Period: budgetPeriodMonthly, Category: "Food",
		LimitAmount: testMoney("1000"), Currency: "INR", AlertThresholdPercent: 80, Active: true,
	}
	if err := database.DB.Create(&budget).Error; err != nil {
		t.Fatal(err)
	}

	transport := createBudgetTestEntry(t, user.ID, "Travel", "2026-07-08", "1200")
	if err := maybeCreateBudgetAlertsForEntry(transport); err != nil {
		t.Fatal(err)
	}
	assertBudgetNotificationCount(t, user.ID, "budget.exceeded", 0)

	food := createBudgetTestEntry(t, user.ID, "Food", "2026-07-09", "1200")
	if err := maybeCreateBudgetAlertsForEntry(food); err != nil {
		t.Fatal(err)
	}
	assertBudgetNotificationCount(t, user.ID, "budget.exceeded", 1)
	assertBudgetNotificationCount(t, user.ID, "budget.warning", 0)
}

func createBudgetTestUser(t *testing.T) models.User {
	t.Helper()
	user := models.User{UUID: t.Name(), Username: t.Name(), IsGuest: true}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func createBudgetTestEntry(t *testing.T, userID uint, category, date, amount string) models.Entry {
	t.Helper()
	entry := models.Entry{
		UserID: userID, Title: category, Type: "expense", Amount: testMoney(amount),
		Currency: "INR", Source: "manual", Mode: "Cash", Category: category, Date: date,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	return entry
}

func assertBudgetNotificationCount(t *testing.T, userID uint, notificationType string, want int64) {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ?", userID, notificationType).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", notificationType, count, want)
	}
}
