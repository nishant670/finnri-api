package http

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/models"
)

const maxEntryExportRows = 10000

var entryExportCSVHeader = []string{
	"id",
	"date",
	"time",
	"type",
	"amount",
	"currency",
	"title",
	"category",
	"merchant",
	"account_id",
	"account_name",
	"mode",
	"source",
	"notes",
	"tags",
	"created_at",
	"updated_at",
}

func (s *Server) exportEntriesCSV(c *gin.Context) {
	val, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(uint)

	format := strings.TrimSpace(strings.ToLower(c.DefaultQuery("format", "csv")))
	if format != "csv" && format != "pdf" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_filters",
			"fields": gin.H{"format": "must be csv or pdf"},
		})
		return
	}

	query, fields := filteredEntriesQuery(userID, c)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": fields})
		return
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_entries"})
		return
	}
	if total > maxEntryExportRows {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "export_too_large",
			"fields": gin.H{"filters": fmt.Sprintf("export is limited to %d rows; narrow the date range or filters", maxEntryExportRows)},
		})
		return
	}

	var entries []models.Entry
	if err := query.Session(&gorm.Session{}).
		Preload("Account").
		Order("date desc, created_at desc").
		Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_export_entries"})
		return
	}

	if format == "pdf" {
		// The statement claims the range the filters actually described, not a
		// month the server chose — the two formats come off one query, so
		// anything else would be a document disagreeing with the CSV beside it.
		location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
		pdf := buildStatementPDF(
			entries,
			statementPeriodLabel(c.Query("start_date"), c.Query("end_date")),
			time.Now().In(location),
		)
		c.Header("Content-Type", "application/pdf")
		c.Header("Content-Disposition", `attachment; filename="finnri-statement.pdf"`)
		c.Data(http.StatusOK, "application/pdf", pdf)
		return
	}

	body, err := entriesCSV(entries)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_export_entries"})
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="finnri-entries.csv"`)
	c.String(http.StatusOK, body)
}

/*
statementPeriodLabel says what the document covers, in the words the filters
gave it.

An unbounded export is a real thing to ask for — "everything I have ever
logged" — and it has to be labelled as that rather than as a month that was
never requested.
*/
func statementPeriodLabel(startDate, endDate string) string {
	start := strings.TrimSpace(startDate)
	end := strings.TrimSpace(endDate)
	switch {
	case start == "" && end == "":
		return "All transactions"
	case start == "":
		return "Up to " + prettyStatementDate(end)
	case end == "":
		return "From " + prettyStatementDate(start)
	case start == end:
		return prettyStatementDate(start)
	}
	return prettyStatementDate(start) + " to " + prettyStatementDate(end)
}

func prettyStatementDate(value string) string {
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return value
	}
	return parsed.Format("2 Jan 2006")
}

func entriesCSV(entries []models.Entry) (string, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(entryExportCSVHeader); err != nil {
		return "", err
	}
	for _, entry := range entries {
		accountID := ""
		accountName := ""
		if entry.AccountID != nil {
			accountID = fmt.Sprintf("%d", *entry.AccountID)
		}
		if entry.Account != nil {
			accountName = strings.TrimSpace(entry.Account.Name)
		}
		if err := writer.Write([]string{
			fmt.Sprintf("%d", entry.ID),
			entry.Date,
			entry.Time,
			entry.Type,
			entry.Amount.String(),
			entry.Currency,
			entry.Title,
			entry.Category,
			entry.Merchant,
			accountID,
			accountName,
			entry.Mode,
			entry.Source,
			entry.Notes,
			strings.Join(entry.Tags, "|"),
			entry.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			entry.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}); err != nil {
			return "", err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}
