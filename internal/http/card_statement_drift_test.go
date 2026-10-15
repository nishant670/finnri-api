package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

type driftStatementResponse struct {
	ID                     uint                    `json:"id"`
	StatementDate          string                  `json:"statement_date"`
	CycleStart             string                  `json:"cycle_start"`
	CycleEnd               string                  `json:"cycle_end"`
	DueDate                string                  `json:"due_date"`
	Status                 string                  `json:"status"`
	UnitemizedEntryID      *uint                   `json:"unitemized_entry_id"`
	StatementDaySuggestion *statementDaySuggestion `json:"statement_day_suggestion"`
}

type driftFixture struct {
	router *gin.Engine
	token  string
	userID uint
	card   models.Account
}

func newDriftFixture(t *testing.T, statementDay int) driftFixture {
	t.Helper()
	useSmokeDatabase(t)
	router := smokeRouter(t)
	// The smoke router carries a route subset without statements; these are
	// the two this file exercises, behind the same auth as production.
	statements := router.Group("/v1", AuthMiddleware())
	statements.POST("/accounts/:id/statements", (&Server{}).saveCardStatement)
	statements.PATCH("/statements/:id", jsonRequestLimits(&config.Config{MaxJSONKB: 64}), (&Server{}).updateCardStatement)
	auth := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "drift-" + t.Name()}, http.StatusOK)
	card := models.Account{
		UserID: auth.User.ID, Type: "credit_card", Name: "HDFC Regalia", Color: "#8257E5",
		CreditLimit: rupees(200000), StatementDay: statementDay, DueDay: 5,
	}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	return driftFixture{router: router, token: auth.Token, userID: auth.User.ID, card: card}
}

func (f driftFixture) save(t *testing.T, date string, total string, status int) driftStatementResponse {
	t.Helper()
	return performJSONRequest[driftStatementResponse](t, f.router, http.MethodPost,
		fmt.Sprintf("/v1/accounts/%d/statements", f.card.ID), f.token,
		map[string]any{"statement_date": date, "total_due": total}, status)
}

func (f driftFixture) patch(t *testing.T, id uint, date string, total string, status int) driftStatementResponse {
	t.Helper()
	return performJSONRequest[driftStatementResponse](t, f.router, http.MethodPatch,
		fmt.Sprintf("/v1/statements/%d", id), f.token,
		map[string]any{"statement_date": date, "total_due": total}, status)
}

