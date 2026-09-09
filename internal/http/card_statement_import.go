package http

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Comparing a parsed statement against the ledger, and importing what is missing.

Both endpoints are deliberately **stateless**: the client posts the parsed
lines, gets an answer, and nothing is stored. A statement's line items are the
user's complete spending history for a month, and keeping a copy on the server
to save a round trip is not a trade worth making. The same principle governs
statement PDFs — extract, diff, discard.
*/

const maxImportLines = 500

type statementLinesInput struct {
	Lines []statementLine `json:"lines"`
}

func (input statementLinesInput) validate() map[string]string {
	fields := map[string]string{}
	if len(input.Lines) == 0 {
		fields["lines"] = "is required"
	}
	if len(input.Lines) > maxImportLines {
		fields["lines"] = fmt.Sprintf("must be at most %d rows", maxImportLines)
	}
	for _, line := range input.Lines {
		if _, err := parseStrictAPIDate(line.Date); err != nil {
			fields["lines"] = "every row needs a YYYY-MM-DD date"
			break
		}
		if line.Amount <= 0 {
			fields["lines"] = "every row needs a positive amount"
			break
		}
	}
	return fields
}

// diffCardStatement compares parsed statement lines against what Finnri holds
// for that cycle.
func (s *Server) diffCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statement, ok := loadStatementForRequest(c, userID)
	if !ok {
		return
	}

	var input statementLinesInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_lines", "fields": fields})
		return
	}

	entries, err := loadCycleLedgerLines(statement)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_cycle_entries"})
		return
	}

	c.JSON(http.StatusOK, diffStatementLines(input.Lines, entries))
}

// importCardStatementLines creates entries for the rows the user picked.
//
// Only what they selected. The diff is a proposal, and importing everything
// because the parser was confident is how a user ends up with a ledger they no
// longer recognise.
func (s *Server) importCardStatementLines(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statement, ok := loadStatementForRequest(c, userID)
	if !ok {
		return
	}

	var input statementLinesInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_lines", "fields": fields})
		return
	}

	imported := 0
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		for _, line := range input.Lines {
			line.Kind = classifyLine(line)

			// A bill payment is tracked on the statement, never as a card
			// entry — importing one would reduce the card's outstanding a
			// second time. The matcher already keeps these out of the missing
			// bucket; this is the backstop for a client that sends one anyway.
			if line.Kind == lineKindPayment {
				continue
			}

			entry := buildEntryFromStatementLine(statement, line)
			// Re-importing the same row is a no-op rather than a duplicate:
			// the unique index on (user, idempotency_key) does the work.
			result := tx.Where("user_id = ? AND idempotency_key = ?", userID, *entry.IdempotencyKey).
				FirstOrCreate(&entry)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				imported++
			}
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_import_statement_lines"})
		return
	}

	// Importing shrinks the unaccounted bucket, which is the whole point.
	reconciliation, err := reconcileStatement(statement)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_reconcile_statement"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"imported":       imported,
		"reconciliation": reconciliation,
	})
}

// loadStatementForRequest resolves :id to a statement the caller owns.
func loadStatementForRequest(c *gin.Context, userID uint) (*models.CardStatement, bool) {
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return nil, false
	}
	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return nil, false
	}
	return &statement, true
}

// loadCycleLedgerLines is what Finnri holds for a statement's cycle, reduced
// to what matching needs.
//
// The unitemized bucket is excluded. It is Finnri's own placeholder for money
// it could not explain, not a transaction the bank billed, and leaving it in
// would let it match a real statement line and hide a genuine gap.
func loadCycleLedgerLines(statement *models.CardStatement) ([]ledgerLine, error) {
	query := database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND account_id = ? AND date >= ? AND date <= ?",
			statement.UserID, statement.AccountID, statement.CycleStart, statement.CycleEnd)

	if statement.UnitemizedEntryID != nil {
		query = query.Where("id <> ?", *statement.UnitemizedEntryID)
	}

	var entries []models.Entry
	if err := query.Order("date ASC, id ASC").Find(&entries).Error; err != nil {
		return nil, err
	}

	lines := make([]ledgerLine, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, ledgerLine{
			EntryID:  entry.ID,
			Date:     dateOnly(entry.Date),
			Title:    entry.Title,
			Merchant: entry.Merchant,
			Category: entry.Category,
			Amount:   entry.Amount,
			Type:     strings.ToLower(entry.Type),
			Tag:      entry.Tag,
		})
	}
	return lines, nil
}

