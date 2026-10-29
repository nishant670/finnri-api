package ai

import (
	"context"
	"testing"

	"finnri/internal/config"
)

// Statement screenshots are read by the vision model, as receipts are: both
// the Add statement read and the check of a saved bill. gpt-4o-mini bills a
// phone screenshot at ~10x the input tokens for the same rows.
func TestStatementScreenshotsUseTheVisionModel(t *testing.T) {
	reply := `{"choices":[{"message":{"content":"{\"lines\":[]}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	server, bodies := receiptVisionServer(t, func(string) (int, string) { return 200, reply })
	client := NewOpenAIClient(&config.Config{
		OpenAIKey: "test-key", OpenAIBaseURL: server.URL,
		OpenAILlmModel: "gpt-4o-mini", OpenAIVisionModel: "gpt-4.1-mini",
	})
	images := []StatementImage{{MIME: "image/png", Data: []byte("png")}}

	_, readUsage, err := client.ReadStatementImages(context.Background(), images, "2026-10-29")
	if err != nil {
		t.Fatal(err)
	}
	_, checkUsage, err := client.ParseStatementImages(context.Background(), images, "2026-09-26", "2026-10-25")
	if err != nil {
		t.Fatal(err)
	}
	for index, body := range *bodies {
		if body["model"] != "gpt-4.1-mini" {
			t.Fatalf("request %d went to %v, want the vision model", index, body["model"])
		}
	}
	if readUsage.Model != "gpt-4.1-mini" || checkUsage.Model != "gpt-4.1-mini" {
		t.Fatalf("usage should name the vision model: %q, %q", readUsage.Model, checkUsage.Model)
	}
}

func TestStatementScreenshotsFallBackWhenTheVisionModelIsUnavailable(t *testing.T) {
	reply := `{"choices":[{"message":{"content":"{\"lines\":[]}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	server, bodies := receiptVisionServer(t, func(model string) (int, string) {
		if model == "gpt-4.1-mini" {
			return 404, `{"error":{"message":"The model gpt-4.1-mini does not exist or you do not have access to it.","code":"model_not_found"}}`
		}
		return 200, reply
	})
	client := NewOpenAIClient(&config.Config{
		OpenAIKey: "test-key", OpenAIBaseURL: server.URL,
		OpenAILlmModel: "gpt-4o-mini", OpenAIVisionModel: "gpt-4.1-mini",
	})

	_, usage, err := client.ReadStatementImages(context.Background(), []StatementImage{{MIME: "image/png", Data: []byte("png")}}, "2026-10-29")
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 || (*bodies)[1]["model"] != "gpt-4o-mini" || usage.Model != "gpt-4o-mini" {
		t.Fatalf("expected one retry on the text model, got %d requests, model %q", len(*bodies), usage.Model)
	}
}
