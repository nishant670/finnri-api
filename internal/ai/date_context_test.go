package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"finnri/internal/config"
)

// On Monday 23 Nov 2026, "the last Saturday" came back as Thursday 19 Nov: the
// model was given only "Today is 2026-11-23" and worked the weekday out wrong.
// It now gets the days around today by name, so it only has to look one up.
func TestDateContextNamesTheWeekAroundToday(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	got := dateContext(time.Date(2026, 11, 23, 13, 14, 0, 0, ist))

	for _, want := range []string{
		"Today is Monday, 2026-11-23.",
		"Sat 2026-11-21",               // "last Saturday"
		"Past 7 days: Mon 2026-11-16,", // "last Monday", said on a Monday
		"Sun 2026-11-22.",              // past list ends yesterday
		"Next 7 days: Tue 2026-11-24,", // future list starts tomorrow
		"Fri 2026-11-27",               // "by Friday"
		"Mon 2026-11-30.",              // a week ahead
	} {
		if !strings.Contains(got, want) {
			t.Errorf("date context lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Thu 2026-11-21") || strings.Contains(got, "Sat 2026-11-19") {
		t.Fatalf("weekday names are off:\n%s", got)
	}
}

// Month and year ends must name the right days too.
func TestDateContextCrossesAYearEnd(t *testing.T) {
	got := dateContext(time.Date(2027, 1, 2, 10, 0, 0, 0, time.UTC))
	for _, want := range []string{"Today is Saturday, 2027-01-02.", "Sat 2026-12-26", "Fri 2027-01-01.", "Sun 2027-01-03"} {
		if !strings.Contains(got, want) {
			t.Errorf("date context lacks %q:\n%s", want, got)
		}
	}
}

// The parse request itself carries the calendar, in the user's timezone: at
// 01:00 IST on Monday it is still Sunday in UTC.
func TestParseSendsTheWeekdayCalendar(t *testing.T) {
	var userMessage string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				userMessage = m.Content
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer server.Close()

	ist, _ := time.LoadLocation("Asia/Kolkata")
	clockNow = func() time.Time { return time.Date(2026, 11, 23, 1, 0, 0, 0, ist).UTC() }
	defer func() { clockNow = time.Now }()

	client := NewOpenAIClient(&config.Config{OpenAIKey: "test", OpenAIBaseURL: server.URL, OpenAILlmModel: "m", OpenAIMaxTokens: 10})
	if _, _, err := client.ParseText(context.Background(), "2000 cash to Riya last Saturday", "Asia/Kolkata", KnownNames{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(userMessage, "Today is Monday, 2026-11-23.") || !strings.Contains(userMessage, "Sat 2026-11-21") {
		t.Fatalf("parse request lacks the weekday calendar:\n%s", userMessage)
	}
}

func TestPromptSaysToCopyWeekdaysFromTheList(t *testing.T) {
	if !strings.Contains(promptText, "A weekday never needs working out") {
		t.Fatal("the prompt must tell the model to copy weekday dates from the listed days")
	}
}
