package http

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Events group a user's own expenses by occasion. See models.Event for why
// they are not split groups.

const (
	// Far past anything a list can show usefully; it is there so the table
	// cannot be filled by a script.
	maxEventsPerUser = 500
	// One bulk add or remove. A long trip is a few hundred transactions at
	// most, and the app sends what is selected on screen.
	maxEventEntriesPerCall = 200
	maxEventNameLength     = 60
	maxEventEmojiLength    = 16
	maxEventNotesLength    = 500
)

type eventInput struct {
	Name      string        `json:"name"`
	Emoji     string        `json:"emoji"`
	StartDate *string       `json:"start_date"`
	EndDate   *string       `json:"end_date"`
	Budget    *models.Money `json:"budget"`
	Notes     string        `json:"notes"`
}

type eventEntriesInput struct {
	EntryIDs []uint `json:"entry_ids"`
}

// EventListItem is one event on the events list, with enough of its totals to
// draw a card without opening it. NetCost and OwedBack mean what they do on
// EventSummary, so a card and its event page show the same cost.
type EventListItem struct {
	models.Event
	EntryCount int64        `json:"entry_count"`
	TotalPaid  models.Money `json:"total_paid"`
	OwedBack   models.Money `json:"owed_back"`
	NetCost    models.Money `json:"net_cost"`
}

// EventSummary is what an occasion cost.
//
// TotalPaid is what left the user's pocket. OwedBack is what the split bills
// on those entries assign to other people, where the user paid — what the
// bills say, not what has been settled so far. NetCost is the difference:
// the user's real cost of the occasion.
type EventSummary struct {
	EntryCount      int64         `json:"entry_count"`
	TotalPaid       models.Money  `json:"total_paid"`
	OwedBack        models.Money  `json:"owed_back"`
	NetCost         models.Money  `json:"net_cost"`
	BudgetLeft      *models.Money `json:"budget_left,omitempty"`
	FirstDate       *string       `json:"first_date"`
	LastDate        *string       `json:"last_date"`
	SplitEntryCount int           `json:"split_entry_count"`
}

type EventResponse struct {
	Event   models.Event `json:"event"`
	Summary EventSummary `json:"summary"`
}

// normalize cleans the input in place and returns what is wrong with it.
func (input *eventInput) normalize() map[string]string {
	fields := map[string]string{}

	input.Name = strings.Join(strings.Fields(input.Name), " ")
	if input.Name == "" {
		fields["name"] = "is required"
	} else if utf8.RuneCountInString(input.Name) > maxEventNameLength {
		fields["name"] = "must be at most 60 characters"
	}
	input.Emoji = strings.TrimSpace(input.Emoji)
	if utf8.RuneCountInString(input.Emoji) > maxEventEmojiLength {
		fields["emoji"] = "must be at most 16 characters"
	}
	input.Notes = strings.TrimSpace(input.Notes)
	if utf8.RuneCountInString(input.Notes) > maxEventNotesLength {
		fields["notes"] = "must be at most 500 characters"
	}

	input.StartDate = cleanEventDate(input.StartDate, "start_date", fields)
	input.EndDate = cleanEventDate(input.EndDate, "end_date", fields)
	if input.StartDate != nil && input.EndDate != nil && *input.StartDate > *input.EndDate {
		fields["end_date"] = "must be on or after start_date"
	}
	if input.Budget != nil && !input.Budget.IsPositive() {
		fields["budget"] = "must be greater than zero"
	}
	return fields
}

// cleanEventDate turns an empty date into no date and checks the rest.
func cleanEventDate(value *string, field string, fields map[string]string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", trimmed); err != nil {
		fields[field] = "must use YYYY-MM-DD"
		return nil
	}
	return &trimmed
}

func (input eventInput) apply(event *models.Event) {
	event.Name = input.Name
	event.Emoji = input.Emoji
	event.StartDate = input.StartDate
	event.EndDate = input.EndDate
	event.Budget = input.Budget
	event.Notes = input.Notes
}

