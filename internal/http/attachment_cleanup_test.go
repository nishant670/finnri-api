package http

import (
	"fmt"
	"net/http"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

// A file left on disk with no record naming it can be attached by anybody who
// once saw its name, so every path that drops the last mention removes it.
func TestDroppingTheLastUseRemovesTheFile(t *testing.T) {
	// Each case arranges records of the owner's that name receipt, and returns
	// the action that drops them.
	cases := map[string]func(t *testing.T, w attachmentWorld, receipt string) func(){
		"delete split bill": func(t *testing.T, w attachmentWorld, receipt string) func() {
			bill, _ := w.ownerBill(t, receipt)
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/split/bills/%d", bill.ID), nil) }
		},
		"replace split bill receipt": func(t *testing.T, w attachmentWorld, receipt string) func() {
			bill, body := w.ownerBill(t, receipt)
			return func() {
				body["attachment"] = w.upload(t)
				w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), body)
			}
		},
		"replace group photo": func(t *testing.T, w attachmentWorld, receipt string) func() {
			group := w.ownerGroup(t, receipt)
			return func() {
				w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID),
					map[string]any{"name": "Goa", "kind": "trip", "photo_url": w.upload(t)})
			}
		},
		"remove group photo": func(t *testing.T, w attachmentWorld, receipt string) func() {
			group := w.ownerGroup(t, receipt)
			return func() {
				w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID),
					map[string]any{"name": "Goa", "kind": "trip", "photo_url": ""})
			}
		},
		"delete group, keeping entries": func(t *testing.T, w attachmentWorld, receipt string) func() {
			group := w.ownerGroup(t, "")
			mustCreate(t, &models.SplitBill{UserID: w.owner.ID, GroupID: &group.ID, Title: "b", TotalAmount: 100, Currency: "INR", Date: "2026-11-01", Attachment: receipt})
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/split/groups/%d", group.ID), nil) }
		},
		"delete group and its entries": func(t *testing.T, w attachmentWorld, receipt string) func() {
			group := w.ownerGroup(t, "")
			entry := w.forceSecondReference(t, w.owner.ID, receipt)
			mustCreate(t, &models.SplitBill{UserID: w.owner.ID, GroupID: &group.ID, EntryID: &entry.ID, Title: "b", TotalAmount: 100, Currency: "INR", Date: "2026-11-01"})
			return func() {
				w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/split/groups/%d?entries=delete", group.ID), nil)
			}
		},
		"delete entry whose split has its own receipt": func(t *testing.T, w attachmentWorld, receipt string) func() {
			entry := w.entryWithSplitReceipt(t, receipt)
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", entry.ID), nil) }
		},
		"remove a split that has its own receipt": func(t *testing.T, w attachmentWorld, receipt string) func() {
			entry := w.entryWithSplitReceipt(t, receipt)
			return func() {
				w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), map[string]any{"split": nil})
			}
		},
		"clear an entry's receipt over its split's own": func(t *testing.T, w attachmentWorld, receipt string) func() {
			entry := w.entryWithSplitReceipt(t, receipt)
			return func() {
				w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), map[string]any{"attachment": ""})
			}
		},
		"delete card statement with a payment": func(t *testing.T, w attachmentWorld, receipt string) func() {
			statement, _ := w.paidStatement(t, receipt)
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/statements/%d", statement.ID), nil) }
		},
		"delete card statement with an unitemised entry": func(t *testing.T, w attachmentWorld, receipt string) func() {
			statement := w.statement(t)
			bucket := w.forceSecondReference(t, w.owner.ID, receipt)
			if err := database.DB.Model(&statement).Update("unitemized_entry_id", bucket.ID).Error; err != nil {
				t.Fatal(err)
			}
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/statements/%d", statement.ID), nil) }
		},
		"delete card payment": func(t *testing.T, w attachmentWorld, receipt string) func() {
			statement, payment := w.paidStatement(t, receipt)
			return func() {
				w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/statements/%d/payments/%d", statement.ID, payment.ID), nil)
			}
		},
		"unitemised entry cleared": func(t *testing.T, w attachmentWorld, receipt string) func() {
			statement := w.statement(t)
			bucket := w.forceSecondReference(t, w.owner.ID, receipt)
			statement.UnitemizedEntryID = &bucket.ID
			return func() {
				if err := clearUnitemizedEntry(&statement); err != nil {
					t.Fatal(err)
				}
			}
		},
		"convert a purchase to EMI": func(t *testing.T, w attachmentWorld, receipt string) func() {
			card := createTestCard(t, w.owner.ID)
			source := createCardSpend(t, w.owner.ID, card.ID, "2099-01-31", "6000.00")
			if err := database.DB.Model(&source).Update("attachment", receipt).Error; err != nil {
				t.Fatal(err)
			}
			return func() {
				w.ownerDoes(t, http.MethodPost, fmt.Sprintf("/v1/accounts/%d/emi-plans", card.ID), map[string]any{
					"title": "Laptop", "principal": "6000.00", "tenure_months": 6,
					"purchased_on": "2099-01-31", "source_entry_id": source.ID,
				})
			}
		},
		"delete EMI plan": func(t *testing.T, w attachmentWorld, receipt string) func() {
			card := createTestCard(t, w.owner.ID)
			plan := createNoCostEMIPlan(t, w.owner.ID, card, "2099-02-10")
			entry := w.forceSecondReference(t, w.owner.ID, receipt)
			if err := database.DB.Model(&models.CardEMIInstallment{}).
				Where("plan_id = ? AND seq = ?", plan.ID, 1).Update("entry_id", entry.ID).Error; err != nil {
				t.Fatal(err)
			}
			return func() { w.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/v1/emi-plans/%d", plan.ID), nil) }
		},
		"delete account with a group photo": func(t *testing.T, w attachmentWorld, receipt string) func() {
			w.ownerGroup(t, receipt)
			return func() { w.ownerDoes(t, http.MethodDelete, "/v1/user", nil) }
		},
	}
	for name, arrange := range cases {
		for _, shared := range []bool{false, true} {
			title := name + ", the only use"
			if shared {
				title = name + ", used elsewhere too"
			}
			t.Run(title, func(t *testing.T) {
				w := newAttachmentWorld(t)
				receipt := w.upload(t)
				drop := arrange(t, w, receipt)
				if shared {
					w.forceSecondReference(t, w.member.ID, receipt)
				}
				drop()
				if fileExists(receipt) != shared {
					t.Fatalf("file exists = %v, want %v", fileExists(receipt), shared)
				}
			})
		}
	}
}

