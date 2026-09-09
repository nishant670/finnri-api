package http

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	defaultMerchantSuggestionLimit = 10
	maxMerchantSuggestionLimit     = 25
	merchantSuggestionScanLimit    = 500
)

type MerchantSuggestionsResponse struct {
	Suggestions []MerchantSuggestion `json:"suggestions"`
}

type MerchantSuggestion struct {
	Merchant         string `json:"merchant"`
	Category         string `json:"category"`
	TransactionCount int    `json:"transaction_count"`
	LastSeenDate     string `json:"last_seen_date"`
}

type merchantSuggestionGroup struct {
	merchant         string
	key              string
	categoryCounts   map[string]int
	categoryLastSeen map[string]string
	transactionCount int
	lastSeenDate     string
	prefixMatch      bool
}

func (s *Server) listMerchantSuggestions(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	search := strings.TrimSpace(c.Query("q"))
	limit, fields := parseMerchantSuggestionLimit(c.DefaultQuery("limit", strconv.Itoa(defaultMerchantSuggestionLimit)))
	if len(search) > 120 {
		fields["q"] = "must not exceed 120 characters"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": fields})
		return
	}

	var entries []models.Entry
	query := database.DB.
		Select("merchant, category, date, created_at").
		Where("user_id = ? AND TRIM(merchant) <> ?", userID, "")
	if search != "" {
		query = query.Where("LOWER(merchant) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	if err := query.
		Order("date DESC, created_at DESC").
		Limit(merchantSuggestionScanLimit).
		Find(&entries).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_merchant_suggestions"})
		return
	}

	c.JSON(http.StatusOK, MerchantSuggestionsResponse{
		Suggestions: buildMerchantSuggestions(entries, search, limit),
	})
}

func parseMerchantSuggestionLimit(raw string) (int, map[string]string) {
	fields := map[string]string{}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit < 1 {
		fields["limit"] = "must be a positive integer"
		return defaultMerchantSuggestionLimit, fields
	}
	if limit > maxMerchantSuggestionLimit {
		fields["limit"] = "must be 25 or less"
		return defaultMerchantSuggestionLimit, fields
	}
	return limit, fields
}

func buildMerchantSuggestions(entries []models.Entry, search string, limit int) []MerchantSuggestion {
	searchKey := strings.ToLower(strings.TrimSpace(search))
	groups := map[string]*merchantSuggestionGroup{}

	for _, entry := range entries {
		merchant := strings.TrimSpace(entry.Merchant)
		if merchant == "" {
			continue
		}
		key := strings.ToLower(merchant)
		group := groups[key]
		if group == nil {
			group = &merchantSuggestionGroup{
				merchant:         merchant,
				key:              key,
				categoryCounts:   map[string]int{},
				categoryLastSeen: map[string]string{},
				prefixMatch:      searchKey == "" || strings.HasPrefix(key, searchKey),
			}
			groups[key] = group
		}

		if entry.Date > group.lastSeenDate {
			group.lastSeenDate = entry.Date
			group.merchant = merchant
		}
		group.transactionCount++

		category := strings.TrimSpace(entry.Category)
		if category != "" {
			group.categoryCounts[category]++
			if entry.Date > group.categoryLastSeen[category] {
				group.categoryLastSeen[category] = entry.Date
			}
		}
	}

	ordered := make([]*merchantSuggestionGroup, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.prefixMatch != right.prefixMatch {
			return left.prefixMatch
		}
		if left.transactionCount != right.transactionCount {
			return left.transactionCount > right.transactionCount
		}
		if left.lastSeenDate != right.lastSeenDate {
			return left.lastSeenDate > right.lastSeenDate
		}
		return left.key < right.key
	})

	if limit > len(ordered) {
		limit = len(ordered)
	}
	suggestions := make([]MerchantSuggestion, 0, limit)
	for _, group := range ordered[:limit] {
		suggestions = append(suggestions, MerchantSuggestion{
			Merchant:         group.merchant,
			Category:         topMerchantCategory(group),
			TransactionCount: group.transactionCount,
			LastSeenDate:     group.lastSeenDate,
		})
	}
	return suggestions
}

func topMerchantCategory(group *merchantSuggestionGroup) string {
	bestCategory := ""
	bestCount := 0
	bestLastSeen := ""
	for category, count := range group.categoryCounts {
		lastSeen := group.categoryLastSeen[category]
		if count > bestCount ||
			(count == bestCount && lastSeen > bestLastSeen) ||
			(count == bestCount && lastSeen == bestLastSeen && strings.ToLower(category) < strings.ToLower(bestCategory)) {
			bestCategory = category
			bestCount = count
			bestLastSeen = lastSeen
		}
	}
	return bestCategory
}
