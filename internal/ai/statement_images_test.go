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

func TestParseStatementImagesUsesMultimodalContentAndStatementLimit(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"lines\":[]}"}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(&config.Config{
		OpenAIKey:                "test-key",
		OpenAIBaseURL:            server.URL,
		OpenAILlmModel:           "gpt-4o-mini",
		OpenAIStatementMaxTokens: 4096,
	})
	_, _, err := client.ParseStatementImages(context.Background(), []StatementImage{{
		MIME: "image/png", Data: []byte("png bytes"),
	}}, "2026-07-06", "2026-08-05")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestBody["max_completion_tokens"]; got != float64(4096) {
		t.Fatalf("max_completion_tokens = %v", got)
	}
	messages := requestBody["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	imagePart := content[1].(map[string]any)["image_url"].(map[string]any)
	if !strings.HasPrefix(imagePart["url"].(string), "data:image/png;base64,") {
		t.Fatalf("unexpected image URL %q", imagePart["url"])
	}
}
