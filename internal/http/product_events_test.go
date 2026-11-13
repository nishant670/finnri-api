package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type productEventInput struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	At    string `json:"at,omitempty"`
}

func postProductEvents(t *testing.T, router *gin.Engine, token string, events []productEventInput, status int) map[string]any {
	t.Helper()
	return performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/analytics/events", token,
		map[string]any{"platform": "android", "app_version": "1.0.0", "events": events}, status)
}

func TestProductEventsKeepOnlyWhatIsOnTheList(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	now := time.Now().UTC()

	result := postProductEvents(t, router, token, []productEventInput{
		{Name: "entry_saved", Label: "voice", At: now.Add(-time.Minute).Format(time.RFC3339)},
		{Name: "app_opened"},
		// Not on the list.
		{Name: "typed_a_secret"},
		// A label that is not a single lowercase word: something a person
		// typed, an amount, a merchant. Dropped whole.
		{Name: "entry_saved", Label: "Dinner at Bukhara 4500"},
		{Name: "entry_saved", Label: "4,500.00"},
		// Too old to be believed.
		{Name: "app_opened", At: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)},
	}, http.StatusOK)
	if result["accepted"] != float64(2) || result["dropped"] != float64(4) {
		t.Fatalf("accepted/dropped = %v/%v, want 2/4", result["accepted"], result["dropped"])
	}

	var stored []models.ProductEvent
	if err := database.DB.Where("user_id = ?", user.ID).Order("id").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Name != "entry_saved" || stored[0].Label != "voice" || stored[1].Name != "app_opened" {
		t.Fatalf("unexpected stored events: %#v", stored)
	}
	if stored[0].Platform != "android" || stored[0].AppVersion != "1.0.0" {
		t.Fatalf("platform and version were not kept: %#v", stored[0])
	}
}

func TestProductEventsClampTheFutureAndCapTheBatch(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)

	postProductEvents(t, router, token, []productEventInput{
		{Name: "app_opened", At: time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)},
	}, http.StatusOK)
	var event models.ProductEvent
	if err := database.DB.Where("user_id = ?", user.ID).First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.OccurredAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("an event from the future was stored as %v", event.OccurredAt)
	}

	tooMany := make([]productEventInput, maxProductEventsPerBatch+1)
	for index := range tooMany {
		tooMany[index] = productEventInput{Name: "app_opened"}
	}
	postProductEvents(t, router, token, tooMany, http.StatusUnprocessableEntity)

	// Signed-in only.
	postProductEvents(t, router, "", []productEventInput{{Name: "app_opened"}}, http.StatusUnauthorized)
}

func TestProductFunnelCountsPeopleAndLabelsInOrder(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)
	_, first := createBillingTestUserSession(t)
	_, second := createBillingTestUserSession(t)

	postProductEvents(t, router, first, []productEventInput{
		{Name: "app_opened"}, {Name: "app_opened"},
		{Name: "entry_saved", Label: "voice"}, {Name: "entry_saved", Label: "voice"}, {Name: "entry_saved", Label: "manual"},
	}, http.StatusOK)
	postProductEvents(t, router, second, []productEventInput{
		{Name: "app_opened"}, {Name: "entry_saved", Label: "manual"},
	}, http.StatusOK)

	funnel := performJSONRequest[struct {
		People int64 `json:"people"`
		Steps  []struct {
			Name   string `json:"name"`
			Users  int64  `json:"users"`
			Events int64  `json:"events"`
			Labels []struct {
				Label  string `json:"label"`
				Users  int64  `json:"users"`
				Events int64  `json:"events"`
			} `json:"labels"`
		} `json:"steps"`
	}](t, router, http.MethodGet, "/v1/admin/analytics/product-funnel", ownerToken, nil, http.StatusOK)

	if funnel.People != 2 || len(funnel.Steps) != len(productEventNames) {
		t.Fatalf("people=%d steps=%d, want 2 and %d", funnel.People, len(funnel.Steps), len(productEventNames))
	}
	for index, step := range funnel.Steps {
		if step.Name != productEventNames[index] {
			t.Fatalf("step %d is %q, want funnel order %q", index, step.Name, productEventNames[index])
		}
		switch step.Name {
		case "app_opened":
			if step.Users != 2 || step.Events != 3 {
				t.Fatalf("app_opened users/events = %d/%d, want 2/3", step.Users, step.Events)
			}
		case "entry_saved":
			if step.Users != 2 || step.Events != 4 || len(step.Labels) != 2 {
				t.Fatalf("entry_saved = %+v", step)
			}
			for _, label := range step.Labels {
				if label.Label == "voice" && (label.Users != 1 || label.Events != 2) {
					t.Fatalf("voice label = %+v, want 1 user and 2 events", label)
				}
				if label.Label == "manual" && (label.Users != 2 || label.Events != 2) {
					t.Fatalf("manual label = %+v, want 2 users and 2 events", label)
				}
			}
		case "paywall_shown":
			if step.Users != 0 || step.Events != 0 {
				t.Fatalf("an event nobody recorded should read zero, got %+v", step)
			}
		}
	}
}

func TestOldProductEventsArePurged(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-productEventRetention - time.Hour), now.Add(-time.Hour)} {
		if err := database.DB.Create(&models.ProductEvent{UserID: user.ID, Name: "app_opened", OccurredAt: at}).Error; err != nil {
			t.Fatal(err)
		}
	}
	removed, err := purgeOldProductEvents(now)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want 1", removed, err)
	}
}
