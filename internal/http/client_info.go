package http

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Which apps and browsers people use, for the admin console's Audience page.

The app describes itself in headers on every request (from the build that
added them, 2026-11):

	X-Finnri-Platform      android | ios
	X-Finnri-App-Version   1.0.0
	X-Finnri-Build         14
	X-Finnri-OS            Android 15
	X-Finnri-Device        Samsung SM-S918B
	X-Finnri-Form-Factor   phone | tablet
	X-Finnri-Variant       play | preview | debug
	X-Finnri-Test-Device   1 on Google Play's pre-launch test devices

The website sends none of these; its User-Agent says enough (phone, tablet
or computer; browser; OS). Older app builds send only okhttp's User-Agent and
are recorded as an Android app of unknown version.

Nothing here is used to decide what anyone may do. It is descriptive, so
values are trimmed and length-capped rather than validated, and a client
that lies only changes what the admin console shows about it.
*/

const (
	clientPlatformAndroid = "android"
	clientPlatformIOS     = "ios"
	clientPlatformWeb     = "web"
	clientPlatformUnknown = "unknown"

	clientFormPhone   = "phone"
	clientFormTablet  = "tablet"
	clientFormDesktop = "desktop"
	clientFormUnknown = "unknown"

	headerClientTestDevice = "X-Finnri-Test-Device"
)

// clientWriteInterval bounds how often one user's one client is written.
const clientWriteInterval = 10 * time.Minute

type clientInfo struct {
	Platform     string
	FormFactor   string
	Device       string
	OS           string
	AppVersion   string
	AppBuild     string
	Variant      string
	IsTestDevice bool
}

var clientValueCleaner = regexp.MustCompile(`[^\p{L}\p{N} ._()/+\-]`)

func cleanClientValue(value string, limit int) string {
	value = strings.TrimSpace(clientValueCleaner.ReplaceAllString(value, ""))
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func oneOf(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

// clientInfoFromRequest describes the app or browser making this request.
func clientInfoFromRequest(c *gin.Context) clientInfo {
	info := clientInfo{
		Platform:     oneOf(c.GetHeader("X-Finnri-Platform"), clientPlatformAndroid, clientPlatformIOS, clientPlatformWeb),
		FormFactor:   oneOf(c.GetHeader("X-Finnri-Form-Factor"), clientFormPhone, clientFormTablet, clientFormDesktop),
		Device:       cleanClientValue(c.GetHeader("X-Finnri-Device"), 80),
		OS:           cleanClientValue(c.GetHeader("X-Finnri-OS"), 40),
		AppVersion:   cleanClientValue(c.GetHeader("X-Finnri-App-Version"), 24),
		AppBuild:     cleanClientValue(c.GetHeader("X-Finnri-Build"), 16),
		Variant:      oneOf(c.GetHeader("X-Finnri-Variant"), "play", "preview", "debug"),
		IsTestDevice: strings.TrimSpace(c.GetHeader(headerClientTestDevice)) == "1",
	}
	if info.Platform == "" {
		info = mergeUserAgent(info, c.GetHeader("User-Agent"))
	}
	if info.FormFactor == "" {
		info.FormFactor = clientFormUnknown
	}
	return info
}

// mergeUserAgent fills in what a client without app headers looks like from
// its User-Agent: the website in a browser, or an app build from before the
// headers existed.
func mergeUserAgent(info clientInfo, userAgent string) clientInfo {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.HasPrefix(ua, "okhttp/"):
		info.Platform = clientPlatformAndroid
		info.Device = firstNonEmpty(info.Device, "Older app build")
		return info
	case strings.Contains(ua, "cfnetwork") || strings.Contains(ua, "darwin/") && !strings.Contains(ua, "mozilla"):
		info.Platform = clientPlatformIOS
		info.Device = firstNonEmpty(info.Device, "Older app build")
		return info
	case strings.Contains(ua, "mozilla/"):
		info.Platform = clientPlatformWeb
		info.Variant = clientPlatformWeb
	default:
		info.Platform = clientPlatformUnknown
		return info
	}

	switch {
	case strings.Contains(ua, "ipad") || strings.Contains(ua, "tablet") || strings.Contains(ua, "android") && !strings.Contains(ua, "mobile"):
		info.FormFactor = clientFormTablet
	case strings.Contains(ua, "mobile") || strings.Contains(ua, "iphone"):
		info.FormFactor = clientFormPhone
	default:
		info.FormFactor = clientFormDesktop
	}
	os := "Other"
	switch {
	case strings.Contains(ua, "windows"):
		os = "Windows"
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad"):
		os = "iOS"
	case strings.Contains(ua, "android"):
		os = "Android"
	case strings.Contains(ua, "mac os x") || strings.Contains(ua, "macintosh"):
		os = "macOS"
	case strings.Contains(ua, "cros"):
		os = "ChromeOS"
	case strings.Contains(ua, "linux"):
		os = "Linux"
	}
	browser := "Browser"
	switch {
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "samsungbrowser/"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "opr/") || strings.Contains(ua, "opera"):
		browser = "Opera"
	case strings.Contains(ua, "firefox/") || strings.Contains(ua, "fxios/"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome/") || strings.Contains(ua, "crios/"):
		browser = "Chrome"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}
	info.OS = os
	info.Device = browser + " on " + os
	return info
}

// key is one row per user per app install or browser kind. Versions are left
// out so an update moves the same row forward instead of adding a new one.
func (info clientInfo) key() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{info.Platform, info.Variant, info.Device, info.FormFactor}, "|")))
	return hex.EncodeToString(sum[:16])
}

