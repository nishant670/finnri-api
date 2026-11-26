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

	"finnri/internal/database"
	"finnri/internal/models"
)

func resetClientWritesForTest() {
	clientWrites.Lock()
	clientWrites.at = map[string]time.Time{}
	clientWrites.Unlock()
}

func clientInfoFor(headers map[string]string) clientInfo {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}
	return clientInfoFromRequest(c)
}

func TestClientInfoFromAppHeadersAndUserAgents(t *testing.T) {
	app := clientInfoFor(map[string]string{
		"X-Finnri-Platform": "Android", "X-Finnri-App-Version": "1.0.0", "X-Finnri-Build": "15", "X-Finnri-OS": "Android 15",
		"X-Finnri-Device": "Samsung SM-S918B<script>", "X-Finnri-Form-Factor": "phone", "X-Finnri-Variant": "play", "X-Finnri-Test-Device": "1",
		"User-Agent": "okhttp/4.12.0",
	})
	if app.Platform != "android" || app.Device != "Samsung SM-S918Bscript" || app.AppBuild != "15" || app.Variant != "play" || !app.IsTestDevice || app.FormFactor != "phone" {
		t.Fatalf("app headers: %+v", app)
	}
	for name, testCase := range map[string]struct {
		ua                         string
		platform, form, os, device string
	}{
		"older app":      {"okhttp/4.12.0", "android", "unknown", "", "Older app build"},
		"windows chrome": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36", "web", "desktop", "Windows", "Chrome on Windows"},
		"mac safari":     {"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15", "web", "desktop", "macOS", "Safari on macOS"},
		"iphone safari":  {"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1", "web", "phone", "iOS", "Safari on iOS"},
		"android phone":  {"Mozilla/5.0 (Linux; Android 15; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Mobile Safari/537.36", "web", "phone", "Android", "Chrome on Android"},
		"android tablet": {"Mozilla/5.0 (Linux; Android 14; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36", "web", "tablet", "Android", "Chrome on Android"},
		"edge":           {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36 Edg/131.0", "web", "desktop", "Windows", "Edge on Windows"},
		"curl":           {"curl/8.4.0", "unknown", "unknown", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			info := clientInfoFor(map[string]string{"User-Agent": testCase.ua})
			if info.Platform != testCase.platform || info.FormFactor != testCase.form || info.OS != testCase.os || info.Device != testCase.device {
				t.Fatalf("%s: %+v", testCase.ua, info)
			}
		})
	}
	if a, b := clientInfoFor(map[string]string{"X-Finnri-Platform": "android", "X-Finnri-Device": "Pixel 8", "X-Finnri-App-Version": "1.0.0"}).key(),
		clientInfoFor(map[string]string{"X-Finnri-Platform": "android", "X-Finnri-Device": "Pixel 8", "X-Finnri-App-Version": "1.0.1"}).key(); a != b {
		t.Fatal("an app update should keep the same client row")
	}
}

func appHeaders(device, build string, test bool) map[string]string {
	headers := map[string]string{
		"X-Finnri-Platform": "android", "X-Finnri-App-Version": "1.0.0", "X-Finnri-Build": build, "X-Finnri-OS": "Android 15",
		"X-Finnri-Device": device, "X-Finnri-Form-Factor": "phone", "X-Finnri-Variant": "play",
	}
	if test {
		headers["X-Finnri-Test-Device"] = "1"
	}
	return headers
}

func requestWithHeaders(t *testing.T, router *gin.Engine, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestGuestOnPlayTestDeviceIsTaggedAndClientsRecorded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	resetClientWritesForTest()
	router := smokeRouter(t)

	type guestResponse struct {
		Token string      `json:"token"`
		User  models.User `json:"user"`
	}
	signIn := func(device string, headers map[string]string) guestResponse {
		recorder := requestWithHeaders(t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": device}, headers)
		if recorder.Code != http.StatusOK {
			t.Fatalf("guest sign-in = %d %s", recorder.Code, recorder.Body.String())
		}
		var response guestResponse
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		return response
	}
	robot := signIn("robot-device-abcdef", appHeaders("Google Pixel 6", "15", true))
	person := signIn("person-device-abcdef", appHeaders("Samsung SM-A546E", "15", false))

	var robotRow, personRow models.User
	database.DB.First(&robotRow, robot.User.ID)
	database.DB.First(&personRow, person.User.ID)
	if !robotRow.IsTest || robotRow.TestReason != models.UserTestReasonTestLab || robotRow.SignupPlatform != "android" {
		t.Fatalf("a guest from a Play test device should be tagged: %+v", robotRow)
	}
	if personRow.IsTest || personRow.SignupPlatform != "android" {
		t.Fatalf("a real guest should stay real: %+v", personRow)
	}

	// Signed-in requests record the client; a second one inside the interval
	// doesn't write again, a different client adds a row.
	for range 2 {
		if code := requestWithHeaders(t, router, http.MethodGet, "/v1/quick-prompts", person.Token, nil, appHeaders("Samsung SM-A546E", "15", false)).Code; code != http.StatusOK {
			t.Fatalf("authorized request = %d", code)
		}
	}
	requestWithHeaders(t, router, http.MethodGet, "/v1/quick-prompts", person.Token, nil, map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36",
	})
	var clients []models.UserClient
	database.DB.Where("user_id = ?", person.User.ID).Order("platform").Find(&clients)
	if len(clients) != 2 || clients[0].Platform != "android" || clients[0].AppBuild != "15" || clients[1].Device != "Chrome on Windows" || clients[1].FormFactor != "desktop" {
		t.Fatalf("expected the phone and the browser: %+v", clients)
	}

	// An admin's un-flag sticks even when the test device keeps calling.
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	performJSONRequest[map[string]any](t, router, http.MethodPatch, "/v1/admin/users/"+strconv.Itoa(int(robot.User.ID))+"/test", supportToken, map[string]any{"is_test": false}, http.StatusOK)
	resetClientWritesForTest()
	requestWithHeaders(t, router, http.MethodGet, "/v1/quick-prompts", robot.Token, nil, appHeaders("Google Pixel 6", "15", true))
	database.DB.First(&robotRow, robot.User.ID)
	if robotRow.IsTest {
		t.Fatal("automatic tagging must not overrule an admin's un-flag")
	}
}

func TestAdminAudienceCountsPeoplePerPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	now := time.Now().UTC()

	both := createRealUsersTestUser(t, "both", false, func(user *models.User) { user.SignupPlatform = "android" })
	phone := createRealUsersTestUser(t, "phone", true, func(user *models.User) { user.SignupPlatform = "android" })
	robot := createRealUsersTestUser(t, "robot", true, func(user *models.User) {
		user.IsTest, user.TestReason, user.SignupPlatform = true, models.UserTestReasonTestLab, "android"
	})
	client := func(user models.User, platform, device, form, version, build string) {
		t.Helper()
		row := models.UserClient{UserID: user.ID, ClientKey: generateUUID()[:32], Platform: platform, Device: device, FormFactor: form, AppVersion: version, AppBuild: build, Variant: "play", FirstSeenAt: now.Add(-time.Hour), LastSeenAt: now.Add(-time.Hour)}
		if platform == "web" {
			row.Variant = "web"
		}
		if err := database.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	client(both, "android", "Samsung SM-S918B", "phone", "1.0.0", "15")
	client(both, "web", "Chrome on Windows", "desktop", "", "")
	client(phone, "android", "Samsung SM-A546E", "phone", "1.0.0", "14")
	client(robot, "android", "Google Pixel 6", "phone", "1.0.0", "15")
	if err := database.DB.Create(&models.PushDevice{UserID: phone.ID, Token: "ExponentPushToken[x]", Platform: "android", Active: true}).Error; err != nil {
		t.Fatal(err)
	}

	type bucket struct {
		Key   string `json:"key"`
		Users int    `json:"users"`
	}
	type audience struct {
		RecordedUsers int      `json:"recorded_users"`
		Platform      []bucket `json:"platform"`
		FormFactor    []bucket `json:"form_factor"`
		AppVersion    []bucket `json:"app_version"`
		Signups       []bucket `json:"signups_by_platform"`
		PushReach     []bucket `json:"push_reach"`
	}
	find := func(rows []bucket, key string) int {
		for _, row := range rows {
			if row.Key == key {
				return row.Users
			}
		}
		return 0
	}
	real := performJSONRequest[audience](t, router, http.MethodGet, "/v1/admin/audience", viewerToken, nil, http.StatusOK)
	if real.RecordedUsers != 2 || find(real.Platform, "android") != 2 || find(real.Platform, "web") != 1 || find(real.FormFactor, "desktop") != 1 {
		t.Fatalf("real audience: %+v", real)
	}
	if find(real.AppVersion, "1.0.0 (15)") != 1 || find(real.AppVersion, "1.0.0 (14)") != 1 {
		t.Fatalf("app versions should name the build: %+v", real.AppVersion)
	}
	if find(real.Signups, "android") != 2 || find(real.PushReach, "android") != 1 {
		t.Fatalf("signups and push reach: %+v %+v", real.Signups, real.PushReach)
	}
	all := performJSONRequest[audience](t, router, http.MethodGet, "/v1/admin/audience?include_test=true", viewerToken, nil, http.StatusOK)
	if find(all.Platform, "android") != 3 {
		t.Fatalf("include_test should add the robot: %+v", all.Platform)
	}

	type list struct {
		Users []adminUserListItem `json:"users"`
		Total int64               `json:"total"`
	}
	web := performJSONRequest[list](t, router, http.MethodGet, "/v1/admin/users?platform=web", viewerToken, nil, http.StatusOK)
	if web.Total != 1 || web.Users[0].Username != "both" || web.Users[0].Platform == "" {
		t.Fatalf("platform filter and latest device: %+v", web.Users)
	}
}
