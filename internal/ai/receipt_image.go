package ai

import (
	"context"
	"fmt"
)

// ReceiptImageParser reads one photographed bill or receipt into a capture
// draft. Like StatementImageParser it is optional: a parser that only handles
// text does not have to pretend it can see.
type ReceiptImageParser interface {
	ParseReceiptImage(ctx context.Context, image StatementImage, today string) ([]byte, Usage, error)
}

// receiptMaxTokens bounds the reply. A receipt becomes one draft, not a list of
// lines, so this is far smaller than the statement budget.
const receiptMaxTokens = 1200

// The keys below are the capture-schema fields the review sheet fills. Anything
// else the model adds is stripped by the draft normaliser, so the prompt only
// has to steer, not police.
const receiptImagePrompt = `Read this photo or screenshot of a bill, receipt, invoice or payment confirmation and return JSON only.
Return ONE transaction for the whole document, never one per line item:
{"summary": string, "type": "expense|income", "title": string|null, "amount": number|null, "currency": "INR",
 "mode": "Cash|Bank Account|UPI|Credit Card|Wallets|null", "card_network": "Visa|Mastercard|Amex|Rupay|null",
 "account_hint": string|null, "category": "Food & Drinks|Transport|Travel|Shopping|Bills|Entertainment|Family/Gifts|Misc|Salary|Freelance|Interest|Refund|Other|null",
 "merchant": string|null, "tags": [string], "note": string|null, "date": "YYYY-MM-DD"|null, "time": "HH:MM"|null,
 "confidence": {"amount": 0..1, "merchant": 0..1, "date": 0..1, "category": 0..1, "mode": 0..1, "type": 0..1},
 "missing_fields": [string], "clarifications": [string]}
Rules:
- amount is the final amount actually paid: the grand total after tax, discounts, tips and round-off. Never a subtotal or a single item. Positive number, no currency symbol.
- type is "expense" unless the document clearly shows money received (a refund credit, salary slip, money received screen).
- title is 2-4 words a person would write, like "Dinner at Barbeque Nation" or "Petrol". merchant is the shop or brand name as printed, cleaned of legal suffixes.
- mode only when the document says how it was paid (UPI ref, card ending, "CASH"); card_network only when printed. account_hint is the bank or wallet name when printed (e.g. "HDFC", "Paytm"), never a card number.
- note is a short plain-language line about what was bought, e.g. "2 pizzas, 1 drink". Never copy addresses, phone numbers, GSTINs, card numbers or names of people.
- date is the transaction date printed on the document. If the year is missing, use the most recent such date not after today. If no date is printed, null and list "date" in missing_fields.
- summary is one short line describing what you read, e.g. "Domino's bill, Rs 548, 12 Sep". It is shown back to the user.
- If the image is not a bill, receipt or payment record, or the total cannot be read, set amount to null and add a clarification saying what was unreadable.
Today is %s.`

// ParseReceiptImage asks the vision model for a single draft built from one
// receipt image.
func (c *OpenAIClient) ParseReceiptImage(ctx context.Context, image StatementImage, today string) ([]byte, Usage, error) {
	if len(image.Data) == 0 {
		return nil, Usage{}, fmt.Errorf("no receipt image")
	}
	return c.completeWithImages(ctx, "receipt", fmt.Sprintf(receiptImagePrompt, today), []StatementImage{image}, receiptMaxTokens)
}
