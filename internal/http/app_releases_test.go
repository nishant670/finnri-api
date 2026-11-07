package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

type appReleaseListResponse struct {
	Releases                []AppReleaseView `json:"releases"`
	LatestVersionCode       int              `json:"latest_version_code"`
	MinSupportedVersionCode int              `json:"min_supported_version_code"`
}

func releaseRequest(t *testing.T, router *gin.Engine, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeRelease[T any](t *testing.T, response *httptest.ResponseRecorder, expectedStatus int) T {
	t.Helper()
	if response.Code != expectedStatus {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, expectedStatus, response.Body.String())
	}
	var decoded T
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode: %v; body = %s", err, response.Body.String())
	}
	return decoded
}

func sampleRelease(versionCode int) map[string]any {
	return map[string]any{
		"platform":     "android",
		"version_code": versionCode,
		"version_name": "1.1",
		"headline":     "Plan events,   add anyone to a split.",
		"items": []map[string]any{
			{"kind": "new", "title": "Events", "body": "See what a whole occasion cost.", "deep_link": "/events"},
			{"kind": "fix", "title": "Voice capture works again", "problem": "Tapping the mic did nothing.", "fix": "Finnri asks for the microphone again."},
			{"kind": "improved", "title": "Widget matches your app colours", "deep_link": "/ignored", "problem": "ignored"},
		},
	}
}

func TestAppReleaseIsHiddenUntilPublishedAndOrdersItsCards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)

	draft := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, sampleRelease(12)), http.StatusCreated)
	if draft.Status != models.AppReleaseStatusDraft || draft.Headline != "Plan events, add anyone to a split." {
		t.Fatalf("a new release should be a cleaned-up draft: %#v", draft)
	}
	if len(draft.Items) != 3 || draft.Items[0].Kind != "new" || draft.Items[1].Kind != "fix" {
		t.Fatalf("cards should keep the order they were sent in: %#v", draft.Items)
	}
	if improved := draft.Items[2]; improved.DeepLink != "" || improved.Problem != "" {
		t.Fatalf("fields that do not apply to an improvement should be cleared: %#v", improved)
	}

	hidden := decodeRelease[appReleaseListResponse](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=android&since=11", "", nil), http.StatusOK)
	if len(hidden.Releases) != 0 || hidden.LatestVersionCode != 0 {
		t.Fatalf("a draft must not reach the app: %#v", hidden)
	}

	// The editor saves the whole release; reordering is just a new order.
	reordered := sampleRelease(12)
	items := reordered["items"].([]map[string]any)
	reordered["items"] = []map[string]any{items[1], items[0]}
	updated := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/releases/"+strconv.Itoa(int(draft.ID)), support, reordered), http.StatusOK)
	if len(updated.Items) != 2 || updated.Items[0].Kind != "fix" || updated.Items[1].Kind != "new" {
		t.Fatalf("a save should replace the cards in their new order: %#v", updated.Items)
	}

	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases/"+strconv.Itoa(int(draft.ID))+"/publish", support, nil), http.StatusOK)

	listed := decodeRelease[appReleaseListResponse](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=android&since=11", "", nil), http.StatusOK)
	if len(listed.Releases) != 1 || listed.LatestVersionCode != 12 || listed.MinSupportedVersionCode != 0 {
		t.Fatalf("the published release should reach an older build: %#v", listed)
	}
	if got := listed.Releases[0].Items; len(got) != 2 || got[0].Kind != "fix" || got[1].DeepLink != "/events" {
		t.Fatalf("the app should get the cards in order: %#v", got)
	}

	upToDate := decodeRelease[appReleaseListResponse](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=android&since=12", "", nil), http.StatusOK)
	if len(upToDate.Releases) != 0 || upToDate.LatestVersionCode != 12 {
		t.Fatalf("a build that is current gets no releases but still learns the latest: %#v", upToDate)
	}
	ios := decodeRelease[appReleaseListResponse](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=ios", "", nil), http.StatusOK)
	if len(ios.Releases) != 0 {
		t.Fatalf("an Android release must not reach iOS: %#v", ios)
	}

	own := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases/12?platform=android", "", nil), http.StatusOK)
	if own.VersionName != "1.1" || len(own.Items) != 2 {
		t.Fatalf("a freshly updated build should read its own notes: %#v", own)
	}

	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases/"+strconv.Itoa(int(draft.ID))+"/unpublish", support, nil), http.StatusOK)
	if response := releaseRequest(t, router, http.MethodGet, "/v1/app/releases/12?platform=android", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("an unpublished release should be gone from the app, got %d", response.Code)
	}
}

func TestOnlyAnOwnerCanForceAnUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)
	_, owner := createAdminSessionForTest(t, models.AdminRoleOwner)

	forced := sampleRelease(12)
	forced["min_supported"] = true
	if response := releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, forced); response.Code != http.StatusForbidden {
		t.Fatalf("support should not be able to force an update, got %d %s", response.Code, response.Body.String())
	}
	release := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", owner, forced), http.StatusCreated)

	// Support may still edit the copy of a release an owner made critical.
	forced["headline"] = "A fix for voice capture."
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/releases/"+strconv.Itoa(int(release.ID)), support, forced), http.StatusOK)
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases/"+strconv.Itoa(int(release.ID))+"/publish", support, nil), http.StatusOK)

	// A newer release that is not critical, and a critical one still in draft.
	newer := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, sampleRelease(13)), http.StatusCreated)
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases/"+strconv.Itoa(int(newer.ID))+"/publish", support, nil), http.StatusOK)
	draftCritical := sampleRelease(14)
	draftCritical["min_supported"] = true
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", owner, draftCritical), http.StatusCreated)

	listed := decodeRelease[appReleaseListResponse](t, releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=android&since=10", "", nil), http.StatusOK)
	if listed.MinSupportedVersionCode != 12 || listed.LatestVersionCode != 13 {
		t.Fatalf("only a published critical release should set the minimum: %#v", listed)
	}
	if len(listed.Releases) != 2 || listed.Releases[0].VersionCode != 13 {
		t.Fatalf("releases should be newest first: %#v", listed.Releases)
	}
}

