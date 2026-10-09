package http

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
When the bank moves a card's statement date.

`statement_day` is learned from the first bill and then trusted. Banks do move
it — permanently, or for one cycle around a holiday — and a single statement
cannot tell those apart. So nothing here re-learns the day silently:

  - A cycle starts the day after the card's previous *actual* statement when
    there is one, not after where the anchor day says it should have been.
    That is what keeps consecutive cycles tiling with no gap and no overlap
    across a move. With no neighbour, the anchor rule in statementCycle stands.
  - A statement's date can be corrected in place (PATCH), so fixing a date
    never leaves a second bill behind.
  - Saving the card's latest statement on a day that is not the card's day
    returns a suggestion. The user decides whether the card moved; accepting
    is an ordinary account update, declining changes nothing.
  - One cycle is one statement. A draft the reminder job opened, or a bill
    already saved, within a fortnight of a date counts as that cycle's
    statement, so a moved date never grows a duplicate beside it.
*/

const (
	// sameCycleWindowDays is how far apart two statement dates can be and
	// still be the same cycle. Monthly statements are at least 28 days apart,
	// so a fortnight either side never reaches a neighbouring cycle.
	sameCycleWindowDays = 14
	// neighbourLookbackDays bounds how far back the previous statement may be
	// and still anchor this cycle's start. Further back means a missing month
	// in between, and a two-month window would be wrong — fall back instead.
	neighbourLookbackDays = 45
)

// statementDaySuggestion is the offer to move the card's billing day to the
// day this statement was actually dated.
type statementDaySuggestion struct {
	CurrentDay  int `json:"current_day"`
	ObservedDay int `json:"observed_day"`
}

// savedStatementResponse is a statement write's response: the statement, and
// the suggestion when its date disagrees with the card.
type savedStatementResponse struct {
	cardStatementResponse
	StatementDaySuggestion *statementDaySuggestion `json:"statement_day_suggestion,omitempty"`
}

// previousStatement is the card's latest statement dated before `date`,
// excluding `excludeID` (the statement being edited). Zero ID means none.
func previousStatement(userID, accountID, excludeID uint, date string) (models.CardStatement, error) {
	var previous models.CardStatement
	err := database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date < ? AND id <> ?", userID, accountID, date, excludeID).
		Order("statement_date DESC").
		Limit(1).
		Find(&previous).Error
	return previous, err
}

// nextStatement is the card's earliest statement dated after `date`.
func nextStatement(userID, accountID, excludeID uint, date string) (models.CardStatement, error) {
	var next models.CardStatement
	err := database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date > ? AND id <> ?", userID, accountID, date, excludeID).
		Order("statement_date ASC").
		Limit(1).
		Find(&next).Error
	return next, err
}

// cardStatementCycle is statementCycle, anchored on the card's real previous
// statement when one is close enough to be the cycle before this one.
func cardStatementCycle(userID, accountID, excludeID uint, statementDate time.Time, statementDay int) (time.Time, time.Time, error) {
	start, end := statementCycle(statementDate, statementDay)

	previous, err := previousStatement(userID, accountID, excludeID, statementDate.Format(apiDateLayout))
	if err != nil {
		return start, end, err
	}
	if previous.ID == 0 {
		return start, end, nil
	}
	previousDate, err := parseAPIDate(previous.StatementDate)
	if err != nil {
		return start, end, nil
	}
	if previousDate.Before(end.AddDate(0, 0, -neighbourLookbackDays)) {
		return start, end, nil
	}
	return previousDate.AddDate(0, 0, 1), end, nil
}

// retileNextStatement re-derives the cycle of the statement that follows
// `date`, so it starts the day after this one. Called after a statement is
// created or its date corrected. A failure is logged, not returned: the write
// the user asked for has already succeeded, and the next save of that
// statement re-derives the same window.
func retileNextStatement(account models.Account, excludeID uint, date string) {
	next, err := nextStatement(account.UserID, account.ID, excludeID, date)
	if err != nil || next.ID == 0 {
		return
	}
	nextDate, err := parseAPIDate(next.StatementDate)
	if err != nil {
		return
	}
	start, end, err := cardStatementCycle(account.UserID, account.ID, next.ID, nextDate, account.StatementDay)
	if err != nil {
		log.Printf("card statement: could not retile statement %d: %v", next.ID, err)
		return
	}
	newStart, newEnd := start.Format(apiDateLayout), end.Format(apiDateLayout)
	if newStart == next.CycleStart && newEnd == next.CycleEnd {
		return
	}
	if err := database.DB.Model(&next).Updates(map[string]any{"cycle_start": newStart, "cycle_end": newEnd}).Error; err != nil {
		log.Printf("card statement: could not retile statement %d: %v", next.ID, err)
		return
	}
	// The window moved, so what the ledger explains for that bill moved too.
	if _, err := reconcileStatement(&next); err != nil {
		log.Printf("card statement: could not reconcile retiled statement %d: %v", next.ID, err)
	}
}

