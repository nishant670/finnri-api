package http

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	timepkg "time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

func filteredEntriesQuery(userID uint, c *gin.Context) (*gorm.DB, map[string]string) {
	return filteredEntriesQueryOmitting(userID, c, false)
}

// filteredEntriesQueryOmitting builds the same query with one predicate
// optionally left out.
//
// Facet counts need it: a chip reading "Bills 15" is answering "how many would I
// get if I picked Bills", so it has to be counted against every filter *except*
// the category one. Counting inside the category filter would make the selected
// chip show its own count and every other chip show zero.
func filteredEntriesQueryOmitting(
	userID uint,
	c *gin.Context,
	omitCategory bool,
) (*gorm.DB, map[string]string) {
	return buildFilteredEntriesQuery(userID, c.Request.URL.Query(), omitCategory)
}

// buildFilteredEntriesQuery is where every predicate actually lives.
//
// It takes url.Values rather than the request because the parse channel's
// question path is not a request for a list — it is a request for a *number*
// about one — and the number has to be computed over exactly the rows the list
// would show. Answering "₹4,820 on food this month" from one set of predicates
// and then opening a list built from another is the specific failure mode M3
// exists to avoid: a card and the transactions behind it disagreeing. So the
// question resolves itself into the same query parameters the list takes, and
// both sides run them through this function.
//
// c.Query reads from the URL query string and nothing else, so routing the gin
// path through here is an exact equivalence rather than an approximation.
func buildFilteredEntriesQuery(
	userID uint,
	values url.Values,
	omitCategory bool,
) (*gorm.DB, map[string]string) {
	get := values.Get
	fields := map[string]string{}
	query := database.DB.Model(&models.Entry{}).Where("entries.user_id = ?", userID)

	if t := strings.TrimSpace(get("type")); t != "" && !strings.EqualFold(t, "all") {
		if !strings.EqualFold(t, "expense") && !strings.EqualFold(t, "income") {
			fields["type"] = "must be expense or income"
		} else {
			query = query.Where("LOWER(entries.type) = LOWER(?)", t)
		}
	}

	if cat := strings.TrimSpace(get("category")); cat != "" && !omitCategory {
		query = query.Where("LOWER(entries.category) = LOWER(?)", cat)
	}

	// The cleanup filter behind the app's "Uncategorised" preset.
	//
	// Misc is deliberately not included. It is the confirm-first fallback the
	// parser files things under when it cannot place them, and it is also what a
	// manual entry defaults to — a real category people budget against, not an
	// empty one. This matches needsTransactionReview in the app rather than
	// inventing a second meaning for the same word.
	if uncategorised := strings.TrimSpace(get("uncategorised")); uncategorised != "" {
		switch uncategorised {
		case "1", "true":
			query = query.Where(
				"entries.category IS NULL OR TRIM(entries.category) = '' OR LOWER(entries.category) = ?",
				"uncategorized",
			)
		default:
			fields["uncategorised"] = "must be 1 or true when present"
		}
	}

	if mode := strings.TrimSpace(get("mode")); mode != "" {
		// Resolved rather than matched against a second list, so whatever may be
		// saved may be searched for — and an alias like "bank" finds the
		// "Bank Account" rows it means.
		if resolved, ok := canonicalMode(mode); ok {
			query = query.Where("LOWER(entries.mode) = LOWER(?)", resolved)
		} else {
			fields["mode"] = modeMessage()
		}
	}
	if accountID := strings.TrimSpace(get("account_id")); accountID != "" {
		parsed, err := strconv.ParseUint(accountID, 10, 32)
		if err != nil || parsed == 0 {
			fields["account_id"] = "must be a positive integer"
		} else {
			query = query.Where("entries.account_id = ?", parsed)
		}
	}

	var minAmount, maxAmount *models.Money
	if minStr := get("min_amount"); minStr != "" {
		min, err := models.ParseMoney(minStr)
		if err != nil {
			fields["min_amount"] = err.Error()
		} else {
			minAmount = &min
			query = query.Where("entries.amount >= ?", min)
		}
	}

	if maxStr := get("max_amount"); maxStr != "" {
		max, err := models.ParseMoney(maxStr)
		if err != nil {
			fields["max_amount"] = err.Error()
		} else {
			maxAmount = &max
			query = query.Where("entries.amount <= ?", max)
		}
	}
	if minAmount != nil && maxAmount != nil && *minAmount > *maxAmount {
		fields["max_amount"] = "must be greater than or equal to min_amount"
	}

	startDate := get("start_date")
	if startDate != "" {
		if _, err := timepkg.Parse("2006-01-02", startDate); err != nil {
			fields["start_date"] = "must use YYYY-MM-DD"
		} else {
			query = query.Where("entries.date >= ?", startDate)
		}
	}

	endDate := get("end_date")
	if endDate != "" {
		if _, err := timepkg.Parse("2006-01-02", endDate); err != nil {
			fields["end_date"] = "must use YYYY-MM-DD"
		} else {
			query = query.Where("entries.date <= ?", endDate)
		}
	}
	if startDate != "" && endDate != "" && startDate > endDate {
		fields["end_date"] = "must be on or after start_date"
	}

	if tag := strings.TrimSpace(get("tag")); tag != "" {
		query = applyEntryTagFilter(query, tag)
	}
	if search := strings.TrimSpace(get("q")); search != "" {
		if len(search) > 200 {
			fields["q"] = "must not exceed 200 characters"
		} else {
			pattern := "%" + strings.ToLower(search) + "%"
			query = query.Where(
				"LOWER(entries.title) LIKE ? OR LOWER(entries.merchant) LIKE ? OR LOWER(entries.notes) LIKE ?",
				pattern, pattern, pattern,
			)
		}
	}

	if len(fields) > 0 {
		return query, fields
	}
	return query, nil
}

// entrySortOrders maps the sort the client asks for onto SQL.
//
// created_at is the tie-break on every one of them: date has day resolution, so
// several entries share it routinely, and without a second key their relative
// order is whatever the planner felt like — which makes pagination drop and
// repeat rows between pages.
var entrySortOrders = map[string]string{
	"newest":  "entries.date desc, entries.created_at desc",
	"oldest":  "entries.date asc, entries.created_at asc",
	"highest": "entries.amount desc, entries.created_at desc",
	"lowest":  "entries.amount asc, entries.created_at desc",
}

const defaultEntrySort = "newest"

// parseEntrySort resolves the sort parameter, reporting an invalid one rather
// than silently falling back — a list quietly ordered by something other than
// what was asked for is indistinguishable from a bug in the data.
func parseEntrySort(value string) (string, map[string]string) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return entrySortOrders[defaultEntrySort], nil
	}
	if order, ok := entrySortOrders[normalized]; ok {
		return order, nil
	}
	return "", map[string]string{"sort": "must be newest, oldest, highest, or lowest"}
}

func applyEntryTagFilter(query *gorm.DB, tag string) *gorm.DB {
	tagFilter, err := json.Marshal([]string{tag})
	if err != nil {
		return query
	}
	if database.DB.Dialector.Name() == "sqlite" {
		return query.Where("entries.tags LIKE ?", "%\""+strings.ReplaceAll(tag, `"`, `\"`)+"\"%")
	}
	return query.Where("entries.tags @> ?", string(tagFilter))
}
