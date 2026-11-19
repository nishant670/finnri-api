package http

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func createRealUsersTestUser(t *testing.T, username string, guest bool, mutate func(*models.User)) models.User {
	t.Helper()
	user := models.User{UUID: generateUUID(), Username: username, IsGuest: guest}
	if mutate != nil {
		mutate(&user)
	}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func TestAdminCountsLeaveOutTestUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)

	createRealUsersTestUser(t, "real_guest", true, nil)
	robot := createRealUsersTestUser(t, "robot_guest", true, func(user *models.User) {
		user.IsTest, user.TestReason = true, models.UserTestReasonTestLab
	})
	if err := database.DB.Create(&models.Entry{UserID: robot.ID, Type: "expense", Amount: 10, Title: "junk"}).Error; err != nil {
		t.Fatal(err)
	}

	type overview struct {
		Users struct {
			Total  int64 `json:"total"`
			Guests int64 `json:"guests"`
			Test   int64 `json:"test"`
		} `json:"users"`
		Engagement struct {
			ActiveInWindow int `json:"active_in_window"`
		} `json:"engagement"`
	}
	real := performJSONRequest[overview](t, router, http.MethodGet, "/v1/admin/overview", viewerToken, nil, http.StatusOK)
	all := performJSONRequest[overview](t, router, http.MethodGet, "/v1/admin/overview?include_test=true", viewerToken, nil, http.StatusOK)
	if real.Users.Guests != 1 || all.Users.Guests != 2 || real.Users.Total != all.Users.Total-1 || real.Users.Test != 1 {
		t.Fatalf("test guest should drop out of counts: real=%+v all=%+v", real.Users, all.Users)
	}
	// The two admins only have console sessions, which are not app use; the
	// robot's entry is the only activity, and it is a test account's.
	if real.Engagement.ActiveInWindow != 0 || all.Engagement.ActiveInWindow != 1 {
		t.Fatalf("only the robot's entry should count, and only with include_test: real=%d all=%d", real.Engagement.ActiveInWindow, all.Engagement.ActiveInWindow)
	}

	type list struct {
		Users []adminUserListItem `json:"users"`
		Total int64               `json:"total"`
	}
	listed := performJSONRequest[list](t, router, http.MethodGet, "/v1/admin/users?type=guest", viewerToken, nil, http.StatusOK)
	if listed.Total != 1 || listed.Users[0].Username != "real_guest" {
		t.Fatalf("guest list should hold only the real guest: %#v", listed.Users)
	}
	tests := performJSONRequest[list](t, router, http.MethodGet, "/v1/admin/users?type=test", viewerToken, nil, http.StatusOK)
	if tests.Total != 1 || !tests.Users[0].IsTest || tests.Users[0].TestReason != models.UserTestReasonTestLab {
		t.Fatalf("type=test should list the robot: %#v", tests.Users)
	}
	found := performJSONRequest[list](t, router, http.MethodGet, "/v1/admin/users?q=robot_guest", viewerToken, nil, http.StatusOK)
	if found.Total != 1 {
		t.Fatalf("a search should still find a test account: %#v", found.Users)
	}

	path := "/v1/admin/users/" + strconv.Itoa(int(robot.ID)) + "/test"
	performJSONRequest[map[string]any](t, router, http.MethodPatch, path, viewerToken, map[string]any{"is_test": false}, http.StatusForbidden)
	performJSONRequest[map[string]any](t, router, http.MethodPatch, path, supportToken, map[string]any{"is_test": true, "reason": "bot"}, http.StatusUnprocessableEntity)
	performJSONRequest[map[string]any](t, router, http.MethodPatch, path, supportToken, map[string]any{"reason": "internal"}, http.StatusUnprocessableEntity)
	cleared := performJSONRequest[map[string]any](t, router, http.MethodPatch, path, supportToken, map[string]any{"is_test": false}, http.StatusOK)
	if cleared["is_test"] != false || cleared["test_reason"] != "" {
		t.Fatalf("un-flagging should clear the reason: %#v", cleared)
	}
	marked := performJSONRequest[map[string]any](t, router, http.MethodPatch, path, supportToken, map[string]any{"is_test": true}, http.StatusOK)
	if marked["test_reason"] != models.UserTestReasonManual || marked["test_marked_at"] == nil {
		t.Fatalf("flagging without a reason should record manual: %#v", marked)
	}
	performJSONRequest[map[string]any](t, router, http.MethodPatch, "/v1/admin/users/999999/test", supportToken, map[string]any{"is_test": true}, http.StatusNotFound)
}