// Moving a critical release to another build moves the oldest build still
// allowed. Raised past the store's newest build, it would lock every phone
// out, so it is an owner's call like turning the flag on.
func TestOnlyAnOwnerCanMoveAForcedUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)
	_, owner := createAdminSessionForTest(t, models.AdminRoleOwner)

	forced := sampleRelease(12)
	forced["min_supported"] = true
	release := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", owner, forced), http.StatusCreated)
	target := "/v1/admin/releases/" + strconv.Itoa(int(release.ID))

	forced["version_code"] = 99
	if response := releaseRequest(t, router, http.MethodPut, target, support, forced); response.Code != http.StatusForbidden {
		t.Fatalf("support should not move a forced update, got %d %s", response.Code, response.Body.String())
	}
	forced["version_code"] = 12
	forced["platform"] = "ios"
	if response := releaseRequest(t, router, http.MethodPut, target, support, forced); response.Code != http.StatusForbidden {
		t.Fatalf("support should not move a forced update to another platform, got %d %s", response.Code, response.Body.String())
	}

	// Lowering the floor is safe, so support may still clear the flag.
	forced["platform"] = "android"
	forced["min_supported"] = false
	cleared := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPut, target, support, forced), http.StatusOK)
	if cleared.MinSupported {
		t.Fatalf("support should be able to clear the flag: %+v", cleared)
	}

	forced["min_supported"] = true
	forced["version_code"] = 13
	moved := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPut, target, owner, forced), http.StatusOK)
	if moved.VersionCode != 13 || !moved.MinSupported {
		t.Fatalf("an owner may move it: %+v", moved)
	}
}

