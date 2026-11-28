package ai

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"finnri/internal/config"
)

//go:embed prompt.txt
var promptText string

// promptBatchText is appended to the system prompt when the app asks for a
// batch (`multi=1`, P103): several payments in one text come back as
// {"stage":"batch","items":[...]}. Old apps never send multi=1, so their prompt
// and cost are exactly as before.
//
//go:embed prompt_batch.txt
var promptBatchText string

// MaxBatchItems is how many transactions one prompt may return (P116, the
// user's call on 2026-11-20). The app holds the same number in
// lib/voice-batch.ts (MAX_ITEMS_PER_PROMPT).
const MaxBatchItems = 10

// BatchParser is implemented by parsers that can read several transactions
// from one text. The handler falls back to ParseText for one that can't.
type BatchParser interface {
	ParseTextBatch(ctx context.Context, transcript, tz string, known KnownNames) ([]byte, Usage, error)
}

type OpenAIClient struct {
	cfg  *config.Config
	http *http.Client
	// batchHTTP carries multi=1 reads, which may run past the usual timeout.
	batchHTTP *http.Client
}

type Parser interface {
	// Transcribe turns audio into text. hint primes the transcriber with names
	// it would otherwise misspell; empty means none.
	Transcribe(ctx context.Context, filename string, audio []byte, hint string) (string, error)
	ParseText(ctx context.Context, transcript, tz string, known KnownNames) ([]byte, Usage, error)
}

// clockNow is time.Now, swappable so tests can stand on a fixed day.
var clockNow = time.Now

// dateContext tells the model today's date and weekday, and spells out the
// week either side of it with weekday names.
//
// With only "Today is 2026-11-23" the model has to work out the weekday itself,
// and gpt-4o-mini gets it wrong: on Monday 23 Nov 2026 it read "last Saturday"
// as 19 Nov and "on Friday" as 18 Nov, four runs out of four. Given the days by
// name, it only has to look one up.
func dateContext(today time.Time) string {
	day := func(offset int) string {
		d := today.AddDate(0, 0, offset)
		return d.Format("Mon 2006-01-02")
	}
	past := make([]string, 0, 7)
	for offset := -7; offset <= -1; offset++ {
		past = append(past, day(offset))
	}
	coming := make([]string, 0, 7)
	for offset := 1; offset <= 7; offset++ {
		coming = append(coming, day(offset))
	}
	return fmt.Sprintf(
		"Today is %s, %s. Past 7 days: %s. Next 7 days: %s.",
		today.Format("Monday"), today.Format("2006-01-02"),
		strings.Join(past, ", "), strings.Join(coming, ", "),
	)
}

func NewOpenAIClient(cfg *config.Config) *OpenAIClient {
	client := &http.Client{}
	if cfg.ReqTimeoutSec > 0 {
		client.Timeout = time.Duration(cfg.ReqTimeoutSec) * time.Second
	}
	batchClient := &http.Client{Timeout: client.Timeout}
	if cfg.ReqTimeoutSecBatch > 0 {
		batchClient.Timeout = time.Duration(cfg.ReqTimeoutSecBatch) * time.Second
	}
	return &OpenAIClient{cfg: cfg, http: client, batchHTTP: batchClient}
}

func (c *OpenAIClient) Transcribe(ctx context.Context, filename string, audio []byte, hint string) (string, error) {
	if c.cfg.OpenAIKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY missing")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	if _, err := io.Copy(fw, bytes.NewReader(audio)); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", c.cfg.OpenAIWhisper)
	if hint = strings.TrimSpace(hint); hint != "" {
		_ = mw.WriteField("prompt", hint)
	}
	_ = mw.Close()

	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.OpenAIBaseURL+"/audio/transcriptions", &buf)
	req.Header.Set("Authorization", "Bearer "+c.cfg.OpenAIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("whisper error: %s", string(b))
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Text), nil
}

func (c *OpenAIClient) ParseText(ctx context.Context, transcript, tz string, known KnownNames) ([]byte, Usage, error) {
	return c.parseWithPrompt(ctx, c.http, promptText, c.cfg.OpenAIMaxTokens, transcript, tz, known)
}

// ParseTextBatch reads a text that may hold up to MaxBatchItems transactions.
// Ten full drafts need far more room than one, so it has its own token budget.
func (c *OpenAIClient) ParseTextBatch(ctx context.Context, transcript, tz string, known KnownNames) ([]byte, Usage, error) {
	maxTokens := c.cfg.OpenAIMaxTokensBatch
	if maxTokens < c.cfg.OpenAIMaxTokens {
		maxTokens = c.cfg.OpenAIMaxTokens
	}
	httpClient := c.batchHTTP
	if httpClient == nil {
		httpClient = c.http
	}
	return c.parseWithPrompt(ctx, httpClient, promptText+promptBatchText, maxTokens, transcript, tz, known)
}

func (c *OpenAIClient) parseWithPrompt(ctx context.Context, httpClient *http.Client, systemPrompt string, maxTokens int, transcript, tz string, known KnownNames) ([]byte, Usage, error) {
	if c.cfg.OpenAIKey == "" {
		return nil, Usage{}, fmt.Errorf("OPENAI_API_KEY missing")
	}

	// Calculate current date in User's TZ
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, _ = time.LoadLocation("Asia/Kolkata")
	}
	if loc == nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	dates := dateContext(clockNow().In(loc))

	body := map[string]any{
		"model":                 c.cfg.OpenAILlmModel,
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": fmt.Sprintf("Context: Timezone is %s. %s%s\nText: %s", tz, dates, known.contextLines(), transcript)},
		},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.cfg.OpenAIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		bs, _ := io.ReadAll(resp.Body)
		return nil, Usage{}, fmt.Errorf("llm error: %s", string(bs))
	}

	var out chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	log.Printf(
		"openai parse usage: model=%s prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		c.cfg.OpenAILlmModel,
		out.Usage.PromptTokens,
		out.Usage.CompletionTokens,
		out.Usage.TotalTokens,
	)
	if len(out.Choices) == 0 {
		return nil, Usage{}, fmt.Errorf("no choices")
	}
	return []byte(out.Choices[0].Message.Content), out.usage(), nil
}