func TestAdminTrialsListsWhoToOfferCreditsTo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	now := time.Now().UTC()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	day := 24 * time.Hour

	trial := func(user models.User, startedAgo, endsIn time.Duration, remaining int) {
		t.Helper()
		expires := now.Add(endsIn)
		grant := models.CreditGrant{Source: billing.GrantSourceFreeTrial, CreditsGranted: 1000, CreditsRemaining: remaining, ValidFrom: now.Add(-startedAgo), ExpiresAt: &expires}
		if user.IsGuest {
			grant.GuestDeviceIDHash = billing.SubjectForGuestDeviceID(*user.DeviceID).GuestDeviceIDHash
		} else {
			id := user.ID
			grant.UserID = &id
		}
		if err := database.DB.Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
	}

	endingSoon := createRealUsersTestUser(t, "ending_soon", false, nil)
	trial(endingSoon, 27*day, 3*day, 990)
	inTrial := createRealUsersTestUser(t, "in_trial", false, nil)
	trial(inTrial, 10*day, 20*day, 1000)
	endedActive := createRealUsersTestUser(t, "ended_active", false, func(user *models.User) { user.LastActiveAt = ago(time.Hour) })
	trial(endedActive, 32*day, -2*day, 0)
	endedQuiet := createRealUsersTestUser(t, "ended_quiet", false, func(user *models.User) { user.LastActiveAt = ago(40 * day) })
	trial(endedQuiet, 70*day, -40*day, 0)
	covered := createRealUsersTestUser(t, "covered", false, nil)
	trial(covered, 40*day, -10*day, 0)
	if err := database.DB.Create(&models.UserSubscription{UserID: covered.ID, PlanID: monthly.ID, Status: "active", Provider: giftPassProvider, CurrentPeriodStart: now.Add(-day), CurrentPeriodEnd: now.Add(6 * day)}).Error; err != nil {
		t.Fatal(err)
	}
	paidBefore := createRealUsersTestUser(t, "paid_before", false, nil)
	trial(paidBefore, 70*day, -40*day, 0)
	if err := database.DB.Create(&models.UserSubscription{UserID: paidBefore.ID, PlanID: monthly.ID, Status: "expired", Provider: "razorpay", CurrentPeriodStart: now.Add(-40 * day), CurrentPeriodEnd: now.Add(-10 * day)}).Error; err != nil {
		t.Fatal(err)
	}
	robot := createRealUsersTestUser(t, "robot", true, func(user *models.User) {
		device := "robot-device"
		user.DeviceID = &device
		user.IsTest, user.TestReason = true, models.UserTestReasonTestLab
	})
	trial(robot, day, 5*day, 300)
	guest := createRealUsersTestUser(t, "guest", true, func(user *models.User) {
		device := "guest-device"
		user.DeviceID = &device
	})
	trial(guest, 25*day, 5*day, 120)
	guestID := guest.ID
	if err := database.DB.Create(&models.AIUsageLimitEvent{UserID: &guestID, ActionCode: "transaction_parse_receipt", Reason: "insufficient_ai_credits", CreatedAt: now.Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}

	type trials struct {
		Counts map[string]int  `json:"counts"`
		Users  []adminTrialRow `json:"users"`
	}
	names := func(rows []adminTrialRow) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.Username)
		}
		return out
	}
	soon := performJSONRequest[trials](t, router, http.MethodGet, "/v1/admin/trials?status=ending_soon", viewerToken, nil, http.StatusOK)
	if got := names(soon.Users); len(got) != 2 || got[0] != "ending_soon" || got[1] != "guest" {
		t.Fatalf("ending soon should be the two trials ending within 7 days, soonest first: %v", got)
	}
	if soon.Users[0].DaysLeft == nil || *soon.Users[0].DaysLeft != 3 || soon.Users[0].TrialCreditsLeft != 990 {
		t.Fatalf("unexpected trial detail: %#v", soon.Users[0])
	}
	want := map[string]int{"ending_soon": 2, "in_trial": 3, "ended_unpaid": 2, "ended_active": 1, "credit_limit": 1}
	for status, count := range want {
		if soon.Counts[status] != count {
			t.Fatalf("count %s = %d, want %d (all %v)", status, soon.Counts[status], count, soon.Counts)
		}
	}
	ended := performJSONRequest[trials](t, router, http.MethodGet, "/v1/admin/trials?status=ended_unpaid", viewerToken, nil, http.StatusOK)
	if got := names(ended.Users); len(got) != 2 || got[0] != "ended_active" || got[1] != "ended_quiet" {
		t.Fatalf("ended unpaid should skip the covered and the past buyer, most recent first: %v", got)
	}
	active := performJSONRequest[trials](t, router, http.MethodGet, "/v1/admin/trials?status=ended_active", viewerToken, nil, http.StatusOK)
	if got := names(active.Users); len(got) != 1 || got[0] != "ended_active" {
		t.Fatalf("ended active: %v", got)
	}
	limited := performJSONRequest[trials](t, router, http.MethodGet, "/v1/admin/trials?status=credit_limit", viewerToken, nil, http.StatusOK)
	if got := names(limited.Users); len(got) != 1 || got[0] != "guest" || limited.Users[0].CreditLimitHits != 1 {
		t.Fatalf("credit limit: %#v", limited.Users)
	}
	withTest := performJSONRequest[trials](t, router, http.MethodGet, "/v1/admin/trials?status=ending_soon&include_test=true", viewerToken, nil, http.StatusOK)
	if withTest.Counts["ending_soon"] != 3 {
		t.Fatalf("include_test should add the robot: %v", withTest.Counts)
	}
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/admin/trials?status=everyone", viewerToken, nil, http.StatusUnprocessableEntity)
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/admin/trials?days=0", viewerToken, nil, http.StatusUnprocessableEntity)
}

func TestAdminPaidConversionCountsRealUsersOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	now := time.Now().UTC()
	secondary := createRealUsersTestUser(t, "secondary", false, func(user *models.User) {
		user.IsTest, user.TestReason = true, models.UserTestReasonInternal
	})
	payer := createRealUsersTestUser(t, "payer", false, nil)
	createRealUsersTestUser(t, "browser", false, nil)
	for _, id := range []uint{secondary.ID, payer.ID} {
		if err := database.DB.Create(&models.UserSubscription{UserID: id, PlanID: monthly.ID, Status: "active", Provider: "razorpay", CurrentPeriodStart: now.Add(-24 * time.Hour), CurrentPeriodEnd: now.Add(10 * 24 * time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	revenue := performJSONRequest[struct {
		ActiveSubscribers int     `json:"active_subscribers"`
		PaidConversion    float64 `json:"paid_conversion_percent"`
	}](t, router, http.MethodGet, "/v1/admin/revenue", viewerToken, nil, http.StatusOK)
	// Real registered: the viewer admin, payer, browser. One of them pays.
	if revenue.ActiveSubscribers != 2 || revenue.PaidConversion != 33.33 {
		t.Fatalf("money counts both passes, conversion only the real payer: %+v", revenue)
	}
}
