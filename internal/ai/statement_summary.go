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

// StatementSummaryReader reads the summary block of a credit-card statement —
// the bill's own figures and the card's terms — as opposed to its transaction
// rows. Optional, like the other readers: without it a statement still
// produces rows, just no prefilled bill.
type StatementSummaryReader interface {
	// ReadStatementText returns the summary and the rows from masked PDF text
	// in one call — the paid PDF read. Card numbers are masked by the caller
	// before this is reached.
	ReadStatementText(ctx context.Context, text, today string) ([]byte, Usage, error)
	// ReadStatementImages returns the summary and the rows in one call, so a
	// screenshot statement costs no more than it did before.
	ReadStatementImages(ctx context.Context, images []StatementImage, today string) ([]byte, Usage, error)
}

// The summary keys, shared by both prompts. Every amount is copied from the
// document; the caller drops any amount it cannot find printed in the text.
const statementSummaryKeys = `"summary":{
 "statement_date":"YYYY-MM-DD"|null, "due_date":"YYYY-MM-DD"|null,
 "total_due":number|null, "minimum_due":number|null,
 "credit_limit":number|null, "available_limit":number|null,
 "opening_balance":number|null, "payments":number|null, "purchases":number|null, "fees_and_charges":number|null,
 "annual_fee":number|null, "fee_waiver_spend":number|null, "renewal_month":"January".."December"|null,
 "card_last4":"1234"|null, "issuer":string|null}`

const statementSummaryRules = `Rules for the summary:
- Copy every amount exactly as printed, in rupees, as a positive number. Never calculate, estimate or invent one. Use null for anything not printed.
- total_due is the total amount due on this bill, never the minimum due, a limit, or a transaction.
- opening_balance is the previous balance / opening balance; payments is payments and credits received; purchases is purchases and debits; fees_and_charges is the fees, interest and taxes total, each only when the statement's account summary prints it.
- annual_fee is the card's annual or renewal membership fee before GST, only when the statement states it (as a charge on this bill or in its fee terms). fee_waiver_spend is the yearly spend above which that fee is waived, only when stated.
- renewal_month is the month the annual fee is charged: when stated, or the statement's month if the annual fee is charged on this bill.
- card_last4 is the last four digits of the card number. issuer is the bank's name.
- Never include names, addresses, phone numbers, email addresses or full card numbers.`

const statementLinesKeys = `"lines":[{"date":"YYYY-MM-DD","description":"merchant or bank description","amount":123.45,"type":"expense|income"}]`

const statementLinesRules = `Rules for lines: use expense for debits/purchases/fees/interest/EMIs and income for refunds/credits/payments received. Amounts are positive. Include transaction rows only; ignore totals, limits, rewards, headers and footers.`

const statementReadImagesPrompt = `Read these ordered credit-card statement screenshots and return JSON only:
{` + statementSummaryKeys + `,
 ` + statementLinesKeys + `}
` + statementSummaryRules + `
` + statementLinesRules + ` Screenshots can overlap: emit an identical row only once, but keep two genuinely separate transactions even if their amounts match.
Resolve dates without a year so they are not after %s. Use the statement date's cycle when it is visible.`

// statementReadTextPrompt is the PDF read. The text comes from the PDF's own
// text layer, where an issuer's table can arrive one cell per line or with its
// columns interleaved — the layouts a line-by-line pattern cannot follow.
const statementReadTextPrompt = `Read this credit-card statement text and return JSON only:
{` + statementSummaryKeys + `,
 ` + statementLinesKeys + `}
` + statementSummaryRules + `
` + statementLinesRules + ` Table cells may be split across lines or out of order; put each row's date, description and amount back together. Copy every amount exactly as printed. Two separate transactions with the same amount are both rows.
Resolve dates without a year so they are not after %s. Use the statement date's cycle when it is visible.
Statement text:
%s`

// statementTextMaxChars bounds the full-statement read. A month of card
// spending is a few pages; past this it is marketing and terms.
const statementTextMaxChars = 60000

func (c *OpenAIClient) ReadStatementText(ctx context.Context, text, today string) ([]byte, Usage, error) {
	if runes := []rune(text); len(runes) > statementTextMaxChars {
		text = string(runes[:statementTextMaxChars])
	}
	maxTokens := c.cfg.OpenAIStatementMaxTokens
	if maxTokens <= 0 {
		maxTokens = 4000
	}
	return c.chatJSON(ctx, "statement text read", fmt.Sprintf(statementReadTextPrompt, today, text), maxTokens)
}

func (c *OpenAIClient) ReadStatementImages(ctx context.Context, images []StatementImage, today string) ([]byte, Usage, error) {
	if len(images) == 0 {
		return nil, Usage{}, fmt.Errorf("no statement images")
	}
	maxTokens := c.cfg.OpenAIStatementMaxTokens
	if maxTokens <= 0 {
		maxTokens = 4000
	}
	return c.completeWithVisionModel(ctx, "statement read", fmt.Sprintf(statementReadImagesPrompt, today), images, maxTokens)
}

// chatJSON runs one JSON-mode chat completion with a single user message,
// whose content is either plain text or a list of multimodal parts.
func (c *OpenAIClient) chatJSON(ctx context.Context, label string, content any, maxTokens int) ([]byte, Usage, error) {
	if c.cfg.OpenAIKey == "" {
		return nil, Usage{}, fmt.Errorf("OPENAI_API_KEY missing")
	}
	body := map[string]any{
		"model":                 c.cfg.OpenAILlmModel,
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": maxTokens,
		"messages":              []map[string]any{{"role": "user", "content": content}},
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
		return nil, Usage{}, fmt.Errorf("%s error: %s", label, string(message))
	}
	var out chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	if len(out.Choices) == 0 {
		return nil, Usage{}, fmt.Errorf("no choices")
	}
	log.Printf("openai %s usage: model=%s prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		label, c.cfg.OpenAILlmModel, out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.TotalTokens)
	return []byte(out.Choices[0].Message.Content), out.usage(), nil
}