func TestAppReleaseInputIsChecked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)
	_, viewer := createAdminSessionForTest(t, models.AdminRoleViewer)

	if response := releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", viewer, sampleRelease(12)); response.Code != http.StatusForbidden {
		t.Fatalf("a viewer should not write releases, got %d", response.Code)
	}

	bad := map[string]any{
		"platform":     "windows",
		"version_code": 0,
		"version_name": "v1",
		"headline":     " ",
		"items": []map[string]any{
			{"kind": "ui", "title": "Filters"},
			{"kind": "fix", "title": "Mic"},
			{"kind": "new", "title": "Events", "body": "Occasions.", "deep_link": "https://example.com/phish"},
			{"kind": "poster", "title": "Nope"},
		},
	}
	response := releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, bad)
	failure := decodeRelease[struct {
		Fields map[string]string `json:"fields"`
	}](t, response, http.StatusUnprocessableEntity)
	for _, field := range []string{
		"platform", "version_code", "version_name", "headline",
		"items[0].body", "items[1].problem", "items[1].fix",
		"items[2].deep_link", "items[3].kind",
	} {
		if failure.Fields[field] == "" {
			t.Errorf("expected a message for %s, got %#v", field, failure.Fields)
		}
	}

	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, sampleRelease(12)), http.StatusCreated)
	if response := releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, sampleRelease(12)); response.Code != http.StatusConflict {
		t.Fatalf("a second release for the same build should conflict, got %d", response.Code)
	}

	empty := sampleRelease(20)
	empty["items"] = []map[string]any{}
	release := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, empty), http.StatusCreated)
	if response := releaseRequest(t, router, http.MethodPost, "/v1/admin/releases/"+strconv.Itoa(int(release.ID))+"/publish", support, nil); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a release with no cards should not publish, got %d", response.Code)
	}

	if response := releaseRequest(t, router, http.MethodGet, "/v1/app/releases?platform=", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("the app must say which platform it is, got %d", response.Code)
	}
}

func TestANewLookNeedsNoScreenshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)

	// The build that ships a new look draws it live; the API only carries the words.
	look := sampleRelease(12)
	look["items"] = []map[string]any{{"kind": "ui", "title": "Filters", "body": "One tidy sheet.", "before_image": "ignored.png"}}
	release := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, look), http.StatusCreated)
	if len(release.Items) != 1 || release.Items[0].Kind != "ui" || release.Items[0].Body != "One tidy sheet." {
		t.Fatalf("a new-look card should save as text: %#v", release.Items)
	}
	if strings.Contains(releaseRequest(t, router, http.MethodGet, "/v1/admin/releases", support, nil).Body.String(), "image") {
		t.Fatal("releases should carry no image fields")
	}
	if response := releaseRequest(t, router, http.MethodGet, "/v1/app/release-media/"+strings.Repeat("a", 32)+".png", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("the screenshot route should be gone, got %d", response.Code)
	}
}

func TestPublishedReleasesCannotBeDeleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)

	release := decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/releases", support, sampleRelease(12)), http.StatusCreated)
	path := "/v1/admin/releases/" + strconv.Itoa(int(release.ID))
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, path+"/publish", support, nil), http.StatusOK)
	if response := releaseRequest(t, router, http.MethodDelete, path, support, nil); response.Code != http.StatusConflict {
		t.Fatalf("a published release must be unpublished before deleting, got %d", response.Code)
	}
	decodeRelease[AppReleaseView](t, releaseRequest(t, router, http.MethodPost, path+"/unpublish", support, nil), http.StatusOK)
	if response := releaseRequest(t, router, http.MethodDelete, path, support, nil); response.Code != http.StatusNoContent {
		t.Fatalf("a draft should delete, got %d %s", response.Code, response.Body.String())
	}
	var items int64
	database.DB.Model(&models.AppReleaseItem{}).Where("release_id = ?", release.ID).Count(&items)
	if items != 0 {
		t.Fatalf("deleting a release should remove its cards, %d left", items)
	}
}
