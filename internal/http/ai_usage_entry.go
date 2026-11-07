package http

import (
	"strings"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// maxAIRequestIDLength matches ai_usage_events.request_id.
const maxAIRequestIDLength = 120

// linkAIUsageToEntry records which entry an AI read made (P57), so AI usage
// can open it. The app sends back the ai_request_id its parse response carried
// when it saves the draft. Only a read by the same subject — the signed-in user
// or this guest device — that has no entry yet is linked; anything else is
// ignored rather than refused, because a missing link costs a tap later and a
// refused save costs the entry.
func linkAIUsageToEntry(c *gin.Context, requestID string, entryID uint) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > maxAIRequestIDLength || entryID == 0 {
		return
	}
	subject, ok := parseCreditSubject(c)
	if !ok {
		return
	}
	_ = scopeSubject(database.DB.Model(&models.AIUsageEvent{}), subject).
		Where("request_id = ? AND entry_id IS NULL", requestID).
		Update("entry_id", entryID).Error
}

// aiUsageEntry is the entry an AI usage row made, as the list shows it.
type aiUsageEntry struct {
	ID     uint         `json:"id"`
	Title  string       `json:"title"`
	Amount models.Money `json:"amount"`
	Type   string       `json:"type"`
	Date   string       `json:"date"`
}

// aiUsageEntries loads the entries a page of usage rows made, keyed by id. An
// entry deleted since is simply absent: the row says so instead of opening it.
func aiUsageEntries(userID uint, events []models.AIUsageEvent) map[uint]aiUsageEntry {
	ids := make([]uint, 0, len(events))
	for _, event := range events {
		if event.EntryID != nil {
			ids = append(ids, *event.EntryID)
		}
	}
	found := map[uint]aiUsageEntry{}
	if len(ids) == 0 || userID == 0 {
		return found
	}
	var entries []models.Entry
	if err := database.DB.Select("id", "title", "amount", "type", "date").
		Where("user_id = ? AND id IN ?", userID, ids).
		Find(&entries).Error; err != nil {
		return found
	}
	for _, entry := range entries {
		found[entry.ID] = aiUsageEntry{
			ID:     entry.ID,
			Title:  entry.Title,
			Amount: entry.Amount,
			Type:   entry.Type,
			Date:   entry.Date,
		}
	}
	return found
}
