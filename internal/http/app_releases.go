package http

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Release notes for the app's update card and "What's new" screen.
//
// The app reads published releases without a session: the update card shows
// before sign-in too, and a guest who never signs in still gets updates.
//
// Text only. Each build carries its own notes with live previews drawn from
// its real screens (finnri-app lib/whats-new); this copy exists so an older
// build can say what the next one brings. `scripts/publish-release-notes` in
// the app writes it through the admin endpoints below.

const (
	maxReleaseItems          = 12
	maxReleaseHeadlineLength = 140
	maxReleaseVersionName    = 20
	maxReleaseItemTitle      = 80
	maxReleaseItemBody       = 400
	maxReleaseItemProblem    = 300
	maxReleaseDeepLink       = 200
	// Play's own ceiling for versionCode.
	maxReleaseVersionCode = 2100000000

	defaultReleaseListLimit = 10
	maxReleaseListLimit     = 20
)

var (
	releaseVersionNamePattern = regexp.MustCompile(`^\d{1,4}(\.\d{1,4}){0,3}$`)
	// An in-app route such as /events, /(tabs)/split or /entry/new?type=expense.
	// The app opens it with the router, so it never names another host or scheme.
	releaseDeepLinkPattern = regexp.MustCompile(`^/[A-Za-z0-9_\-/()]*(\?[A-Za-z0-9_\-=&.]*)?$`)
)

type appReleaseItemInput struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
	DeepLink string `json:"deep_link"`
}

type appReleaseInput struct {
	Platform     string                `json:"platform"`
	VersionCode  int                   `json:"version_code"`
	VersionName  string                `json:"version_name"`
	Headline     string                `json:"headline"`
	MinSupported bool                  `json:"min_supported"`
	Items        []appReleaseItemInput `json:"items"`
}

// normalize cleans the input in place and returns what is wrong with it.
// Fields that do not apply to an item's kind are cleared rather than refused,
// so switching a card's kind in the editor never strands hidden values.
func (input *appReleaseInput) normalize() map[string]string {
	fields := map[string]string{}

	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	if !slices.Contains(models.AppPlatforms, input.Platform) {
		fields["platform"] = "must be android or ios"
	}
	if input.VersionCode < 1 || input.VersionCode > maxReleaseVersionCode {
		fields["version_code"] = "must be the store build number, from 1 to 2100000000"
	}
	input.VersionName = strings.TrimSpace(input.VersionName)
	if !releaseVersionNamePattern.MatchString(input.VersionName) || len(input.VersionName) > maxReleaseVersionName {
		fields["version_name"] = "must look like 1.1 or 1.2.3"
	}
	input.Headline = collapseSpaces(input.Headline)
	if input.Headline == "" {
		fields["headline"] = "is required"
	} else if utf8.RuneCountInString(input.Headline) > maxReleaseHeadlineLength {
		fields["headline"] = fmt.Sprintf("must be at most %d characters", maxReleaseHeadlineLength)
	}
	if len(input.Items) > maxReleaseItems {
		fields["items"] = fmt.Sprintf("a release can have at most %d cards", maxReleaseItems)
		return fields
	}

	for index := range input.Items {
		for key, message := range input.Items[index].normalize() {
			fields[fmt.Sprintf("items[%d].%s", index, key)] = message
		}
	}
	return fields
}

func (item *appReleaseItemInput) normalize() map[string]string {
	fields := map[string]string{}

	item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
	item.Title = collapseSpaces(item.Title)
	item.Body = strings.TrimSpace(item.Body)
	item.Problem = strings.TrimSpace(item.Problem)
	item.Fix = strings.TrimSpace(item.Fix)
	item.DeepLink = strings.TrimSpace(item.DeepLink)

	if !slices.Contains(models.AppReleaseItemKinds, item.Kind) {
		fields["kind"] = "must be new, ui, fix or improved"
		return fields
	}
	if item.Title == "" {
		fields["title"] = "is required"
	} else if utf8.RuneCountInString(item.Title) > maxReleaseItemTitle {
		fields["title"] = fmt.Sprintf("must be at most %d characters", maxReleaseItemTitle)
	}
	if utf8.RuneCountInString(item.Body) > maxReleaseItemBody {
		fields["body"] = fmt.Sprintf("must be at most %d characters", maxReleaseItemBody)
	}

	if item.Kind != models.AppReleaseItemFix {
		item.Problem, item.Fix = "", ""
	}
	if item.Kind != models.AppReleaseItemNew {
		item.DeepLink = ""
	}

	switch item.Kind {
	case models.AppReleaseItemNew, models.AppReleaseItemUI:
		if item.Body == "" {
			fields["body"] = "say in a sentence or two what changed"
		}
	case models.AppReleaseItemFix:
		if item.Problem == "" {
			fields["problem"] = "say what users saw going wrong"
		} else if utf8.RuneCountInString(item.Problem) > maxReleaseItemProblem {
			fields["problem"] = fmt.Sprintf("must be at most %d characters", maxReleaseItemProblem)
		}
		if item.Fix == "" {
			fields["fix"] = "say what works now"
		} else if utf8.RuneCountInString(item.Fix) > maxReleaseItemProblem {
			fields["fix"] = fmt.Sprintf("must be at most %d characters", maxReleaseItemProblem)
		}
	}

	if item.DeepLink != "" && (len(item.DeepLink) > maxReleaseDeepLink ||
		strings.HasPrefix(item.DeepLink, "//") || !releaseDeepLinkPattern.MatchString(item.DeepLink)) {
		fields["deep_link"] = "must be an in-app path such as /events"
	}
	return fields
}