// entryFitsEvent says whether an entry can belong to an event: expenses only,
// as with tags, and never a card bill payment, whose purchases are already
// counted where they were made.
func entryFitsEvent(entryType, purposeType string) bool {
	return strings.EqualFold(strings.TrimSpace(entryType), "expense") &&
		!strings.EqualFold(strings.TrimSpace(purposeType), cardPaymentPurposeType)
}

// entryEventMessage is the field error for an entry that cannot have an event.
const entryEventMessage = "can be set only on expenses"

func userOwnsEvent(db *gorm.DB, userID, eventID uint) (bool, error) {
	var count int64
	err := db.Model(&models.Event{}).
		Where("id = ? AND user_id = ?", eventID, userID).
		Count(&count).Error
	return count == 1, err
}

// loadOwnedEvent answers 404 for an event that is missing or somebody else's:
// the two are the same answer, so an id cannot be probed.
func loadOwnedEvent(c *gin.Context, userID uint) (models.Event, bool) {
	var event models.Event
	id, ok := parseUintParam(c, "id")
	if !ok {
		return event, false
	}
	err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "event_not_found"})
		return event, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_event"})
		return event, false
	}
	return event, true
}

func (s *Server) listEvents(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var events []models.Event
	if err := database.DB.Where("user_id = ?", userID).
		Order("created_at desc, id desc").
		Find(&events).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_events"})
		return
	}

	var entries []models.Entry
	if err := database.DB.Select("id", "amount", "event_id").
		Where("user_id = ? AND event_id IS NOT NULL", userID).
		Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_events"})
		return
	}
	var bills []models.SplitBill
	if len(entries) > 0 {
		eventEntryIDs := database.DB.Model(&models.Entry{}).Select("id").
			Where("user_id = ? AND event_id IS NOT NULL", userID)
		if err := database.DB.Preload("Participants").
			Where("user_id = ? AND entry_id IN (?)", userID, eventEntryIDs).
			Find(&bills).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_events"})
			return
		}
	}

	items := make([]EventListItem, len(events))
	totals := make(map[uint]*EventListItem, len(events))
	for index, event := range events {
		items[index] = EventListItem{Event: event}
		totals[event.ID] = &items[index]
	}
	amountByEntry := make(map[uint]models.Money, len(entries))
	eventOfEntry := make(map[uint]uint, len(entries))
	for _, entry := range entries {
		item, ok := totals[*entry.EventID]
		if !ok {
			continue
		}
		amountByEntry[entry.ID] = entry.Amount
		eventOfEntry[entry.ID] = *entry.EventID
		item.EntryCount++
		item.TotalPaid += entry.Amount
	}
	for entryID, owed := range owedBackByEntry(bills, amountByEntry) {
		if item, ok := totals[eventOfEntry[entryID]]; ok {
			item.OwedBack += owed
		}
	}
	for index := range items {
		items[index].NetCost = items[index].TotalPaid - items[index].OwedBack
	}
	c.JSON(http.StatusOK, items)
}

func (s *Server) createEvent(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var input eventInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.normalize(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_event", "fields": fields})
		return
	}

	var count int64
	if err := database.DB.Model(&models.Event{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_event"})
		return
	}
	if count >= maxEventsPerUser {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_event",
			"fields": gin.H{"name": "you have reached the limit of 500 events"},
		})
		return
	}

	event := models.Event{UserID: userID}
	input.apply(&event)
	if err := database.DB.Create(&event).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_event"})
		return
	}
	c.JSON(http.StatusCreated, EventResponse{Event: event, Summary: emptyEventSummary(event)})
}

func (s *Server) getEvent(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	event, ok := loadOwnedEvent(c, userID)
	if !ok {
		return
	}
	summary, err := summarizeEvent(database.DB, userID, event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_event"})
		return
	}
	c.JSON(http.StatusOK, EventResponse{Event: event, Summary: summary})
}

