package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestMatchEventByNameToleratesSpokenVariants(t *testing.T) {
	events := []models.Event{
		{ID: 1, Name: "Kanatal trip"},
		{ID: 2, Name: "Diwali 2026"},
		{ID: 3, Name: "Goa trip"},
	}
	cases := map[string]uint{
		"Kanatal trip":       1,
		"kanatal Trip":       1,
		"the Kanatal trip":   1,
		"Kanatal":            1,
		"event Kanatal trip": 1,
		"Kanatl trip":        1,
		"diwali":             2,
		"trip":               0, // two trips: refuse to guess
		"Shimla":             0,
	}
	for spoken, want := range cases {
		got := matchEventByName(events, spoken)
		if want == 0 {
			if got != nil {
				t.Errorf("%q matched %q, want no match", spoken, got.Name)
			}
			continue
		}
		if got == nil || got.ID != want {
			t.Errorf("%q matched %v, want event %d", spoken, got, want)
		}
	}
}

func TestDropUnstatedSubscriptionRemovesAOneOffTag(t *testing.T) {
	entry := map[string]any{"tag": "Subscription", "tags": []any{"Subscription"}, "subscription_candidate": nil}
	dropUnstatedSubscription(entry,
		"5000 paid by UPI to Chaukhat Hospitality for staying and added to event for Kanatal trip and also split in the group with Bubu Dudu group",
		ai.KnownNames{Groups: []string{"Bubu Dudu Daily"}})
	if entry["tag"] != nil {
		t.Fatalf("tag = %v, want nil", entry["tag"])
	}
	if tags := entry["tags"].([]any); len(tags) != 0 {
		t.Fatalf("tags = %v, want none", tags)
	}
}

func TestDropUnstatedSubscriptionIgnoresDailyInAGroupName(t *testing.T) {
	entry := map[string]any{
		"tag": "Subscription", "tags": []any{"Subscription"},
		"subscription_candidate": map[string]any{"billing_interval": "daily"},
		"recurring_candidate":    true,
	}
	dropUnstatedSubscription(entry,
		"140 rupees paid for parking, split it with Bubu, Dudu, daily group",
		ai.KnownNames{Groups: []string{"Bubu Dudu Daily"}})
	if entry["subscription_candidate"] != nil || entry["tag"] != nil {
		t.Fatalf("subscription survived a group name: %v", entry)
	}
}

func TestDropUnstatedSubscriptionKeepsARealOne(t *testing.T) {
	for _, transcript := range []string{
		"Netflix 649 monthly",
		"paid 199 for Spotify subscription",
		"gym 1500 every month by UPI",
		"SIP 500 auto debit",
		"doodh 60 har roz",
	} {
		entry := map[string]any{"tag": "Subscription", "tags": []any{"Subscription"}, "subscription_candidate": map[string]any{}}
		dropUnstatedSubscription(entry, transcript, ai.KnownNames{})
		if entry["tag"] != "Subscription" || entry["subscription_candidate"] == nil {
			t.Errorf("%q lost its subscription: %v", transcript, entry)
		}
	}
}

func TestDropUnaskedAccountQuestionForUPI(t *testing.T) {
	entry := map[string]any{
		"mode": "UPI", "account_hint": nil,
		"clarifications": []any{"Which bank account should be used?", "What date was this?"},
	}
	needs := map[string]any{"account_hint": true, "date": true}
	dropUnaskedAccountQuestion(entry, needs)
	if _, ok := needs["account_hint"]; ok {
		t.Fatal("account_hint still flagged on a UPI payment")
	}
	if got := entry["clarifications"].([]any); len(got) != 1 || got[0] != "What date was this?" {
		t.Fatalf("clarifications = %v", got)
	}
}

func TestDropUnaskedAccountQuestionKeepsABankAccountQuestion(t *testing.T) {
	entry := map[string]any{"mode": "Bank Account", "clarifications": []any{"Which bank account should be used?"}}
	needs := map[string]any{"account_hint": true}
	dropUnaskedAccountQuestion(entry, needs)
	if needs["account_hint"] != true || len(entry["clarifications"].([]any)) != 1 {
		t.Fatalf("a stated bank account lost its question: %v %v", needs, entry)
	}
}

type namesRecordingParser struct {
	result []byte
	known  *ai.KnownNames
}

func (p namesRecordingParser) Transcribe(context.Context, string, []byte, string) (string, error) {
	return "", nil
}

func (p namesRecordingParser) ParseText(_ context.Context, _ string, _ string, known ai.KnownNames) ([]byte, ai.Usage, error) {
	*p.known = known
	return p.result, ai.Usage{}, nil
}

// The 140-rupee parking from 2026-11-11, end to end: the model is shown the
// user's names, the event resolves to its id, the subscription the sentence
// never asked for is gone, and UPI is not flagged.
func TestParseHandlerFillsEventFromSavedNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	var seen ai.KnownNames
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: namesRecordingParser{known: &seen, result: []byte(`{
			"title":"Parking","amount":140,"type":"expense","currency":"INR",
			"mode":"UPI","account_hint":null,"category":"Transport","date":"2026-11-11",
			"tag":"Subscription","tags":["Subscription"],
			"event_name":"Kanatal",
			"split_candidate":true,
			"split_candidate_details":{"group_name":"Bubu Dudu Daily","participants":[],"missing_fields":[]},
			"needs_confirmation":{"account_hint":true},
			"clarifications":["Which bank account should be used?"]
		}`)},
	}

	form := url.Values{"hint_text": {"140 rupees paid for parking for eco park in Dhanolti, paid by UPI today. Add it to event Kanatal trip and split it with Bubu, Dudu, daily group."}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(response)
	ginContext.Request = request
	user := attachParseCreditUser(t, ginContext)

	event := models.Event{UserID: user.ID, Name: "Kanatal trip"}
	other := models.Event{UserID: user.ID + 1000, Name: "Kanatal"} // someone else's
	group := models.SplitGroup{UserID: user.ID, Name: "Bubu Dudu Daily"}
	friend := models.SplitFriend{UserID: user.ID, Name: "Biwi"}
	for _, row := range []any{&event, &other, &group, &friend} {
		if err := database.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	server.handleParse(ginContext)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(seen.Events) != 1 || seen.Events[0] != "Kanatal trip" ||
		len(seen.Groups) != 1 || seen.Groups[0] != "Bubu Dudu Daily" ||
		len(seen.Friends) != 1 || seen.Friends[0] != "Biwi" {
		t.Fatalf("model was shown %+v", seen)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["event_id"] != float64(event.ID) || body["event_name"] != "Kanatal trip" {
		t.Fatalf("event = %v / %v, want %d", body["event_id"], body["event_name"], event.ID)
	}
	if body["tag"] == "Subscription" {
		t.Fatalf("a one-off parking was tagged Subscription: %s", response.Body.String())
	}
	if needs, _ := body["needs_confirmation"].(map[string]any); needs["account_hint"] == true {
		t.Fatalf("UPI was flagged for an account check: %s", response.Body.String())
	}
}