// statementInSameCycle finds a statement on this card within a fortnight of
// `date`. With draftsOnly it considers reminder-job placeholders only.
func statementInSameCycle(userID, accountID uint, date time.Time, draftsOnly bool) (models.CardStatement, error) {
	from := date.AddDate(0, 0, -sameCycleWindowDays).Format(apiDateLayout)
	to := date.AddDate(0, 0, sameCycleWindowDays).Format(apiDateLayout)
	query := database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date BETWEEN ? AND ?", userID, accountID, from, to)
	if draftsOnly {
		query = query.Where("status = ?", statementStatusDraft)
	}
	var found models.CardStatement
	err := query.Order("statement_date ASC").Limit(1).Find(&found).Error
	return found, err
}

// suggestStatementDay offers to move the card's day when this statement — the
// card's latest — is dated on a different one. A card billing on the 31st
// that bills on the 30th in November has not moved, so the comparison is
// against the clamped anchor, not the raw day. Correcting an older statement
// says nothing about where the card bills now, so it never suggests.
func suggestStatementDay(account models.Account, statement models.CardStatement) *statementDaySuggestion {
	if account.StatementDay < 1 || account.StatementDay > 31 || statement.Status == statementStatusDraft {
		return nil
	}
	date, err := parseAPIDate(statement.StatementDate)
	if err != nil {
		return nil
	}
	if clampDayToMonth(date.Year(), date.Month(), account.StatementDay).Equal(truncateDate(date)) {
		return nil
	}
	later, err := nextStatement(account.UserID, account.ID, statement.ID, statement.StatementDate)
	if err != nil || later.ID != 0 {
		return nil
	}
	return &statementDaySuggestion{CurrentDay: account.StatementDay, ObservedDay: date.Day()}
}

// updateCardStatement corrects one statement in place — its date included.
//
// Saving with a new date through POST would upsert on the new date and leave
// the old bill behind; this moves the one that exists. The new date may not
// collide with another of the card's statements, and may not jump past a
// neighbouring one: that would reorder the card's history and is never a
// correction.
func (s *Server) updateCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return
	}
	account, err := loadUserCard(userID, statement.AccountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}

	var input cardStatementInput
	if err := c.ShouldBindBodyWith(&input, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	// The edit sheet sends the four figures it shows. Notes it does not show
	// must survive the edit rather than be blanked, and the statement keeps
	// its own source; only a field the client actually sends replaces them.
	var sent struct {
		Notes  *string `json:"notes"`
		Source *string `json:"source"`
	}
	if err := c.ShouldBindBodyWith(&sent, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if sent.Notes == nil {
		input.Notes = statement.Notes
	}
	if sent.Source == nil || *sent.Source == "" {
		input.Source = statement.Source
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement", "fields": fields})
		return
	}

	statementDate, _ := parseStrictAPIDate(input.StatementDate)
	newDate := statementDate.Format(apiDateLayout)
	oldDate := statement.StatementDate

	if newDate != oldDate {
		var clash int64
		if err := database.DB.Model(&models.CardStatement{}).
			Where("user_id = ? AND account_id = ? AND statement_date = ? AND id <> ?", userID, account.ID, newDate, statement.ID).
			Count(&clash).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
			return
		}
		if clash > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "statement_date_taken"})
			return
		}
		previous, prevErr := previousStatement(userID, account.ID, statement.ID, oldDate)
		next, nextErr := nextStatement(userID, account.ID, statement.ID, oldDate)
		if prevErr != nil || nextErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
			return
		}
		if (previous.ID != 0 && newDate <= previous.StatementDate) || (next.ID != 0 && newDate >= next.StatementDate) {
			c.JSON(http.StatusConflict, gin.H{"error": "statement_date_out_of_order"})
			return
		}
	}

	cycleStart, cycleEnd, err := cardStatementCycle(userID, account.ID, statement.ID, statementDate, account.StatementDay)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
		return
	}
	dueDate := input.DueDate
	if dueDate == "" {
		dueDate = dueDateFor(statementDate, effectiveDueDay(account, statementDate)).Format(apiDateLayout)
	}

	statement.StatementDate = newDate
	statement.CycleStart = cycleStart.Format(apiDateLayout)
	statement.CycleEnd = cycleEnd.Format(apiDateLayout)
	statement.DueDate = dueDate
	statement.TotalDue = input.TotalDue
	statement.MinimumDue = input.MinimumDue
	statement.Currency = normalizedStatementCurrency(input.Currency)
	statement.Source = normalizedStatementSource(input.Source)
	statement.Notes = input.Notes
	// Same rule as saving: an amount turns a draft into a bill.
	if statement.Status == "" || statement.Status == statementStatusDraft {
		statement.Status = statementStatusUnpaid
	}
	statement.Status = deriveStatementStatus(statement.Status, statement.TotalDue, statement.PaidAmount)

	if err := database.DB.Save(&statement).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			c.JSON(http.StatusConflict, gin.H{"error": "statement_date_taken"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
		return
	}
	retileNextStatement(account, statement.ID, newDate)

	respondWithSavedStatement(c, &statement, false, suggestStatementDay(account, statement))
}

// respondWithSavedStatement is respondWithStatement plus the suggestion.
func respondWithSavedStatement(c *gin.Context, statement *models.CardStatement, created bool, suggestion *statementDaySuggestion) {
	response, ok := buildStatementResponse(c, statement)
	if !ok {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, savedStatementResponse{cardStatementResponse: response, StatementDaySuggestion: suggestion})
}
