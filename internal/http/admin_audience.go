package http

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type audienceBucket struct {
	Key   string `json:"key"`
	Users int    `json:"users"`
}

// GET /v1/admin/audience?start&end&include_test — which apps and browsers
// people used in the window, real users only by default. Someone on both the
// app and the website counts in both rows, so a breakdown can sum to more
// than active_users.
func (s *Server) getAdminAudience(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	includeTest := adminIncludeTest(c)

	var clients []models.UserClient
	if err := realUserRows(database.DB.Model(&models.UserClient{}), "user_id", includeTest).
		Where("last_seen_at >= ? AND last_seen_at < ?", start, end).Find(&clients).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_audience"})
		return
	}
	type tally map[string]map[uint]struct{}
	add := func(t tally, key string, userID uint) {
		if key == "" {
			key = "unknown"
		}
		if t[key] == nil {
			t[key] = map[uint]struct{}{}
		}
		t[key][userID] = struct{}{}
	}
	platform, form, variant, version, osName, device := tally{}, tally{}, tally{}, tally{}, tally{}, tally{}
	seen := map[uint]struct{}{}
	for _, client := range clients {
		seen[client.UserID] = struct{}{}
		add(platform, client.Platform, client.UserID)
		add(form, client.FormFactor, client.UserID)
		add(osName, client.OS, client.UserID)
		add(device, client.Device, client.UserID)
		if client.Platform == clientPlatformWeb {
			continue
		}
		add(variant, client.Variant, client.UserID)
		appVersion := client.AppVersion
		if appVersion != "" && client.AppBuild != "" {
			appVersion += " (" + client.AppBuild + ")"
		}
		add(version, appVersion, client.UserID)
	}
	buckets := func(t tally, limit int) []audienceBucket {
		out := make([]audienceBucket, 0, len(t))
		for key, users := range t {
			out = append(out, audienceBucket{Key: key, Users: len(users)})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Users != out[j].Users {
				return out[i].Users > out[j].Users
			}
			return out[i].Key < out[j].Key
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}

	// Active people the console has no app or browser for yet: they were last
	// active before devices were recorded, or only on an app build that
	// predates it and made no request since.
	active := activeUserIDs(start, end, includeTest)
	unrecorded := 0
	for id := range active {
		if _, ok := seen[id]; !ok {
			unrecorded++
		}
	}

	var signupRows []struct {
		SignupPlatform string
		Users          int
	}
	_ = realUsersOnly(database.DB.Model(&models.User{}), includeTest).
		Select("signup_platform, COUNT(*) AS users").
		Where("created_at >= ? AND created_at < ?", start, end).
		Group("signup_platform").Scan(&signupRows).Error
	signupBuckets := []audienceBucket{}
	for _, row := range signupRows {
		key := row.SignupPlatform
		if key == "" {
			key = "unknown"
		}
		signupBuckets = append(signupBuckets, audienceBucket{Key: key, Users: row.Users})
	}
	sort.Slice(signupBuckets, func(i, j int) bool { return signupBuckets[i].Users > signupBuckets[j].Users })

	var pushRows []struct {
		Platform string
		Users    int
	}
	_ = realUserRows(database.DB.Model(&models.PushDevice{}), "user_id", includeTest).
		Select("platform, COUNT(DISTINCT user_id) AS users").
		Where("active = ?", true).Group("platform").Scan(&pushRows).Error
	push := []audienceBucket{}
	for _, row := range pushRows {
		push = append(push, audienceBucket{Key: row.Platform, Users: row.Users})
	}

	c.JSON(http.StatusOK, gin.H{
		"window":                  gin.H{"start": start, "end": end},
		"active_users":            len(seen) + unrecorded,
		"recorded_users":          len(seen),
		"unrecorded_active_users": unrecorded,
		"platform":                buckets(platform, 0),
		"form_factor":             buckets(form, 0),
		"variant":                 buckets(variant, 0),
		"app_version":             buckets(version, 12),
		"os":                      buckets(osName, 12),
		"device":                  buckets(device, 15),
		"signups_by_platform":     signupBuckets,
		"push_reach":              push,
		"recorded_since":          clientRecordingStarted(),
	})
}

// clientRecordingStarted is when the first app or browser was recorded, so
// the page can say how far back its numbers go.
func clientRecordingStarted() *time.Time {
	var first models.UserClient
	if err := database.DB.Order("first_seen_at ASC").Limit(1).Find(&first).Error; err != nil || first.ID == 0 {
		return nil
	}
	return &first.FirstSeenAt
}

// latestClients returns each user's most recently used app or browser.
func latestClients(ids []uint) map[uint]models.UserClient {
	latest := map[uint]models.UserClient{}
	if len(ids) == 0 {
		return latest
	}
	var rows []models.UserClient
	_ = database.DB.Where("user_id IN ?", ids).Order("last_seen_at ASC").Find(&rows).Error
	for _, row := range rows {
		latest[row.UserID] = row
	}
	return latest
}
