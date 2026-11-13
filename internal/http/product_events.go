package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// productEventNames is every event the clients may record, in funnel order.
// An event that is not here is dropped, not stored: the list is the contract,
// and it is what keeps this table from becoming a place things leak into.
var productEventNames = []string{
	"app_opened",
	"onboarding_completed",
	"auth_completed",    // label: google | guest | upgrade
	"capture_requested", // label: text | voice | receipt
	"capture_drafted",   // label: text | voice | receipt
	"capture_failed",    // label: text | voice | receipt
	"entry_saved",       // label: manual | text | voice | receipt | …
	"paywall_shown",     // label: what brought it up
	"checkout_started",  // label: plan code
	"invite_created",
	"invite_accepted",
}

var productEventNameSet = func() map[string]bool {
	set := make(map[string]bool, len(productEventNames))
	for _, name := range productEventNames {
		set[name] = true
	}
	return set
}()

const (
	maxProductEventsPerBatch = 50
	// A phone that was offline sends late. Anything older than this says more
	// about a wrong clock than about the week it claims to be from.
	maxProductEventAge       = 7 * 24 * time.Hour
	productEventRetention    = 400 * 24 * time.Hour
	maxProductEventLabelSize = 32
)

// validProductEventToken accepts lowercase words: a-z, 0-9 and underscores.
// Labels and the platform both go through it, which is the whole privacy rule
// in one place — nothing a person typed can be shaped like this by accident,
// and nothing longer than a word fits.
func validProductEventToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

func validAppVersion(value string) bool {
	if len(value) > 24 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && r != '.' && r != '-' && r != '+' {
			return false
		}
	}
	return true
}

// gormSession starts each funnel query from a fresh copy of the shared window,
// so one query's Select and Group do not carry into the next.
var gormSession = gorm.Session{}

// POST /v1/analytics/events
//
// Not /v1/events — that is the occasions feature. Events arrive in batches
// from the app's queue. A bad event is dropped and counted rather than failing
// the batch: the client cannot fix it, and refusing the good ones with it
// would only make the queue retry the same bad one for ever.
func (s *Server) recordProductEvents(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input struct {
		Platform   string `json:"platform"`
		AppVersion string `json:"app_version"`
		Events     []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
			At    string `json:"at"`
		} `json:"events"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if len(input.Events) > maxProductEventsPerBatch {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "too_many_events", "max": maxProductEventsPerBatch})
		return
	}

	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	if !validProductEventToken(platform, 16) {
		platform = ""
	}
	appVersion := strings.ToLower(strings.TrimSpace(input.AppVersion))
	if !validAppVersion(appVersion) {
		appVersion = ""
	}

	now := time.Now().UTC()
	rows := make([]models.ProductEvent, 0, len(input.Events))
	for _, event := range input.Events {
		name := strings.TrimSpace(event.Name)
		label := strings.TrimSpace(event.Label)
		if !productEventNameSet[name] || (label != "" && !validProductEventToken(label, maxProductEventLabelSize)) {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(event.At))
		if err != nil {
			at = now
		}
		at = at.UTC()
		if at.After(now) {
			at = now
		}
		if now.Sub(at) > maxProductEventAge {
			continue
		}
		rows = append(rows, models.ProductEvent{
			UserID: userID, Name: name, Label: label, Platform: platform, AppVersion: appVersion, OccurredAt: at,
		})
	}
	if len(rows) > 0 {
		if err := database.DB.Create(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_record_events"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"accepted": len(rows), "dropped": len(input.Events) - len(rows)})
}

// GET /v1/admin/analytics/product-funnel
//
// For each event, in funnel order: how many people did it and how many times,
// split by label. Defaults to the last thirty days.
func (s *Server) getAdminProductFunnel(c *gin.Context) {
	end := time.Now().UTC()
	start := end.Add(-30 * 24 * time.Hour)
	var err error
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		if start, err = parseFlexibleDate(raw, false); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_start"})
			return
		}
	}
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		if end, err = parseFlexibleDate(raw, true); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_end"})
			return
		}
	}

	window := database.DB.Model(&models.ProductEvent{}).Where("occurred_at >= ? AND occurred_at < ?", start, end)

	var totals []struct {
		Name   string
		Users  int64
		Events int64
	}
	if err := window.Session(&gormSession).
		Select("name, COUNT(DISTINCT user_id) AS users, COUNT(*) AS events").
		Group("name").Scan(&totals).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_product_funnel"})
		return
	}
	var byLabel []struct {
		Name   string
		Label  string
		Users  int64
		Events int64
	}
	if err := window.Session(&gormSession).
		Select("name, label, COUNT(DISTINCT user_id) AS users, COUNT(*) AS events").
		Where("label <> ?", "").
		Group("name, label").Order("events DESC").Scan(&byLabel).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_product_funnel"})
		return
	}
	var people int64
	if err := window.Session(&gormSession).Distinct("user_id").Count(&people).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_product_funnel"})
		return
	}

	totalFor := map[string]int{}
	for index, row := range totals {
		totalFor[row.Name] = index
	}
	steps := make([]gin.H, 0, len(productEventNames))
	for _, name := range productEventNames {
		step := gin.H{"name": name, "users": int64(0), "events": int64(0), "labels": []gin.H{}}
		if index, ok := totalFor[name]; ok {
			step["users"], step["events"] = totals[index].Users, totals[index].Events
		}
		labels := []gin.H{}
		for _, row := range byLabel {
			if row.Name == name {
				labels = append(labels, gin.H{"label": row.Label, "users": row.Users, "events": row.Events})
			}
		}
		step["labels"] = labels
		steps = append(steps, step)
	}
	c.JSON(http.StatusOK, gin.H{"start": start, "end": end, "people": people, "steps": steps})
}

// purgeOldProductEvents is the retention rule: usage history older than about
// thirteen months answers no question worth keeping it for.
func purgeOldProductEvents(now time.Time) (int64, error) {
	result := database.DB.Where("occurred_at < ?", now.Add(-productEventRetention)).Delete(&models.ProductEvent{})
	return result.RowsAffected, result.Error
}
