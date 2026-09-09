package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"finnri/internal/config"
)

func TestParseTextUsesConfiguredCostControls(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"{\"stage\":\"draft\"}"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}
		}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(&config.Config{
		OpenAIKey:       "test-key",
		OpenAIBaseURL:   server.URL,
		OpenAILlmModel:  "gpt-4o-mini",
		OpenAIMaxTokens: 600,
	})

	if _, _, err := client.ParseText(context.Background(), "coffee 200 via upi", "Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}
	if got := requestBody["model"]; got != "gpt-4o-mini" {
		t.Fatalf("model = %v", got)
	}
	if got := requestBody["max_completion_tokens"]; got != float64(600) {
		t.Fatalf("max_completion_tokens = %v", got)
	}
}

func TestTranscribeUsesConfiguredModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("model"); got != "gpt-4o-mini-transcribe" {
			t.Fatalf("model = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"coffee 200 via upi"}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(&config.Config{
		OpenAIKey:     "test-key",
		OpenAIBaseURL: server.URL,
		OpenAIWhisper: "gpt-4o-mini-transcribe",
	})

	got, err := client.Transcribe(context.Background(), "recording.m4a", []byte("audio"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "coffee 200 via upi" {
		t.Fatalf("transcript = %q", got)
	}
}

// The counts used to stop at a log line, which left every usage row with null
// tokens and pushed the cost model onto its flat per-credit fallback. The
// client has to hand them back to its caller for any of that to be recorded.
func TestParseTextReturnsProviderTokenUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"{\"stage\":\"draft\"}"}}],
			"usage":{"prompt_tokens":4387,"completion_tokens":132,"total_tokens":4519}
		}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(&config.Config{
		OpenAIKey:      "test-key",
		OpenAIBaseURL:  server.URL,
		OpenAILlmModel: "gpt-4o-mini",
	})

	content, usage, err := client.ParseText(context.Background(), "coffee 200 via upi", "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("expected the message content alongside the usage")
	}
	if usage.PromptTokens != 4387 || usage.CompletionTokens != 132 || usage.TotalTokens != 4519 {
		t.Fatalf("usage = %#v", usage)
	}
	if !usage.Reported() {
		t.Fatal("a response carrying counts should report as measured")
	}
}

// A provider that says nothing about tokens must not be recorded as having
// used none: downstream stores nil for "unknown" and zero for "measured none",
// and conflating them prices a real call at nothing.
func TestParseTextUsageIsUnreportedWhenProviderOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(&config.Config{
		OpenAIKey:      "test-key",
		OpenAIBaseURL:  server.URL,
		OpenAILlmModel: "gpt-4o-mini",
	})

	_, usage, err := client.ParseText(context.Background(), "coffee", "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Reported() {
		t.Fatalf("expected no reported usage, got %#v", usage)
	}
}
