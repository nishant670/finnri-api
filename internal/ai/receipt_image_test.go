package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"finnri/internal/config"
)

func receiptVisionServer(t *testing.T, answer func(model string) (int, string)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		status, reply := answer(body["model"].(string))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

const receiptVisionReply = `{"choices":[{"message":{"content":"{\"amount\":3301}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func TestParseReceiptImageUsesTheVisionModelWithReasoningEffort(t *testing.T) {
	server, bodies := receiptVisionServer(t, func(string) (int, string) { return 200, receiptVisionReply })
	client := NewOpenAIClient(&config.Config{
		OpenAIKey: "test-key", OpenAIBaseURL: server.URL,
		OpenAILlmModel: "gpt-4o-mini", OpenAIVisionModel: "gpt-5.4-mini", OpenAIVisionReasoningEffort: "low",
	})

	_, usage, err := client.ParseReceiptImage(context.Background(), StatementImage{MIME: "image/jpeg", Data: []byte("jpeg")}, "2026-10-11")
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("expected one request, got %d", len(*bodies))
	}
	body := (*bodies)[0]
	if body["model"] != "gpt-5.4-mini" || body["reasoning_effort"] != "low" {
		t.Fatalf("model = %v, reasoning_effort = %v", body["model"], body["reasoning_effort"])
	}
	// Reasoning tokens count against the budget, so the cap grows past the
	// draft's own 1200.
	if got := body["max_completion_tokens"].(float64); got <= receiptMaxTokens {
		t.Fatalf("max_completion_tokens = %v, want headroom for reasoning", got)
	}
	if usage.Model != "gpt-5.4-mini" {
		t.Fatalf("usage should name the model that answered, got %q", usage.Model)
	}
	prompt := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(prompt, "find the main document") || !strings.Contains(prompt, "Today is 2026-10-11.") {
		t.Fatalf("prompt is missing the main-document rule or the date")
	}
}

func TestParseReceiptImageFallsBackWhenTheVisionModelIsUnavailable(t *testing.T) {
	server, bodies := receiptVisionServer(t, func(model string) (int, string) {
		if model == "gpt-5.4-mini" {
			return 404, `{"error":{"message":"The model gpt-5.4-mini does not exist or you do not have access to it.","code":"model_not_found"}}`
		}
		return 200, receiptVisionReply
	})
	client := NewOpenAIClient(&config.Config{
		OpenAIKey: "test-key", OpenAIBaseURL: server.URL,
		OpenAILlmModel: "gpt-4o-mini", OpenAIVisionModel: "gpt-5.4-mini", OpenAIVisionReasoningEffort: "low",
	})

	raw, usage, err := client.ParseReceiptImage(context.Background(), StatementImage{MIME: "image/jpeg", Data: []byte("jpeg")}, "2026-10-11")
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 || (*bodies)[1]["model"] != "gpt-4o-mini" {
		t.Fatalf("expected a retry on the text model, got %d requests", len(*bodies))
	}
	if _, sent := (*bodies)[1]["reasoning_effort"]; sent {
		t.Fatalf("reasoning_effort must not be sent to a non-reasoning model")
	}
	if string(raw) != `{"amount":3301}` || usage.Model != "gpt-4o-mini" {
		t.Fatalf("raw = %s, model = %q", raw, usage.Model)
	}
}

func TestParseReceiptImageDoesNotRetryOtherFailures(t *testing.T) {
	server, bodies := receiptVisionServer(t, func(string) (int, string) {
		return 500, `{"error":{"message":"upstream overloaded"}}`
	})
	client := NewOpenAIClient(&config.Config{
		OpenAIKey: "test-key", OpenAIBaseURL: server.URL,
		OpenAILlmModel: "gpt-4o-mini", OpenAIVisionModel: "gpt-5.4-mini",
	})

	if _, _, err := client.ParseReceiptImage(context.Background(), StatementImage{MIME: "image/jpeg", Data: []byte("jpeg")}, "2026-10-11"); err == nil {
		t.Fatal("expected the provider error")
	}
	if len(*bodies) != 1 {
		t.Fatalf("a server error is not a model problem; got %d requests", len(*bodies))
	}
}
