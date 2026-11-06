package http

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func seedEventEntry(t *testing.T, userID uint, entryType string, amount models.Money, date string) models.Entry {
	t.Helper()
	entry := models.Entry{
		UserID: userID, Title: "Party " + date, Type: entryType, Amount: amount, Category: "Food & Drinks", Date: date,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatalf("failed to seed entry: %v", err)
	}
	return entry
}

// seedEntrySplit records a bill on an entry, the way the split sheet writes
// one: each participant row is what that friend owes the author, or (for
// user_owes_friend) what the author owes the friend who paid.
func seedEntrySplit(t *testing.T, userID uint, entry models.Entry, total models.Money, shares map[string]models.Money, direction string) {
	t.Helper()
	bill := models.SplitBill{UserID: userID, EntryID: &entry.ID, Title: entry.Title, TotalAmount: total, Date: entry.Date}
	if err := database.DB.Create(&bill).Error; err != nil {
		t.Fatalf("failed to seed bill: %v", err)
	}
	for name, share := range shares {
		friend := models.SplitFriend{UserID: userID, Name: name}
		if err := database.DB.Create(&friend).Error; err != nil {
			t.Fatalf("failed to seed friend: %v", err)
		}
		participant := models.SplitParticipant{
			UserID: userID, BillID: bill.ID, FriendID: friend.ID, ShareAmount: share, Direction: direction,
		}
		if err := database.DB.Create(&participant).Error; err != nil {
			t.Fatalf("failed to seed participant: %v", err)
		}
	}
}

func createTestEvent(t *testing.T, router *gin.Engine, token string, body map[string]any) EventResponse {
	t.Helper()
	return performJSONRequest[EventResponse](t, router, http.MethodPost, "/v1/events", token, body, http.StatusCreated)
}

func eventPath(id uint, suffix string) string {
	return fmt.Sprintf("/v1/events/%d%s", id, suffix)
}

func TestEventCreateValidatesAndCleansInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	created := createTestEvent(t, router, token, map[string]any{
		"name": "  Diwali   party ", "emoji": " 🎉 ", "start_date": "2026-10-31", "end_date": "2026-11-01", "budget": 15000,
	})
	if created.Event.Name != "Diwali party" || created.Event.Emoji != "🎉" {
		t.Fatalf("name and emoji should be cleaned, got %#v", created.Event)
	}
	if created.Event.StartDate == nil || *created.Event.StartDate != "2026-10-31" {
		t.Fatalf("start date = %v", created.Event.StartDate)
	}
	if created.Summary.BudgetLeft == nil || *created.Summary.BudgetLeft != models.Money(1500000) {
		t.Fatalf("a new event's budget is all left, got %v", created.Summary.BudgetLeft)
	}

	// Empty dates mean no dates, not a bad date.
	undated := createTestEvent(t, router, token, map[string]any{"name": "House move", "start_date": "", "end_date": ""})
	if undated.Event.StartDate != nil || undated.Event.EndDate != nil {
		t.Fatalf("empty dates should be stored as none, got %#v", undated.Event)
	}

	cases := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "   "}, "name"},
		{map[string]any{"name": strings.Repeat("a", 61)}, "name"},
		{map[string]any{"name": "Trip", "start_date": "2026-11-05", "end_date": "2026-11-01"}, "end_date"},
		{map[string]any{"name": "Trip", "start_date": "05/11/2026"}, "start_date"},
		{map[string]any{"name": "Trip", "budget": 0}, "budget"},
	}
	for _, tc := range cases {
		got := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/events", token, tc.body, http.StatusUnprocessableEntity)
		fields, _ := got["fields"].(map[string]any)
		if _, ok := fields[tc.field]; !ok {
			t.Fatalf("body %v: expected a %s error, got %v", tc.body, tc.field, got)
		}
	}
}

func TestEventsAreOnlyVisibleToTheirOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	other, otherToken := createBillingTestUserSession(t)

	event := createTestEvent(t, router, token, map[string]any{"name": "Mine"})
	theirEntry := seedEventEntry(t, other.ID, "expense", 50000, "2026-11-01")

	performJSONRequest[map[string]any](t, router, http.MethodGet, eventPath(event.Event.ID, ""), otherToken, nil, http.StatusNotFound)
	performJSONRequest[map[string]any](t, router, http.MethodPut, eventPath(event.Event.ID, ""), otherToken,
		map[string]any{"name": "Theirs now"}, http.StatusNotFound)
	performJSONRequest[map[string]any](t, router, http.MethodDelete, eventPath(event.Event.ID, ""), otherToken, nil, http.StatusNotFound)
	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(event.Event.ID, "/entries"), otherToken,
		map[string]any{"entry_ids": []uint{theirEntry.ID}}, http.StatusNotFound)

	list := performJSONRequest[[]EventListItem](t, router, http.MethodGet, "/v1/events", otherToken, nil, http.StatusOK)
	if len(list) != 0 {
		t.Fatalf("another user's events leaked into the list: %#v", list)
	}

	// Nor can an entry be pointed at somebody else's event.
	got := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/entries", otherToken, map[string]any{
		"title": "Cake", "type": "expense", "amount": 500, "date": "2026-11-01", "category": "Food & Drinks", "mode": "cash", "event_id": event.Event.ID,
	}, http.StatusUnprocessableEntity)
	if fields, _ := got["fields"].(map[string]any); fields["event_id"] == nil {
		t.Fatalf("expected an event_id error, got %v", got)
	}
}

func TestEventBulkAddIsAllOrNothingAndMovesBetweenEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	other, _ := createBillingTestUserSession(t)

	party := createTestEvent(t, router, token, map[string]any{"name": "Party"})
	trip := createTestEvent(t, router, token, map[string]any{"name": "Trip"})
	snacks := seedEventEntry(t, user.ID, "expense", 120000, "2026-10-31")
	dinner := seedEventEntry(t, user.ID, "expense", 240000, "2026-11-01")
	salary := seedEventEntry(t, user.ID, "income", 5000000, "2026-11-01")
	cardBill := seedEventEntry(t, user.ID, "expense", 900000, "2026-11-01")
	database.DB.Model(&cardBill).Update("purpose_type", cardPaymentPurposeType)
	theirs := seedEventEntry(t, other.ID, "expense", 10000, "2026-11-01")

	refused := performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(party.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{snacks.ID, salary.ID, cardBill.ID, theirs.ID}}, http.StatusUnprocessableEntity)
	if fmt.Sprint(refused["not_allowed"]) != fmt.Sprintf("[%d %d]", salary.ID, cardBill.ID) ||
		fmt.Sprint(refused["not_found"]) != fmt.Sprintf("[%d]", theirs.ID) {
		t.Fatalf("refusal should name the bad ids, got %v", refused)
	}
	var linked int64
	database.DB.Model(&models.Entry{}).Where("event_id IS NOT NULL").Count(&linked)
	if linked != 0 {
		t.Fatalf("a refused bulk add changed %d entries", linked)
	}

	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(trip.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{dinner.ID}}, http.StatusOK)
	added := performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(party.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{snacks.ID, dinner.ID, snacks.ID}}, http.StatusOK)
	if added["added"] != float64(2) || added["moved"] != float64(1) {
		t.Fatalf("expected 2 added with the dinner moved from the trip, got %v", added)
	}

	list := performJSONRequest[[]EventListItem](t, router, http.MethodGet, "/v1/events", token, nil, http.StatusOK)
	totals := map[string]EventListItem{}
	for _, item := range list {
		totals[item.Name] = item
	}
	if totals["Party"].EntryCount != 2 || totals["Party"].TotalPaid != 360000 || totals["Trip"].EntryCount != 0 {
		t.Fatalf("unexpected list totals: %#v", totals)
	}

	removed := performJSONRequest[map[string]any](t, router, http.MethodDelete, eventPath(party.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{snacks.ID}}, http.StatusOK)
	if removed["removed"] != float64(1) {
		t.Fatalf("expected one removed, got %v", removed)
	}

	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(party.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{}}, http.StatusUnprocessableEntity)
	tooMany := make([]uint, maxEventEntriesPerCall+1)
	for index := range tooMany {
		tooMany[index] = uint(index + 1)
	}
	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(party.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": tooMany}, http.StatusUnprocessableEntity)
}

func TestDeletingAnEventKeepsItsTransactions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)

	event := createTestEvent(t, router, token, map[string]any{"name": "Party"})
	first := seedEventEntry(t, user.ID, "expense", 1000, "2026-11-01")
	second := seedEventEntry(t, user.ID, "expense", 2000, "2026-11-01")
	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(event.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{first.ID, second.ID}}, http.StatusOK)

	got := performJSONRequest[map[string]any](t, router, http.MethodDelete, eventPath(event.Event.ID, ""), token, nil, http.StatusOK)
	if got["unlinked"] != float64(2) {
		t.Fatalf("expected two unlinked, got %v", got)
	}
	var entries []models.Entry
	database.DB.Where("user_id = ?", user.ID).Find(&entries)
	if len(entries) != 2 {
		t.Fatalf("deleting an event must keep its transactions, found %d", len(entries))
	}
	for _, entry := range entries {
		if entry.EventID != nil {
			t.Fatalf("entry %d still points at the deleted event", entry.ID)
		}
	}
	performJSONRequest[map[string]any](t, router, http.MethodGet, eventPath(event.Event.ID, ""), token, nil, http.StatusNotFound)
}

func TestEntrySaveAndUpdateCarryTheEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	event := createTestEvent(t, router, token, map[string]any{"name": "Party"})

	saved := performJSONRequest[models.Entry](t, router, http.MethodPost, "/v1/entries", token, map[string]any{
		"title": "Cake", "type": "expense", "amount": 800, "date": "2026-11-01", "category": "Food & Drinks", "mode": "cash", "event_id": event.Event.ID,
	}, http.StatusCreated)
	if saved.EventID == nil || *saved.EventID != event.Event.ID {
		t.Fatalf("saved entry event = %v", saved.EventID)
	}

	got := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/entries", token, map[string]any{
		"title": "Gift money", "type": "income", "amount": 800, "date": "2026-11-01", "category": "Other", "mode": "cash", "event_id": event.Event.ID,
	}, http.StatusUnprocessableEntity)
	if fields, _ := got["fields"].(map[string]any); fields["event_id"] != entryEventMessage {
		t.Fatalf("income with an event should be refused, got %v", got)
	}

	// An edit that leaves event_id out keeps it.
	edited := performJSONRequest[models.Entry](t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", saved.ID), token,
		map[string]any{"title": "Chocolate cake"}, http.StatusOK)
	if edited.EventID == nil {
		t.Fatalf("an edit that did not mention the event removed it")
	}

	// Turning it into income drops the event rather than refusing the edit.
	income := performJSONRequest[models.Entry](t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", saved.ID), token,
		map[string]any{"type": "income", "category": "Other"}, http.StatusOK)
	if income.EventID != nil {
		t.Fatalf("income kept its event: %v", income.EventID)
	}

	back := performJSONRequest[models.Entry](t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", saved.ID), token,
		map[string]any{"type": "expense", "category": "Food & Drinks", "event_id": event.Event.ID}, http.StatusOK)
	if back.EventID == nil || *back.EventID != event.Event.ID {
		t.Fatalf("setting the event on edit failed: %v", back.EventID)
	}
	cleared := performJSONRequest[models.Entry](t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", saved.ID), token,
		map[string]any{"event_id": nil}, http.StatusOK)
	if cleared.EventID != nil {
		t.Fatalf("event_id null should clear it, got %v", cleared.EventID)
	}
}