func (f driftFixture) statements(t *testing.T) []models.CardStatement {
	t.Helper()
	var rows []models.CardStatement
	if err := database.DB.Where("account_id = ?", f.card.ID).Order("statement_date ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertCycle(t *testing.T, label string, got driftStatementResponse, start, end string) {
	t.Helper()
	if got.CycleStart != start || got.CycleEnd != end {
		t.Fatalf("%s cycle = %s..%s, want %s..%s", label, got.CycleStart, got.CycleEnd, start, end)
	}
}

// The property statementCycle's comment claims, held across a move: a bank
// that moves a card from the 15th to the 20th must not open a gap (16–20 Oct
// belonging to no bill) or an overlap (16–20 Nov in two bills).
func TestCyclesTileAcrossABankMove(t *testing.T) {
	f := newDriftFixture(t, 15)

	october := f.save(t, "2026-10-15", "1000", http.StatusCreated)
	assertCycle(t, "October", october, "2026-09-16", "2026-10-15")

	november := f.save(t, "2026-11-20", "1000", http.StatusCreated)
	assertCycle(t, "November", november, "2026-10-16", "2026-11-20")

	// The user declined to move the card, so the anchor is still the 15th —
	// the neighbour, not the anchor, is what keeps December from overlapping.
	december := f.save(t, "2026-12-20", "1000", http.StatusCreated)
	assertCycle(t, "December", december, "2026-11-21", "2026-12-20")
}

// Ordinary months are unchanged by anchoring on the neighbour, including the
// 31st-of-month clamping through February.
func TestNeighbourAnchoringKeepsMonthEndClamping(t *testing.T) {
	f := newDriftFixture(t, 31)

	january := f.save(t, "2027-01-31", "1000", http.StatusCreated)
	february := f.save(t, "2027-02-28", "1000", http.StatusCreated)
	march := f.save(t, "2027-03-31", "1000", http.StatusCreated)

	assertCycle(t, "January", january, "2027-01-01", "2027-01-31")
	assertCycle(t, "February", february, "2027-02-01", "2027-02-28")
	assertCycle(t, "March", march, "2027-03-01", "2027-03-31")
	for _, s := range []driftStatementResponse{january, february, march} {
		if s.StatementDaySuggestion != nil {
			t.Fatalf("%s suggested a move on a clamped month-end: %+v", s.StatementDate, s.StatementDaySuggestion)
		}
	}
}

// A missing month must not stretch the next cycle across two months.
func TestDistantPreviousStatementFallsBackToTheAnchor(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-08-15", "1000", http.StatusCreated)
	october := f.save(t, "2026-10-15", "1000", http.StatusCreated)
	assertCycle(t, "October", october, "2026-09-16", "2026-10-15")
}

func TestPatchCorrectsTheDateInPlace(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)
	november := f.save(t, "2026-11-15", "5000", http.StatusCreated)
	if november.UnitemizedEntryID == nil {
		t.Fatal("expected an unitemised bucket for a bill with no card spend")
	}

	moved := f.patch(t, november.ID, "2026-11-20", "5000", http.StatusOK)

	if moved.ID != november.ID {
		t.Fatalf("patch returned statement %d, want the same statement %d", moved.ID, november.ID)
	}
	if rows := f.statements(t); len(rows) != 2 {
		t.Fatalf("card has %d statements after correcting a date, want 2", len(rows))
	}
	assertCycle(t, "corrected November", moved, "2026-10-16", "2026-11-20")
	if moved.DueDate != "2026-12-05" {
		t.Fatalf("due date = %s, want it re-derived to 2026-12-05", moved.DueDate)
	}

	var bucket models.Entry
	if err := database.DB.First(&bucket, *moved.UnitemizedEntryID).Error; err != nil {
		t.Fatal(err)
	}
	if bucket.Date != "2026-11-20" {
		t.Fatalf("unitemised entry dated %s, want it moved to 2026-11-20", bucket.Date)
	}
}

// The edit sheet does not show notes, so an edit must not blank them.
func TestPatchKeepsFieldsTheClientDidNotSend(t *testing.T) {
	f := newDriftFixture(t, 15)
	saved := performJSONRequest[driftStatementResponse](t, f.router, http.MethodPost,
		fmt.Sprintf("/v1/accounts/%d/statements", f.card.ID), f.token,
		map[string]any{"statement_date": "2026-11-15", "total_due": "1000", "notes": "Disputed ₹200 lounge fee", "source": "sms"},
		http.StatusCreated)

	f.patch(t, saved.ID, "2026-11-16", "1200", http.StatusOK)

	var row models.CardStatement
	if err := database.DB.First(&row, saved.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Notes != "Disputed ₹200 lounge fee" || row.Source != "sms" {
		t.Fatalf("after edit notes=%q source=%q, want both kept", row.Notes, row.Source)
	}
}

func TestPatchRetilesTheFollowingStatement(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)
	november := f.save(t, "2026-11-15", "1000", http.StatusCreated)
	f.save(t, "2026-12-15", "1000", http.StatusCreated)

	f.patch(t, november.ID, "2026-11-20", "1000", http.StatusOK)

	rows := f.statements(t)
	december := rows[2]
	if december.CycleStart != "2026-11-21" || december.CycleEnd != "2026-12-15" {
		t.Fatalf("December cycle = %s..%s, want 2026-11-21..2026-12-15", december.CycleStart, december.CycleEnd)
	}
}