var clientWrites = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// dueForClientWrite reports whether this user's client was last written more
// than clientWriteInterval ago in this process, and marks it written.
func dueForClientWrite(userID uint, key string, now time.Time) bool {
	cacheKey := key + ":" + strconv.FormatUint(uint64(userID), 10)
	clientWrites.Lock()
	defer clientWrites.Unlock()
	if last, ok := clientWrites.at[cacheKey]; ok && now.Sub(last) < clientWriteInterval {
		return false
	}
	// A crude bound: the map is only a write throttle, so forgetting it costs
	// one extra upsert per active client.
	if len(clientWrites.at) > 50000 {
		clientWrites.at = map[string]time.Time{}
	}
	clientWrites.at[cacheKey] = now
	return true
}

// recordUserClient upserts this request's client for the signed-in user, at
// most every clientWriteInterval per client. It also fills signup_platform for
// an account made in the last hour that has none yet, and flags a guest using
// one of Google Play's test devices as a test account.
func recordUserClient(c *gin.Context, user *models.User) {
	if user == nil || user.ID == 0 {
		return
	}
	info := clientInfoFromRequest(c)
	now := time.Now().UTC()
	key := info.key()
	if !dueForClientWrite(user.ID, key, now) {
		return
	}
	row := models.UserClient{
		UserID: user.ID, ClientKey: key, Platform: info.Platform, FormFactor: info.FormFactor, Device: info.Device, OS: info.OS,
		AppVersion: info.AppVersion, AppBuild: info.AppBuild, Variant: info.Variant, IsTestDevice: info.IsTestDevice,
		FirstSeenAt: now, LastSeenAt: now,
	}
	if err := database.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "client_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"os", "app_version", "app_build", "is_test_device", "last_seen_at"}),
	}).Create(&row).Error; err != nil {
		log.Printf("[WARN] record user client failed user=%d: %v", user.ID, err)
		return
	}
	if user.SignupPlatform == "" && info.Platform != clientPlatformUnknown && now.Sub(user.CreatedAt) < time.Hour {
		_ = database.DB.Model(&models.User{}).Where("id = ? AND signup_platform = ''", user.ID).UpdateColumn("signup_platform", info.Platform).Error
	}
	if info.IsTestDevice {
		markGuestFromTestDevice(user.ID)
	}
}

// markGuestFromTestDevice flags a guest made on Google Play's pre-launch test
// devices. Only guests: a signed-in person who happens to use a test-lab
// device is still a person. An account whose flag has ever been set or
// cleared (test_marked_at not null) is left alone, so an admin's un-flag is
// never overruled.
func markGuestFromTestDevice(userID uint) {
	now := time.Now().UTC()
	_ = database.DB.Model(&models.User{}).
		Where("id = ? AND is_guest = ? AND is_test = ? AND test_marked_at IS NULL", userID, true, false).
		Updates(map[string]any{"is_test": true, "test_reason": models.UserTestReasonTestLab, "test_marked_at": &now}).Error
}