func TestEventSummaryNetsOutWhatFriendsOwe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	event := createTestEvent(t, router, token, map[string]any{"name": "Party", "budget": 10000})

	// ₹1,200 snacks, nobody else's.
	snacks := seedEventEntry(t, user.ID, "expense", 120000, "2026-10-31")
	// ₹2,400 dinner I paid, split equally three ways: two friends owe ₹800 each.
	dinner := seedEventEntry(t, user.ID, "expense", 240000, "2026-11-01")
	seedEntrySplit(t, user.ID, dinner, 240000, map[string]models.Money{"Asha": 80000, "Ravi": 80000}, splitDirectionFriendOwesUser)
	// ₹1,000 drinks I paid, uneven: one friend owes ₹700.
	drinks := seedEventEntry(t, user.ID, "expense", 100000, "2026-11-01")
	seedEntrySplit(t, user.ID, drinks, 100000, map[string]models.Money{"Kabir": 70000}, splitDirectionFriendOwesUser)
	// A cab whose bill says a friend paid ₹600 and I owe ₹300: nothing owed back.
	cab := seedEventEntry(t, user.ID, "expense", 30000, "2026-11-02")
	seedEntrySplit(t, user.ID, cab, 60000, map[string]models.Money{"Meera": 30000}, splitDirectionUserOwesFriend)
	// The entry was edited down to ₹500 but its bill still says ₹2,000 with
	// ₹1,800 owed: what is owed back stops at the entry's amount.
	cake := seedEventEntry(t, user.ID, "expense", 50000, "2026-11-01")
	seedEntrySplit(t, user.ID, cake, 200000, map[string]models.Money{"Zoya": 180000}, splitDirectionFriendOwesUser)
	// Same day, not in the event: must not count.
	seedEventEntry(t, user.ID, "expense", 99900, "2026-11-01")

	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(event.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{snacks.ID, dinner.ID, drinks.ID, cab.ID, cake.ID}}, http.StatusOK)

	got := performJSONRequest[EventResponse](t, router, http.MethodGet, eventPath(event.Event.ID, ""), token, nil, http.StatusOK)
	summary := got.Summary
	wantPaid := models.Money(120000 + 240000 + 100000 + 30000 + 50000)
	wantOwed := models.Money(160000 + 70000 + 0 + 50000)
	if summary.EntryCount != 5 || summary.TotalPaid != wantPaid {
		t.Fatalf("count/paid = %d/%v, want 5/%v", summary.EntryCount, summary.TotalPaid, wantPaid)
	}
	if summary.OwedBack != wantOwed {
		t.Fatalf("owed back = %v, want %v", summary.OwedBack, wantOwed)
	}
	if summary.NetCost != wantPaid-wantOwed {
		t.Fatalf("net cost = %v, want %v", summary.NetCost, wantPaid-wantOwed)
	}
	if summary.SplitEntryCount != 4 {
		t.Fatalf("split entries = %d, want 4", summary.SplitEntryCount)
	}
	if summary.FirstDate == nil || *summary.FirstDate != "2026-10-31" || *summary.LastDate != "2026-11-02" {
		t.Fatalf("dates = %v..%v", summary.FirstDate, summary.LastDate)
	}
	if summary.BudgetLeft == nil || *summary.BudgetLeft != models.Money(1000000)-(wantPaid-wantOwed) {
		t.Fatalf("budget left = %v", summary.BudgetLeft)
	}

	// The events list shows the same cost as the event page.
	createTestEvent(t, router, token, map[string]any{"name": "Empty"})
	list := performJSONRequest[[]EventListItem](t, router, http.MethodGet, "/v1/events", token, nil, http.StatusOK)
	byName := map[string]EventListItem{}
	for _, item := range list {
		byName[item.Name] = item
	}
	party := byName["Party"]
	if party.EntryCount != 5 || party.TotalPaid != wantPaid || party.OwedBack != wantOwed || party.NetCost != wantPaid-wantOwed {
		t.Fatalf("list item = %+v, want count 5, paid %v, owed %v", party, wantPaid, wantOwed)
	}
	if empty := byName["Empty"]; empty.EntryCount != 0 || empty.NetCost != 0 {
		t.Fatalf("empty event = %+v", empty)
	}
}

func TestEntryFiltersReportAndExportFollowTheEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	event := createTestEvent(t, router, token, map[string]any{"name": "Diwali, party"})
	inside := seedEventEntry(t, user.ID, "expense", 150000, "2026-11-01")
	seedEventEntry(t, user.ID, "expense", 99900, "2026-11-01")
	performJSONRequest[map[string]any](t, router, http.MethodPost, eventPath(event.Event.ID, "/entries"), token,
		map[string]any{"entry_ids": []uint{inside.ID}}, http.StatusOK)

	query := fmt.Sprintf("?event_id=%d", event.Event.ID)
	list := performJSONRequest[struct {
		Entries []models.Entry `json:"entries"`
		Total   int64          `json:"total"`
	}](t, router, http.MethodGet, "/v1/entries"+query, token, nil, http.StatusOK)
	if list.Total != 1 || len(list.Entries) != 1 || list.Entries[0].ID != inside.ID {
		t.Fatalf("list should hold only the event's entry, got %#v", list)
	}
	report := performJSONRequest[TransactionReportResponse](t, router, http.MethodGet,
		"/v1/reports/transactions/summary"+query, token, nil, http.StatusOK)
	if report.Summary.TotalExpense != 1500 || report.Summary.TransactionCount != 1 {
		t.Fatalf("report should total the event only, got %+v", report.Summary)
	}
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/entries?event_id=abc", token, nil, http.StatusUnprocessableEntity)

	request := httptest.NewRequest(http.MethodGet, "/v1/entries/export"+query, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("export status %d: %s", response.Code, response.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected header plus one row, got %v (%v)", rows, err)
	}
	assertCSVValue(t, rows[0], rows[1], "event", "Diwali, party")
}

func TestDeleteUserDataRemovesEvents(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	event := models.Event{UserID: user.ID, Name: "Party"}
	if err := database.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	entry := seedEventEntry(t, user.ID, "expense", 1000, "2026-11-01")
	database.DB.Model(&entry).Update("event_id", event.ID)

	if _, err := deleteUserData(database.DB, user); err != nil {
		t.Fatalf("delete user data: %v", err)
	}
	var count int64
	database.DB.Model(&models.Event{}).Where("user_id = ?", user.ID).Count(&count)
	if count != 0 {
		t.Fatalf("events survived account deletion: %d", count)
	}
}
