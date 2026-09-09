package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

type StatementAlertParser interface {
	ParseStatementAlert(ctx context.Context, text, receivedDate string) ([]byte, Usage, error)
}

const statementAlertPrompt = `Extract a credit-card bill alert into JSON only.
Return {"statement_date":"YYYY-MM-DD or empty","due_date":"YYYY-MM-DD or empty","total_due":123.45,"minimum_due":0}.
Copy amounts from the alert; never calculate or invent one. total_due is the full bill/amount due, not the minimum due, outstanding limit, credit limit, available limit, or a transaction amount.
Resolve dates without a year around the received date %s. If the statement date is absent, leave it empty. If minimum due is absent, use 0.
Alert text: %s`

func (c *OpenAIClient) ParseStatementAlert(ctx context.Context, text, receivedDate string) ([]byte, Usage, error) {
	if c.cfg.OpenAIKey == "" {
		return nil, Usage{}, fmt.Errorf("OPENAI_API_KEY missing")
	}
	body := map[string]any{
		"model":                 c.cfg.OpenAILlmModel,
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": c.cfg.OpenAIMaxTokens,
		"messages": []map[string]string{{
			"role": "user", "content": fmt.Sprintf(statementAlertPrompt, receivedDate, text),
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
		return nil, Usage{}, fmt.Errorf("statement alert error: %s", string(message))
	}
	var out chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	if len(out.Choices) == 0 {
		return nil, Usage{}, fmt.Errorf("no choices")
	}
	log.Printf("openai statement alert usage: model=%s prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		c.cfg.OpenAILlmModel, out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.TotalTokens)
	return []byte(out.Choices[0].Message.Content), out.usage(), nil
}