func (input appReleaseInput) items(releaseID uint) []models.AppReleaseItem {
	items := make([]models.AppReleaseItem, len(input.Items))
	for index, item := range input.Items {
		items[index] = models.AppReleaseItem{
			ReleaseID: releaseID,
			Position:  index,
			Kind:      item.Kind,
			Title:     item.Title,
			Body:      item.Body,
			Problem:   item.Problem,
			Fix:       item.Fix,
			DeepLink:  item.DeepLink,
		}
	}
	return items
}

func preloadReleaseItems(db *gorm.DB) *gorm.DB {
	return db.Order("position ASC, id ASC")
}

// --- the app -----------------------------------------------------------------

// listAppReleases answers the app's update check: the published releases newer
// than the build asking, newest first, and the oldest build still allowed.
//
// min_supported_version_code is 0 when no release forces an update. The app
// compares it with its own build number; it never needs to know which release
// set it.
func (s *Server) listAppReleases(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
	if !slices.Contains(models.AppPlatforms, platform) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_platform"})
		return
	}
	since := 0
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_since"})
			return
		}
		since = parsed
	}
	limit := defaultReleaseListLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_limit"})
			return
		}
		limit = min(parsed, maxReleaseListLimit)
	}

	published := database.DB.Model(&models.AppRelease{}).
		Where("platform = ? AND status = ?", platform, models.AppReleaseStatusPublished)

	var releases []models.AppRelease
	if err := published.Session(&gorm.Session{}).
		Preload("Items", preloadReleaseItems).
		Where("version_code > ?", since).
		Order("version_code DESC").
		Limit(limit).
		Find(&releases).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_releases"})
		return
	}

	var bounds struct {
		Latest       int
		MinSupported int
	}
	if err := published.Session(&gorm.Session{}).
		Select("COALESCE(MAX(version_code), 0) AS latest, " +
			"COALESCE(MAX(CASE WHEN min_supported THEN version_code END), 0) AS min_supported").
		Scan(&bounds).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_releases"})
		return
	}

	// Short enough that a publish reaches everyone within minutes, long enough
	// that every app launch is not a database read.
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, gin.H{
		"releases":                   releases,
		"latest_version_code":        bounds.Latest,
		"min_supported_version_code": bounds.MinSupported,
	})
}

// getAppRelease is one published release, read by a freshly updated build to
// show its own notes once.
func (s *Server) getAppRelease(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
	if !slices.Contains(models.AppPlatforms, platform) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_platform"})
		return
	}
	versionCode, err := strconv.Atoi(c.Param("version_code"))
	if err != nil || versionCode < 1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "release_not_found"})
		return
	}

	var release models.AppRelease
	err = database.DB.Preload("Items", preloadReleaseItems).
		Where("platform = ? AND version_code = ? AND status = ?", platform, versionCode, models.AppReleaseStatusPublished).
		First(&release).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "release_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_release"})
		return
	}
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, release)
}

// --- the admin dashboard -----------------------------------------------------

func loadAdminAppRelease(c *gin.Context) (models.AppRelease, bool) {
	var release models.AppRelease
	id, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "release_not_found"})
		return release, false
	}
	err := database.DB.Preload("Items", preloadReleaseItems).First(&release, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "release_not_found"})
		return release, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_release"})
		return release, false
	}
	return release, true
}

// bindAppReleaseInput reads and checks an editor save. `current` is the
// release being edited, nil for a new one.
//
// Which builds must update is an owner's call, even though the rest of a
// release is support's: turning "minimum supported" on, and moving a release
// that has it to another build or platform. A moved floor can strand every
// installed build behind an update that does not exist.
func bindAppReleaseInput(c *gin.Context, current *models.AppRelease) (appReleaseInput, bool) {
	var input appReleaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return input, false
	}
	if fields := input.normalize(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_release", "fields": fields})
		return input, false
	}
	if movesUpdateFloor(current, input) {
		if admin := currentAdmin(c); admin == nil || adminRoleRank(admin.Role) < adminRoleRank(models.AdminRoleOwner) {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin_role_required", "required_role": models.AdminRoleOwner})
			return input, false
		}
	}
	return input, true
}

