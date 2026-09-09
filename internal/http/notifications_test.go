package http

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestNotificationPaginationValidation(t *testing.T) {
	page, pageSize, fields := parseNotificationPagination("2", "50")
	if len(fields) != 0 {
		t.Fatalf("expected valid pagination, got %v", fields)
	}
	if page != 2 || pageSize != 50 {
		t.Fatalf("unexpected pagination: page=%d pageSize=%d", page, pageSize)
	}

	_, _, fields = parseNotificationPagination("0", "101")
	if fields["page"] == nil || fields["page_size"] == nil {
		t.Fatalf("expected page and page_size validation errors, got %v", fields)
	}
}

func TestNotificationsSkipStaticBearerGate(t *testing.T) {
	if !skipsStaticBearer("/v1/notifications/unread-count") {
		t.Fatal("notifications should use user-session auth, not the static bearer gate")
	}
}

// The API used to write a notification for every entry the user created,
// updated, or deleted — a receipt for an action they had just performed and
// watched succeed. That buried the alerts that actually matter. The inbox is
// reserved for events the user could not otherwise know about.
func TestEntryMutationsCreateNoSelfNotifications(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "notification-noise-device",
		}, http.StatusOK,
	)

	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)
	if len(accounts) == 0 {
		t.Fatal("expected a default account for the guest user")
	}

	entry := map[string]any{
		"title": "Chai", "type": "expense", "amount": 80, "currency": "INR",
		"source": "manual", "account_id": accounts[0].ID, "mode": "Cash",
		"category": "Food & Drinks", "merchant": "Tea Stall",
		"date": "2026-07-12", "time": "09:15",
	}

	saved := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", auth.Token, entry, http.StatusCreated,
	)
	assertNoTransactionNotifications(t, "create")

	entry["amount"] = 95
	performJSONRequest[models.Entry](
		t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", saved.ID),
		auth.Token, entry, http.StatusOK,
	)
	assertNoTransactionNotifications(t, "update")

	performJSONRequest[map[string]any](
		t, router, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", saved.ID),
		auth.Token, nil, http.StatusOK,
	)
	assertNoTransactionNotifications(t, "delete")
}

func assertNoTransactionNotifications(t *testing.T, stage string) {
	t.Helper()

	var found []models.Notification
	if err := database.DB.Where("type LIKE ?", "transaction.%").Find(&found).Error; err != nil {
		t.Fatalf("failed to read notifications after %s: %v", stage, err)
	}
	if len(found) != 0 {
		t.Fatalf("entry %s wrote %d self-notification(s), first: type=%q title=%q",
			stage, len(found), found[0].Type, found[0].Title)
	}
}

func TestNotificationScopeAlwaysScopesUser(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=localhost user=test dbname=test sslmode=disable",
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}

	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return notificationScope(tx, 42).Where("id = ?", 9).Delete(&models.Notification{})
	})
	if !strings.Contains(sql, "user_id") || !strings.Contains(sql, "id") ||
		!strings.Contains(sql, "42") || !strings.Contains(sql, "9") {
		t.Fatalf("ownership predicate missing from notification mutation: %s", sql)
	}
}
