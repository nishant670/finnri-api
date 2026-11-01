package models

import (
	"time"

	"gorm.io/gorm"
)

// Event groups a user's own transactions by occasion — a party they hosted, a
// trip, a wedding — so they can see later what it cost them.
//
// It is not a split group. A group is shared and answers who owes whom; an
// event is private and answers what the occasion cost, split or not. An entry
// belongs to at most one event (Entry.EventID), so event totals never count
// the same money twice.
//
// StartDate and EndDate are for display and suggestions only: supplies bought
// a week early still belong to the party.
type Event struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index;not null" json:"user_id"`
	Name      string    `gorm:"type:varchar(60);not null" json:"name"`
	Emoji     string    `gorm:"type:varchar(16);not null;default:''" json:"emoji"`
	StartDate *string   `gorm:"type:date" json:"start_date"`
	EndDate   *string   `gorm:"type:date" json:"end_date"`
	Budget    *Money    `gorm:"type:numeric(19,2)" json:"budget"`
	Notes     string    `gorm:"not null;default:''" json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// See Entry.AfterFind: a DATE comes back as a timestamp in both dialects.
func (event *Event) AfterFind(_ *gorm.DB) error {
	if event.StartDate != nil {
		day := CalendarDay(*event.StartDate)
		event.StartDate = &day
	}
	if event.EndDate != nil {
		day := CalendarDay(*event.EndDate)
		event.EndDate = &day
	}
	return nil
}
