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

// reasoningHeadroomTokens is added to a reasoning model's budget, because its
// hidden reasoning counts against max_completion_tokens too. At "low" effort a
// receipt read used well under this.
const reasoningHeadroomTokens = 3000

// The keys below are the capture-schema fields the review sheet fills, plus
// five the handler reads and strips before normalising: documents_in_photo,
// other_papers, other_papers_detail, total_label and total_as_printed. They let
// the server flag a draft the model may have taken from the wrong paper, or
// whose amount does not match the figure it says it read.
//
// "Find the main document" is the rule that fixes the reported failure: a
// petrol bill photographed with an unrelated HDFC card slip on top was read as
// the slip's ₹7,647.60 and bank. On eleven synthetic bills (2026-10-11) the old
// prompt on gpt-4o-mini got that photo wrong on every run. This prompt on
// gpt-4.1-mini got all eleven amounts and dates right on repeated runs, and
// called the slip unrelated every time. "The shop's bill, not the slip on top
// of it" is what made the difference: before it, models still took the slip
// on some runs because it was the paper most in view.
const receiptImagePrompt = `You read one photo or screenshot of a bill, receipt, invoice, card or UPI payment slip, or payment confirmation, and return JSON only.

Step 1, find the main document. A photo can hold more than one paper: the shop's bill, the card-machine slip printed for that same purchase, and unrelated receipts lying beside or under it. The main document is the shop's own bill: the paper that lists what was bought (items, fuel, a service) with its total. A bank card-machine slip or payment screen is the main document only when there is no shop bill in the photo. Being on top, larger or more readable does not make a paper the main one. A second paper is for the same purchase only when it shows the same total as the main document, like the card slip printed for that bill. Every other paper is unrelated: never take its amount, date, shop, card or bank.

Step 2, return ONE transaction for the main document, never one per line item:
{"summary": string, "documents_in_photo": integer, "other_papers": "none|same_purchase|unrelated",
 "other_papers_detail": [{"shop": string|null, "amount": number|null, "date": "YYYY-MM-DD"|null}],
 "total_label": string|null, "total_as_printed": string|null,
 "type": "expense|income", "title": string|null, "amount": number|null, "currency": "INR, or the ISO code of the currency printed on the bill",
 "mode": "Cash|Bank Account|UPI|Credit Card|Wallets|null", "card_network": "Visa|Mastercard|Amex|Rupay|null",
 "account_hint": string|null, "category": "Food & Drinks|Transport|Travel|Shopping|Bills|Entertainment|Family/Gifts|Misc|Salary|Freelance|Interest|Refund|Other|null",
 "merchant": string|null, "tags": [string], "note": string|null, "date": "YYYY-MM-DD"|null, "time": "HH:MM"|null,
 "confidence": {"amount": 0..1, "merchant": 0..1, "date": 0..1, "category": 0..1, "mode": 0..1, "type": 0..1},
 "missing_fields": [string], "clarifications": [string]}

Rules:
- amount is the final amount actually paid on the main document: the grand total after tax, discounts, tips and round-off. Look for Grand Total, Net Amount, Net Payable, Amount Payable, Total Amount, Bill Amount, Invoice Total, Amount (Rs), Sale Amount or Paid. Never a subtotal, taxable value, MRP total, "you saved", a single item, an item count or a quantity.
- On a fuel pump receipt the amount is the sale Amount in rupees. Never the Rate, Volume or Litres, Preset, Totalizer, Density, odometer, nozzle, or a receipt or transaction number.
- total_label is the label printed beside the amount you chose, e.g. "Grand Total". total_as_printed copies that figure exactly as printed, with its commas and decimals, e.g. "1,24,560.00".
- Read each digit of the total carefully. Indian bills group digits as 1,24,560.00. If a digit of the total is unclear (faded, folded, glare, cut off), give your best reading and set confidence.amount below 0.6.
- documents_in_photo counts the separate bills, receipts and slips you can see, even partly. other_papers is "same_purchase" only when every extra paper shows the main document's total, "unrelated" when any does not, and "none" when there is only one. other_papers_detail lists each extra paper's shop, total and date as printed; [] when there is only one.
- type is "expense" unless the document clearly shows money received (a refund credit, salary slip, money received screen).
- title is 2-4 words a person would write, like "Dinner at Barbeque Nation" or "Petrol". merchant is the shop or brand name as printed, cleaned of legal suffixes.
- mode only when the main document or its own payment slip says how it was paid (UPI ref, card ending, "CASH"); card_network only when printed there. account_hint is the bank or wallet that paid, only when the main document or its own payment slip shows it (e.g. "HDFC", "Paytm"), never a card number, never from an unrelated paper.
- note is a short plain-language line about what was bought, e.g. "2 pizzas, 1 drink". Never copy addresses, phone numbers, GSTINs, card numbers or names of people.
- date is the transaction date printed on the main document. If the year is missing, use the most recent such date not after today. If no date is printed, null and list "date" in missing_fields.
- summary is one short line describing what you read, e.g. "Domino's bill, Rs 548, 12 Sep". It is shown back to the user.
- If the image is not a bill, receipt or payment record, or the total cannot be read, set amount to null and add a clarification saying what was unreadable.
Today is %s.`

// ParseReceiptImage asks the vision model for a single draft built from one
// receipt image.
func (c *OpenAIClient) ParseReceiptImage(ctx context.Context, image StatementImage, today string) ([]byte, Usage, error) {
	if len(image.Data) == 0 {
		return nil, Usage{}, fmt.Errorf("no receipt image")
	}
	prompt := fmt.Sprintf(receiptImagePrompt, today)
	return c.completeWithVisionModel(ctx, "receipt", prompt, []StatementImage{image}, receiptMaxTokens)
}