func (s *Server) updateEvent(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	event, ok := loadOwnedEvent(c, userID)
	if !ok {
		return
	}

	var input eventInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.normalize(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_event", "fields": fields})
		return
	}

	input.apply(&event)
	if err := database.DB.Save(&event).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_event"})
		return
	}
	summary, err := summarizeEvent(database.DB, userID, event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_event"})
		return
	}
	c.JSON(http.StatusOK, EventResponse{Event: event, Summary: summary})
}

// deleteEvent removes the event and keeps every transaction that was in it.
//
// The entries are unlinked here rather than left to the foreign key: a key
// GORM created would be NO ACTION, and the delete would fail instead.
func (s *Server) deleteEvent(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	event, ok := loadOwnedEvent(c, userID)
	if !ok {
		return
	}

	var unlinked int64
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.Entry{}).
			Where("user_id = ? AND event_id = ?", userID, event.ID).
			Update("event_id", nil)
		if result.Error != nil {
			return result.Error
		}
		unlinked = result.RowsAffected
		return tx.Where("id = ? AND user_id = ?", event.ID, userID).Delete(&models.Event{}).Error
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_event"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"unlinked": unlinked})
}

// readEventEntryIDs binds and checks a bulk request's ids. Duplicates are
// dropped rather than refused: a selection that names a row twice still means
// that row.
func readEventEntryIDs(c *gin.Context) ([]uint, bool) {
	var input eventEntriesInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return nil, false
	}
	seen := make(map[uint]bool, len(input.EntryIDs))
	ids := make([]uint, 0, len(input.EntryIDs))
	for _, id := range input.EntryIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "invalid_event_entries", "fields": gin.H{"entry_ids": "must name at least one transaction"},
		})
		return nil, false
	}
	if len(ids) > maxEventEntriesPerCall {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "invalid_event_entries", "fields": gin.H{"entry_ids": "must name at most 200 transactions"},
		})
		return nil, false
	}
	return ids, true
}

// addEventEntries puts the named entries in this event. An entry already in
// another event moves, since an entry belongs to one event at most.
//
// All or nothing: if any id is not the user's, or is not an expense, nothing
// changes and the answer names those ids.
func (s *Server) addEventEntries(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	event, ok := loadOwnedEvent(c, userID)
	if !ok {
		return
	}
	ids, ok := readEventEntryIDs(c)
	if !ok {
		return
	}

	var entries []models.Entry
	if err := database.DB.Select("id", "type", "purpose_type", "event_id").
		Where("user_id = ? AND id IN ?", userID, ids).
		Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_event_entries"})
		return
	}
	found := make(map[uint]models.Entry, len(entries))
	for _, entry := range entries {
		found[entry.ID] = entry
	}
	missing := []uint{}
	unfit := []uint{}
	moved := 0
	for _, id := range ids {
		entry, ok := found[id]
		switch {
		case !ok:
			missing = append(missing, id)
		case !entryFitsEvent(entry.Type, entry.PurposeType):
			unfit = append(unfit, id)
		case entry.EventID != nil && *entry.EventID != event.ID:
			moved++
		}
	}
	if len(missing) > 0 || len(unfit) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":       "invalid_event_entries",
			"fields":      gin.H{"entry_ids": "must be your own expenses"},
			"not_found":   missing,
			"not_allowed": unfit,
		})
		return
	}

	result := database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND id IN ?", userID, ids).
		Update("event_id", event.ID)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_event_entries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"added": len(ids), "moved": moved})
}

// removeEventEntries takes the named entries out of this event. Ids that are
// not in it are ignored: the result is the same.
func (s *Server) removeEventEntries(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	event, ok := loadOwnedEvent(c, userID)
	if !ok {
		return
	}
	ids, ok := readEventEntryIDs(c)
	if !ok {
		return
	}

	result := database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND event_id = ? AND id IN ?", userID, event.ID, ids).
		Update("event_id", nil)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_event_entries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": result.RowsAffected})
}

func emptyEventSummary(event models.Event) EventSummary {
	summary := EventSummary{}
	if event.Budget != nil {
		left := *event.Budget
		summary.BudgetLeft = &left
	}
	return summary
}