func TestPatchRefusesATakenOrReorderingDate(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)
	november := f.save(t, "2026-11-15", "1000", http.StatusCreated)
	f.save(t, "2026-12-15", "1000", http.StatusCreated)

	taken := performJSONRequest[map[string]any](t, f.router, http.MethodPatch,
		fmt.Sprintf("/v1/statements/%d", november.ID), f.token,
		map[string]any{"statement_date": "2026-12-15", "total_due": "1000"}, http.StatusConflict)
	if taken["error"] != "statement_date_taken" {
		t.Fatalf("error = %v, want statement_date_taken", taken["error"])
	}

	reordered := performJSONRequest[map[string]any](t, f.router, http.MethodPatch,
		fmt.Sprintf("/v1/statements/%d", november.ID), f.token,
		map[string]any{"statement_date": "2026-12-18", "total_due": "1000"}, http.StatusConflict)
	if reordered["error"] != "statement_date_out_of_order" {
		t.Fatalf("error = %v, want statement_date_out_of_order", reordered["error"])
	}

	if rows := f.statements(t); rows[1].StatementDate != "2026-11-15" {
		t.Fatalf("a refused patch moved the statement to %s", rows[1].StatementDate)
	}
}

func TestPatchIsScopedToTheOwner(t *testing.T) {
	f := newDriftFixture(t, 15)
	statement := f.save(t, "2026-11-15", "1000", http.StatusCreated)

	stranger := performJSONRequest[AuthResponse](t, f.router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "drift-stranger"}, http.StatusOK)
	performJSONRequest[map[string]any](t, f.router, http.MethodPatch,
		fmt.Sprintf("/v1/statements/%d", statement.ID), stranger.Token,
		map[string]any{"statement_date": "2026-11-20", "total_due": "1"}, http.StatusNotFound)
}

func TestSuggestsTheObservedDayOnlyForTheLatestStatement(t *testing.T) {
	f := newDriftFixture(t, 15)

	onDay := f.save(t, "2026-10-15", "1000", http.StatusCreated)
	if onDay.StatementDaySuggestion != nil {
		t.Fatalf("a statement on the card's day suggested a move: %+v", onDay.StatementDaySuggestion)
	}

	moved := f.save(t, "2026-11-20", "1000", http.StatusCreated)
	if moved.StatementDaySuggestion == nil ||
		moved.StatementDaySuggestion.CurrentDay != 15 || moved.StatementDaySuggestion.ObservedDay != 20 {
		t.Fatalf("suggestion = %+v, want 15 → 20", moved.StatementDaySuggestion)
	}

	// Correcting the older bill says nothing about where the card bills now.
	older := f.patch(t, onDay.ID, "2026-10-17", "1000", http.StatusOK)
	if older.StatementDaySuggestion != nil {
		t.Fatalf("correcting an older statement suggested a move: %+v", older.StatementDaySuggestion)
	}

	// Nothing was changed on the card by the suggestion itself.
	var card models.Account
	if err := database.DB.First(&card, f.card.ID).Error; err != nil {
		t.Fatal(err)
	}
	if card.StatementDay != 15 {
		t.Fatalf("statement_day = %d, want it left at 15 until the user accepts", card.StatementDay)
	}
}

// The first bill still teaches the card its day, silently, as before.
func TestFirstBillStillInfersTheDayWithoutASuggestion(t *testing.T) {
	f := newDriftFixture(t, 0)
	first := f.save(t, "2026-11-20", "1000", http.StatusCreated)
	if first.StatementDaySuggestion != nil {
		t.Fatalf("first bill suggested a move: %+v", first.StatementDaySuggestion)
	}
	var card models.Account
	if err := database.DB.First(&card, f.card.ID).Error; err != nil {
		t.Fatal(err)
	}
	if card.StatementDay != 20 {
		t.Fatalf("statement_day = %d, want 20 inferred from the first bill", card.StatementDay)
	}
}