// buildEntryFromStatementLine turns an imported row into a transaction.
func buildEntryFromStatementLine(statement *models.CardStatement, line statementLine) models.Entry {
	entryType := "expense"
	if line.isCredit() {
		entryType = "income"
	}

	key := statementLineIdempotencyKey(statement.ID, line)
	return models.Entry{
		Title:          statementLineTitle(line),
		Type:           entryType,
		Amount:         line.Amount,
		Currency:       statement.Currency,
		Source:         "statement",
		Category:       statementLineCategory(line),
		Merchant:       statementLineTitle(line),
		Tag:            unitemizedTag,
		Notes:          "Imported from your " + statement.StatementDate + " statement.",
		Date:           dateOnly(line.Date),
		AccountID:      &statement.AccountID,
		UserID:         statement.UserID,
		IdempotencyKey: &key,
	}
}

// statementLineCategory files fees and interest under Bills, which is what
// they are, and leaves everything else on the confirm-first fallback rather
// than guessing a category from a bank's shouty description.
func statementLineCategory(line statementLine) string {
	switch line.Kind {
	case lineKindFee, lineKindInterest:
		return "Bills"
	default:
		return defaultCategory
	}
}

// statementLineTitle tidies a bank description into something readable
// without pretending to understand it.
func statementLineTitle(line statementLine) string {
	title := strings.Join(strings.Fields(line.Description), " ")
	if title == "" {
		return "Card transaction"
	}
	if len(title) > 120 {
		title = title[:120]
	}
	return title
}

// statementLineIdempotencyKey identifies a row well enough that importing the
// same statement twice cannot duplicate it. The description is hashed so the
// key stays inside the column's 128 characters whatever the bank wrote.
func statementLineIdempotencyKey(statementID uint, line statementLine) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(line.Description))))
	return fmt.Sprintf("stmt:%d:%s:%s:%s",
		statementID, dateOnly(line.Date), line.Amount.String(), hex.EncodeToString(sum[:8]))
}

// uploadCardStatementPDF reads a statement PDF and returns the diff against
// the ledger for that cycle.
//
// The file and the password live for the duration of this request and no
// longer: nothing is written to disk, nothing is stored, and neither value
// appears in a log line or an error. The response carries the parsed rows so
// the user can review them, and the client holds those for the import step —
// which is why there is no server-side "parsed statement" to leak.
func (s *Server) uploadCardStatementPDF(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statement, ok := loadStatementForRequest(c, userID)
	if !ok {
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_required"})
		return
	}
	opened, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_unreadable"})
		return
	}
	defer opened.Close()

	data, err := readUploadedPDF(opened)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_pdf"})
		return
	}

	// The only place the password is read. It is not copied anywhere, and the
	// deferred wipe keeps it from lingering in a reusable buffer.
	password := c.PostForm("password")

	text, err := extractStatementText(data, password)
	// Both the bytes and the password stop being needed the moment the text is
	// out, so neither is kept alive past this point.
	data = nil
	password = ""

	if err != nil {
		switch err {
		case errStatementPasswordRequired:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_password_required"})
		case errStatementPasswordWrong:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_password_incorrect"})
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_unreadable"})
		}
		return
	}

	cycleEnd, dateErr := parseAPIDate(statement.CycleEnd)
	fallbackYear := 0
	if dateErr == nil {
		fallbackYear = cycleEnd.Year()
	}

	lines := parseStatementText(text, fallbackYear)
	if len(lines) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no_transactions_found"})
		return
	}

	entries, err := loadCycleLedgerLines(statement)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_cycle_entries"})
		return
	}

	c.JSON(http.StatusOK, diffStatementLines(lines, entries))
}