// summarizeEvent works out what an event cost the user.
//
// Owed-back comes from the split bills the user wrote on these entries, read
// with splitBillSlots — the same walk that draws "you lent" on a bill — so
// the direction of each row is interpreted in one place only. A bill the user
// did not pay owes them nothing back. What a bill assigns to others is capped
// at the entry's amount, so an entry edited below its bill's total can never
// make the occasion cost less than nothing.
func summarizeEvent(db *gorm.DB, userID uint, event models.Event) (EventSummary, error) {
	summary := emptyEventSummary(event)

	var entries []models.Entry
	if err := db.Select("id", "amount", "date").
		Where("user_id = ? AND event_id = ?", userID, event.ID).
		Find(&entries).Error; err != nil {
		return summary, err
	}
	if len(entries) == 0 {
		return summary, nil
	}

	amountByEntry := make(map[uint]models.Money, len(entries))
	entryIDs := make([]uint, 0, len(entries))
	dates := make([]string, 0, len(entries))
	for _, entry := range entries {
		summary.TotalPaid += entry.Amount
		amountByEntry[entry.ID] = entry.Amount
		entryIDs = append(entryIDs, entry.ID)
		dates = append(dates, entry.Date)
	}
	summary.EntryCount = int64(len(entries))
	sort.Strings(dates)
	summary.FirstDate = &dates[0]
	summary.LastDate = &dates[len(dates)-1]

	var bills []models.SplitBill
	if err := db.Preload("Participants").
		Where("user_id = ? AND entry_id IN ?", userID, entryIDs).
		Find(&bills).Error; err != nil {
		return summary, err
	}
	for _, owed := range owedBackByEntry(bills, amountByEntry) {
		summary.OwedBack += owed
		summary.SplitEntryCount++
	}

	summary.NetCost = summary.TotalPaid - summary.OwedBack
	if event.Budget != nil {
		left := *event.Budget - summary.NetCost
		summary.BudgetLeft = &left
	}
	return summary, nil
}

// owedBackByEntry is what the bills assign to other people on each entry they
// hang off, capped at the entry's amount and never below zero. Every entry
// with a bill appears, even when nobody owes anything on it.
func owedBackByEntry(bills []models.SplitBill, amountByEntry map[uint]models.Money) map[uint]models.Money {
	owed := map[uint]models.Money{}
	for _, bill := range bills {
		if bill.EntryID == nil {
			continue
		}
		owed[*bill.EntryID] += splitBillOwedToAuthor(bill)
	}
	for entryID, value := range owed {
		if limit := amountByEntry[entryID]; value > limit {
			value = limit
		}
		if value < 0 {
			value = 0
		}
		owed[entryID] = value
	}
	return owed
}

// splitBillOwedToAuthor is what a bill assigns to people other than its
// author, when the author paid it; zero when somebody else paid.
func splitBillOwedToAuthor(bill models.SplitBill) models.Money {
	slotOfFriend := make(map[uint]string, len(bill.Participants))
	for _, participant := range bill.Participants {
		slotOfFriend[participant.FriendID] = splitGroupSlotForFriend(participant.FriendID)
	}
	totals := splitBillSlots(bill, models.SplitBillViewerSelfSlot, slotOfFriend)
	if totals.payerSlot != models.SplitBillViewerSelfSlot {
		return 0
	}
	owed := bill.TotalAmount - totals.share[models.SplitBillViewerSelfSlot]
	if owed < 0 {
		return 0
	}
	return owed
}

// entryEventProblem checks an event named on an entry save. It returns the
// field message to show, or "" when the event can be set.
func entryEventProblem(userID, eventID uint, entryType, purposeType string) (string, error) {
	if eventID == 0 {
		return "must be a positive integer", nil
	}
	if !entryFitsEvent(entryType, purposeType) {
		return entryEventMessage, nil
	}
	owned, err := userOwnsEvent(database.DB, userID, eventID)
	if err != nil {
		return "", err
	}
	if !owned {
		return "must belong to the current user", nil
	}
	return "", nil
}
