package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

type bulkGiftResponse struct {
	Results []bulkGiftResult `json:"results"`
	Counts  map[string]int   `json:"counts"`
}

func postBulkGift(t *testing.T, router *gin.Engine, token, key string, body map[string]any, status int) bulkGiftResponse {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/gifts/bulk", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-test-token")
	request.Header.Set("X-Admin-Session", token)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != status {
		t.Fatalf("bulk gift status = %d, want %d, body = %s", recorder.Code, status, recorder.Body.String())
	}
	var response bulkGiftResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	return response
}

func bulkStatuses(response bulkGiftResponse) map[uint]string {
	out := map[uint]string{}
	for _, result := range response.Results {
		out[result.UserID] = result.Status
		if result.Reason != "" {
			out[result.UserID] += ":" + result.Reason
		}
	}
	return out
}

func TestAdminBulkGiftPassReachesEveryoneOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	weekly := seedPurchasablePlan(t, "weekly_pass", "weekly", 7900, 800)
	now := time.Now().UTC()

	fresh := createRealUsersTestUser(t, "fresh", false, nil)
	paying := createRealUsersTestUser(t, "paying", false, nil)
	paidEnd := now.Add(3 * 24 * time.Hour)
	if err := database.DB.Create(&models.UserSubscription{UserID: paying.ID, PlanID: weekly.ID, Status: "active", Provider: "razorpay", CurrentPeriodStart: now.Add(-4 * 24 * time.Hour), CurrentPeriodEnd: paidEnd}).Error; err != nil {
		t.Fatal(err)
	}
	device := "guest-device-1"
	guest := createRealUsersTestUser(t, "guest", true, func(user *models.User) { user.DeviceID = &device })
	deviceless := createRealUsersTestUser(t, "deviceless", true, nil)

	body := map[string]any{
		"user_ids":  []uint{fresh.ID, paying.ID, guest.ID, deviceless.ID, 999999, fresh.ID},
		"kind":      "pass",
		"plan_code": "weekly_pass",
		"periods":   1,
		"note":      "Diwali 2026",
		"message":   map[string]string{"title": "Happy Diwali!", "body": "A free week of Finnri, on us."},
	}
	postBulkGift(t, router, viewerToken, "diwali-1", body, http.StatusForbidden)
	first := postBulkGift(t, router, supportToken, "diwali-1", body, http.StatusOK)
	want := map[uint]string{
		fresh.ID:      "gifted",
		paying.ID:     "gifted",
		guest.ID:      "credits_instead",
		deviceless.ID: "skipped:guest_without_device",
		999999:        "skipped:user_not_found",
	}
	got := bulkStatuses(first)
	if len(first.Results) != len(want) {
		t.Fatalf("duplicates should collapse: %#v", first.Results)
	}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("user %d = %q, want %q (all %v)", id, got[id], status, got)
		}
	}

	var queued models.UserSubscription
	if err := database.DB.Where("user_id = ? AND provider = ?", paying.ID, giftPassProvider).First(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if !queued.CurrentPeriodStart.Equal(paidEnd) {
		t.Fatalf("the gift should queue behind the paid pass: starts %s, paid ends %s", queued.CurrentPeriodStart, paidEnd)
	}
	var guestGrant models.CreditGrant
	if err := database.DB.Where("guest_device_id_hash = ?", billing.HashUsageKey(device)).First(&guestGrant).Error; err != nil {
		t.Fatalf("the guest's credits should sit on its device: %v", err)
	}
	if guestGrant.CreditsGranted != 800 || guestGrant.ExpiresAt == nil || guestGrant.ExpiresAt.Sub(now) < 6*24*time.Hour {
		t.Fatalf("the guest should get the pass's credits for a week: %+v", guestGrant)
	}
	var notice models.Notification
	if err := database.DB.Where("user_id = ?", fresh.ID).First(&notice).Error; err != nil || notice.Title != "Happy Diwali!" {
		t.Fatalf("the occasion message should be what they see: %+v %v", notice, err)
	}

	// The same batch again gifts nobody twice and sends nothing new.
	again := postBulkGift(t, router, supportToken, "diwali-1", body, http.StatusOK)
	if again.Counts["already_gifted"] != 3 || again.Counts["gifted"] != 0 || again.Counts["credits_instead"] != 0 {
		t.Fatalf("a replay should be a no-op: %v", again.Counts)
	}
	var passes, grants, notices int64
	database.DB.Model(&models.UserSubscription{}).Where("provider = ?", giftPassProvider).Count(&passes)
	database.DB.Model(&models.CreditGrant{}).Where("guest_device_id_hash = ?", billing.HashUsageKey(device)).Count(&grants)
	database.DB.Model(&models.Notification{}).Count(&notices)
	if passes != 2 || grants != 1 || notices != 3 {
		t.Fatalf("replay duplicated work: passes=%d guest grants=%d notifications=%d", passes, grants, notices)
	}
}

func TestAdminBulkGiftCreditsAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)

	registered := createRealUsersTestUser(t, "registered", false, nil)
	device := "guest-device-2"
	guest := createRealUsersTestUser(t, "guest", true, func(user *models.User) { user.DeviceID = &device })
	expires := time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
	response := postBulkGift(t, router, supportToken, "winback-1", map[string]any{
		"user_ids": []uint{registered.ID, guest.ID}, "kind": "credits", "credits": 200, "expires_at": expires, "note": "Win-back",
	}, http.StatusOK)
	if response.Counts["gifted"] != 2 {
		t.Fatalf("both should get credits: %v", response.Results)
	}
	var userGrant, guestGrant models.CreditGrant
	if err := database.DB.Where("user_id = ? AND source = ?", registered.ID, "manual_adjustment").First(&userGrant).Error; err != nil || userGrant.CreditsGranted != 200 {
		t.Fatalf("registered grant: %+v %v", userGrant, err)
	}
	if err := database.DB.Where("guest_device_id_hash = ?", billing.HashUsageKey(device)).First(&guestGrant).Error; err != nil || guestGrant.UserID != nil {
		t.Fatalf("guest grant should be on the device: %+v %v", guestGrant, err)
	}
	var notice models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", registered.ID, creditsGiftNotificationType).First(&notice).Error; err != nil {
		t.Fatalf("credits gift should notify: %v", err)
	}

	tooMany := make([]uint, maxBulkGiftUsers+1)
	for index := range tooMany {
		tooMany[index] = uint(index + 1)
	}
	for name, testCase := range map[string]struct {
		key  string
		body map[string]any
	}{
		"no key":        {"", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": 10, "note": "x"}},
		"no note":       {"k1", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": 10}},
		"bad kind":      {"k2", map[string]any{"user_ids": []uint{registered.ID}, "kind": "cash", "note": "x"}},
		"no users":      {"k3", map[string]any{"user_ids": []uint{}, "kind": "credits", "credits": 10, "note": "x"}},
		"too many":      {"k4", map[string]any{"user_ids": tooMany, "kind": "credits", "credits": 10, "note": "x"}},
		"past expiry":   {"k5", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": 10, "note": "x", "expires_at": "2020-01-01T00:00:00Z"}},
		"half message":  {"k6", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": 10, "note": "x", "message": map[string]string{"title": "Hi"}}},
		"unknown plan":  {"k7", map[string]any{"user_ids": []uint{registered.ID}, "kind": "pass", "plan_code": "nope", "note": "x"}},
		"zero credits":  {"k8", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": 0, "note": "x"}},
		"credits limit": {"k9", map[string]any{"user_ids": []uint{registered.ID}, "kind": "credits", "credits": maxBulkGiftCredits + 1, "note": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			postBulkGift(t, router, supportToken, testCase.key, testCase.body, http.StatusUnprocessableEntity)
		})
	}
}

func TestAdminCreditAdjustmentForGuestLandsOnDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	device := "guest-device-3"
	guest := createRealUsersTestUser(t, "guest", true, func(user *models.User) { user.DeviceID = &device })
	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/admin/users/"+strconv.Itoa(int(guest.ID))+"/credits/adjustments", supportToken, map[string]any{"credits": 50, "reason_code": "support_test"}, http.StatusCreated)
	summary, err := buildCreditSummary(billing.SubjectForGuestDeviceID(device), true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalCreditsRemaining != 50 {
		t.Fatalf("the guest's own balance should show the credits: %+v", summary)
	}
}