// movesUpdateFloor reports whether a save raises or moves the oldest build
// still allowed. Turning "minimum supported" off only lowers it, which any
// editor may do.
func movesUpdateFloor(current *models.AppRelease, input appReleaseInput) bool {
	if !input.MinSupported {
		return false
	}
	if current == nil || !current.MinSupported {
		return true
	}
	return input.VersionCode != current.VersionCode || input.Platform != current.Platform
}

// releaseVersionTaken reports whether another release already has this build.
func releaseVersionTaken(platform string, versionCode int, exceptID uint) (bool, error) {
	var count int64
	err := database.DB.Model(&models.AppRelease{}).
		Where("platform = ? AND version_code = ? AND id <> ?", platform, versionCode, exceptID).
		Count(&count).Error
	return count > 0, err
}

func (s *Server) listAdminAppReleases(c *gin.Context) {
	query := database.DB.Preload("Items", preloadReleaseItems)
	if platform := strings.ToLower(strings.TrimSpace(c.Query("platform"))); platform != "" {
		if !slices.Contains(models.AppPlatforms, platform) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_platform"})
			return
		}
		query = query.Where("platform = ?", platform)
	}
	var releases []models.AppRelease
	if err := query.Order("platform ASC, version_code DESC").Find(&releases).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_releases"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"releases": releases})
}

func (s *Server) getAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, release)
}

// createAdminAppRelease starts a release as a draft; nothing reaches the app
// until it is published.
func (s *Server) createAdminAppRelease(c *gin.Context) {
	input, ok := bindAppReleaseInput(c, nil)
	if !ok {
		return
	}
	if taken, err := releaseVersionTaken(input.Platform, input.VersionCode, 0); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_release"})
		return
	} else if taken {
		c.JSON(http.StatusConflict, gin.H{"error": "release_exists", "fields": gin.H{"version_code": "this build already has release notes"}})
		return
	}

	release := models.AppRelease{
		Platform:     input.Platform,
		VersionCode:  input.VersionCode,
		VersionName:  input.VersionName,
		Headline:     input.Headline,
		Status:       models.AppReleaseStatusDraft,
		MinSupported: input.MinSupported,
	}
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit(clause.Associations).Create(&release).Error; err != nil {
			return err
		}
		release.Items = input.items(release.ID)
		if len(release.Items) == 0 {
			return nil
		}
		return tx.Create(&release.Items).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_release"})
		return
	}
	c.JSON(http.StatusCreated, release)
}

// updateAdminAppRelease replaces a release and all of its cards in one save,
// which is how the editor works: reordering, adding and removing cards are all
// one PUT. A published release can be edited too, to fix a typo in place.
func (s *Server) updateAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	input, ok := bindAppReleaseInput(c, &release)
	if !ok {
		return
	}
	if release.Status == models.AppReleaseStatusPublished && len(input.Items) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_release", "fields": gin.H{"items": "a published release needs at least one card"}})
		return
	}
	if taken, err := releaseVersionTaken(input.Platform, input.VersionCode, release.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_release"})
		return
	} else if taken {
		c.JSON(http.StatusConflict, gin.H{"error": "release_exists", "fields": gin.H{"version_code": "this build already has release notes"}})
		return
	}

	release.Platform = input.Platform
	release.VersionCode = input.VersionCode
	release.VersionName = input.VersionName
	release.Headline = input.Headline
	release.MinSupported = input.MinSupported
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit(clause.Associations).Save(&release).Error; err != nil {
			return err
		}
		if err := tx.Where("release_id = ?", release.ID).Delete(&models.AppReleaseItem{}).Error; err != nil {
			return err
		}
		release.Items = input.items(release.ID)
		if len(release.Items) == 0 {
			return nil
		}
		return tx.Create(&release.Items).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_release"})
		return
	}
	c.JSON(http.StatusOK, release)
}

func (s *Server) publishAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	if len(release.Items) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_release", "fields": gin.H{"items": "add at least one card before publishing"}})
		return
	}
	if release.Status != models.AppReleaseStatusPublished {
		now := time.Now().UTC()
		release.Status = models.AppReleaseStatusPublished
		release.PublishedAt = &now
		if err := database.DB.Omit(clause.Associations).Save(&release).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_publish_release"})
			return
		}
	}
	c.JSON(http.StatusOK, release)
}

func (s *Server) unpublishAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	if release.Status != models.AppReleaseStatusDraft {
		release.Status = models.AppReleaseStatusDraft
		release.PublishedAt = nil
		if err := database.DB.Omit(clause.Associations).Save(&release).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_unpublish_release"})
			return
		}
	}
	c.JSON(http.StatusOK, release)
}

// deleteAdminAppRelease removes a draft. A published release has to be
// unpublished first, so a misclick never pulls notes out from under users.
// Its screenshots stay in the media store; another release may use them.
func (s *Server) deleteAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	if release.Status == models.AppReleaseStatusPublished {
		c.JSON(http.StatusConflict, gin.H{"error": "release_published"})
		return
	}
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("release_id = ?", release.ID).Delete(&models.AppReleaseItem{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.AppRelease{}, release.ID).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_release"})
		return
	}
	c.Status(http.StatusNoContent)
}