// Declining keeps the card on the 15th. The job must then not open a second
// draft on the 15th beside the bill the user re-dated to the 20th.
func TestDraftIsNotReopenedBesideACorrectedDate(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)

	opened, err := openDueCardStatementDrafts(f.userID, time.Date(2026, time.November, 16, 9, 0, 0, 0, time.UTC))
	if err != nil || len(opened) != 1 {
		t.Fatalf("opened %d drafts (err %v), want the November draft", len(opened), err)
	}
	f.patch(t, opened[0].ID, "2026-11-20", "1000", http.StatusOK)

	again, err := openDueCardStatementDrafts(f.userID, time.Date(2026, time.November, 25, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("job re-opened %d drafts beside the corrected bill", len(again))
	}
	if rows := f.statements(t); len(rows) != 2 {
		t.Fatalf("card has %d statements, want 2", len(rows))
	}
}

// Saving the bill on its real date prices the job's draft instead of leaving
// a phantom unpaid bill on the usual day.
func TestSavingNearADraftPricesTheDraft(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)
	opened, err := openDueCardStatementDrafts(f.userID, time.Date(2026, time.November, 16, 9, 0, 0, 0, time.UTC))
	if err != nil || len(opened) != 1 {
		t.Fatalf("opened %d drafts (err %v)", len(opened), err)
	}

	saved := f.save(t, "2026-11-18", "2400", http.StatusCreated)

	if saved.ID != opened[0].ID {
		t.Fatalf("saved statement %d, want the draft %d to be priced", saved.ID, opened[0].ID)
	}
	if saved.Status != statementStatusUnpaid || saved.StatementDate != "2026-11-18" {
		t.Fatalf("saved = %s on %s, want unpaid on 2026-11-18", saved.Status, saved.StatementDate)
	}
	if rows := f.statements(t); len(rows) != 2 {
		t.Fatalf("card has %d statements, want 2", len(rows))
	}
}

// Accepting the suggestion is an ordinary account update; from then on the
// job anchors drafts on the new day, and they still tile.
func TestAcceptedMoveAnchorsFutureDrafts(t *testing.T) {
	f := newDriftFixture(t, 15)
	f.save(t, "2026-10-15", "1000", http.StatusCreated)
	f.save(t, "2026-11-20", "1000", http.StatusCreated)

	if err := database.DB.Model(&models.Account{}).Where("id = ?", f.card.ID).
		Update("statement_day", 20).Error; err != nil {
		t.Fatal(err)
	}

	opened, err := openDueCardStatementDrafts(f.userID, time.Date(2026, time.December, 21, 9, 0, 0, 0, time.UTC))
	if err != nil || len(opened) != 1 {
		t.Fatalf("opened %d drafts (err %v), want December's", len(opened), err)
	}
	draft := opened[0]
	if draft.StatementDate != "2026-12-20" || draft.CycleStart != "2026-11-21" || draft.CycleEnd != "2026-12-20" {
		t.Fatalf("December draft = %s, cycle %s..%s; want 2026-12-20, 2026-11-21..2026-12-20",
			draft.StatementDate, draft.CycleStart, draft.CycleEnd)
	}
}

// The read path's card-update offer compares against the clamped anchor too.
func TestReadPathDoesNotFlagAClampedMonthEnd(t *testing.T) {
	card := models.Account{StatementDay: 31}
	updates, _ := compareCardWithStatement(card, statementSummary{StatementDate: "2026-11-30"})
	for _, update := range updates {
		if update.Field == "statement_day" {
			t.Fatalf("a 31st card dated 30 November was offered a new statement day: %+v", update)
		}
	}
	updates, _ = compareCardWithStatement(models.Account{StatementDay: 15}, statementSummary{StatementDate: "2026-11-20"})
	found := false
	for _, update := range updates {
		found = found || update.Field == "statement_day"
	}
	if !found {
		t.Fatal("a real move from the 15th to the 20th was not offered")
	}
}
