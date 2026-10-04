package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// StatementImage is request-scoped input for statement extraction. Callers
// discard Data as soon as ParseStatementImages returns; the adapter never
// writes it to disk or logs it.
type StatementImage struct {
	MIME string
	Data []byte
}

// StatementImageParser is deliberately separate from Parser. Tests and local
// parser implementations that only handle transaction text do not need fake
// vision support just because the statement producer is optional.
type StatementImageParser interface {
	ParseStatementImages(ctx context.Context, images []StatementImage, cycleStart, cycleEnd string) ([]byte, Usage, error)
}

const statementImagePrompt = `Read these ordered credit-card statement screenshots and return JSON only.
The response must be {"lines":[{"date":"YYYY-MM-DD","description":"merchant or bank description","amount":123.45,"type":"expense|income"}]}.
Use expense for debits/purchases/fees/interest/EMIs and income for refunds/credits/payments received.
Amounts must be positive. Include transaction rows only. Ignore totals, limits, addresses, card numbers, rewards, headers and footers.
The statement cycle is %s through %s inclusive. Resolve dates without years into that cycle.
Screenshots can overlap: emit an identical row only once. Preserve two genuinely separate transactions even if their amounts match.`

// ParseStatementImages sends image data as data URLs in a multimodal user
// message. Chat Completions is retained here because it is already the
// configured provider path used by ParseText.
func (c *OpenAIClient) ParseStatementImages(ctx context.Context, images []StatementImage, cycleStart, cycleEnd string) ([]byte, Usage, error) {
	if len(images) == 0 {
		return nil, Usage{}, fmt.Errorf("no statement images")
	}
	maxTokens := c.cfg.OpenAIStatementMaxTokens
	if maxTokens <= 0 {
		maxTokens = 4000
	}
	return c.completeWithImages(ctx, "statement", fmt.Sprintf(statementImagePrompt, cycleStart, cycleEnd), images, maxTokens)
}

// completeWithImages runs one JSON-mode chat completion over a text prompt and
// a set of images. The image bytes are only ever base64-encoded into the
// request body; they are never logged or written anywhere.
func (c *OpenAIClient) completeWithImages(ctx context.Context, label, prompt string, images []StatementImage, maxTokens int) ([]byte, Usage, error) {
	if c.cfg.OpenAIKey == "" {
		return nil, Usage{}, fmt.Errorf("OPENAI_API_KEY missing")
	}

	content := make([]map[string]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "text", "text": prompt})
	for _, image := range images {
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url":    "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data),
				"detail": "high",
			},
		})
	}

	body := map[string]any{
		"model":                 c.cfg.OpenAILlmModel,
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": maxTokens,
		"messages": []map[string]any{{
			"role":    "user",
			"content": content,
		}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.OpenAIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, Usage{}, fmt.Errorf("%s vision error: %s", label, string(message))
	}

	var out chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	log.Printf(
		"openai %s parse usage: model=%s images=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		label, c.cfg.OpenAILlmModel, len(images), out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.TotalTokens,
	)
	if len(out.Choices) == 0 {
		return nil, Usage{}, fmt.Errorf("no choices")
	}
	return []byte(out.Choices[0].Message.Content), out.usage(), nil
}