func TestTitleEditRemovesNoFile(t *testing.T) {
	w := newAttachmentWorld(t)
	receipt := w.upload(t)
	entry := w.entryWithSplitReceipt(t, receipt)
	w.ownerDoes(t, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), map[string]any{"title": "Renamed"})
	if !fileExists(receipt) {
		t.Fatal("renaming an entry removed its split's receipt")
	}
}

func (w attachmentWorld) ownerDoes(t *testing.T, method, target string, body any) {
	t.Helper()
	if status, response := w.request(t, method, target, w.ownerToken, body); status >= 300 {
		t.Fatalf("%s %s: %d %s", method, target, status, response)
	}
}

func (w attachmentWorld) ownerFriend(t *testing.T) models.SplitFriend {
	t.Helper()
	return performJSONRequest[models.SplitFriend](t, w.router, http.MethodPost, "/v1/split/friends", w.ownerToken,
		map[string]any{"name": "Riya"}, http.StatusCreated)
}

func (w attachmentWorld) ownerBill(t *testing.T, receipt string) (models.SplitBill, map[string]any) {
	t.Helper()
	friend := w.ownerFriend(t)
	body := map[string]any{
		"title": "Dinner", "total_amount": "500", "date": "2026-11-20", "attachment": receipt,
		"participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "200", "direction": splitDirectionFriendOwesUser}},
	}
	return performJSONRequest[models.SplitBill](t, w.router, http.MethodPost, "/v1/split/bills", w.ownerToken, body, http.StatusCreated), body
}

func (w attachmentWorld) ownerGroup(t *testing.T, photo string) models.SplitGroup {
	t.Helper()
	return performJSONRequest[models.SplitGroup](t, w.router, http.MethodPost, "/v1/split/groups", w.ownerToken,
		map[string]any{"name": "Goa", "kind": "trip", "photo_url": photo}, http.StatusCreated)
}

// A bill created against an entry can carry a receipt the entry does not.
func (w attachmentWorld) entryWithSplitReceipt(t *testing.T, receipt string) models.Entry {
	t.Helper()
	entry := w.createEntry(t, w.ownerToken, "")
	mustCreate(t, &models.SplitBill{UserID: w.owner.ID, EntryID: &entry.ID, Title: "b", TotalAmount: 100, Currency: "INR", Date: "2026-11-01", Attachment: receipt})
	return entry
}

func (w attachmentWorld) statement(t *testing.T) models.CardStatement {
	t.Helper()
	card := createTestCard(t, w.owner.ID)
	statement := models.CardStatement{
		UserID: w.owner.ID, AccountID: card.ID, CycleStart: "2026-10-11", CycleEnd: "2026-11-10",
		StatementDate: "2026-11-10", DueDate: "2026-11-28", TotalDue: 100, Currency: "INR",
	}
	mustCreate(t, &statement)
	return statement
}

func (w attachmentWorld) paidStatement(t *testing.T, receipt string) (models.CardStatement, models.CardStatementPayment) {
	t.Helper()
	statement := w.statement(t)
	bank := w.forceSecondReference(t, w.owner.ID, receipt)
	payment := models.CardStatementPayment{
		UserID: w.owner.ID, StatementID: statement.ID, AccountID: statement.AccountID,
		BankEntryID: &bank.ID, Amount: 100, PaidOn: "2026-11-20",
	}
	mustCreate(t, &payment)
	return statement, payment
}
