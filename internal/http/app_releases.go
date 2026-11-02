package http

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
// before sign-in too, and a guest who never signs in still gets updates. The
// admin dashboard writes them as drafts, uploads their screenshots, and
// publishes them.

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

	// Screenshots are shown in a card, not full screen. Anything bigger than
	// this is an unoptimised export that every user would download.
	maxReleaseMediaBytes = 3 * 1024 * 1024
)

// releaseMediaDir sits inside the uploads volume so it survives redeploys,
// but apart from receipts: these files are public, receipts never are.
var releaseMediaDir = filepath.Join(uploadDir, "release-media")

// releaseMediaTypes maps an accepted image type to the extension it is stored
// under. GIF and WebP may be animated, which is how a release shows motion
// without the app needing a video player.
var releaseMediaTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

var (
	releaseVersionNamePattern = regexp.MustCompile(`^\d{1,4}(\.\d{1,4}){0,3}$`)
	// An in-app route such as /events or /entry/new?type=expense. The app
	// opens it with the router, so it never names another host or scheme.
	releaseDeepLinkPattern = regexp.MustCompile(`^/[A-Za-z0-9_\-/]*(\?[A-Za-z0-9_\-=&.]*)?$`)
)

type appReleaseItemInput struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Problem     string `json:"problem"`
	Fix         string `json:"fix"`
	Image       string `json:"image"`
	BeforeImage string `json:"before_image"`
	AfterImage  string `json:"after_image"`
	DeepLink    string `json:"deep_link"`
}

type appReleaseInput struct {
	Platform     string                `json:"platform"`
	VersionCode  int                   `json:"version_code"`
	VersionName  string                `json:"version_name"`
	Headline     string                `json:"headline"`
	MinSupported bool                  `json:"min_supported"`
	Items        []appReleaseItemInput `json:"items"`
}

// AppReleaseItemView is an item with its images as URLs the app can load.
type AppReleaseItemView struct {
	models.AppReleaseItem
	ImageURL       string `json:"image_url,omitempty"`
	BeforeImageURL string `json:"before_image_url,omitempty"`
	AfterImageURL  string `json:"after_image_url,omitempty"`
}

// AppReleaseView is a release as the app and the admin editor read it.
// Its Items shadow the embedded release's.
type AppReleaseView struct {
	models.AppRelease
	Items []AppReleaseItemView `json:"items"`
}

// normalize cleans the input in place and returns what is wrong with it.
// Fields that do not apply to an item's kind are cleared rather than refused,
// so switching a card's kind in the editor never strands hidden values.
func (input *appReleaseInput) normalize() map[string]string {
	fields := map[string]string{}

	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	if !containsString(models.AppPlatforms, input.Platform) {
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
	item.Image = strings.TrimSpace(item.Image)
	item.BeforeImage = strings.TrimSpace(item.BeforeImage)
	item.AfterImage = strings.TrimSpace(item.AfterImage)
	item.DeepLink = strings.TrimSpace(item.DeepLink)

	if !containsString(models.AppReleaseItemKinds, item.Kind) {
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
	if item.Kind != models.AppReleaseItemUI {
		item.BeforeImage, item.AfterImage = "", ""
	}
	if item.Kind == models.AppReleaseItemUI || item.Kind == models.AppReleaseItemImproved {
		item.Image = ""
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
	if item.Kind == models.AppReleaseItemUI && (item.BeforeImage == "" || item.AfterImage == "") {
		fields["before_image"] = "a new look needs a before and an after screenshot"
	}

	for key, name := range map[string]string{"image": item.Image, "before_image": item.BeforeImage, "after_image": item.AfterImage} {
		if name == "" {
			continue
		}
		if !releaseMediaExists(name) {
			fields[key] = "upload the screenshot first"
		}
	}

	if item.DeepLink != "" && (len(item.DeepLink) > maxReleaseDeepLink ||
		strings.HasPrefix(item.DeepLink, "//") || !releaseDeepLinkPattern.MatchString(item.DeepLink)) {
		fields["deep_link"] = "must be an in-app path such as /events"
	}
	return fields
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (input appReleaseInput) items(releaseID uint) []models.AppReleaseItem {
	items := make([]models.AppReleaseItem, len(input.Items))
	for index, item := range input.Items {
		items[index] = models.AppReleaseItem{
			ReleaseID:   releaseID,
			Position:    index,
			Kind:        item.Kind,
			Title:       item.Title,
			Body:        item.Body,
			Problem:     item.Problem,
			Fix:         item.Fix,
			Image:       item.Image,
			BeforeImage: item.BeforeImage,
			AfterImage:  item.AfterImage,
			DeepLink:    item.DeepLink,
		}
	}
	return items
}

func releaseMediaURL(r *http.Request, name string) string {
	if name == "" {
		return ""
	}
	return publicOrigin(r) + "/v1/app/release-media/" + name
}

func appReleaseView(r *http.Request, release models.AppRelease) AppReleaseView {
	view := AppReleaseView{AppRelease: release, Items: make([]AppReleaseItemView, len(release.Items))}
	for index, item := range release.Items {
		view.Items[index] = AppReleaseItemView{
			AppReleaseItem: item,
			ImageURL:       releaseMediaURL(r, item.Image),
			BeforeImageURL: releaseMediaURL(r, item.BeforeImage),
			AfterImageURL:  releaseMediaURL(r, item.AfterImage),
		}
	}
	return view
}

func appReleaseViews(r *http.Request, releases []models.AppRelease) []AppReleaseView {
	views := make([]AppReleaseView, len(releases))
	for index, release := range releases {
		views[index] = appReleaseView(r, release)
	}
	return views
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
	if !containsString(models.AppPlatforms, platform) {
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
		"releases":                   appReleaseViews(c.Request, releases),
		"latest_version_code":        bounds.Latest,
		"min_supported_version_code": bounds.MinSupported,
	})
}

// getAppRelease is one published release, read by a freshly updated build to
// show its own notes once.
func (s *Server) getAppRelease(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
	if !containsString(models.AppPlatforms, platform) {
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
	c.JSON(http.StatusOK, appReleaseView(c.Request, release))
}

// serveReleaseMedia serves a release screenshot. Names are random and a file
// is never rewritten in place, so it can be cached for good.
func (s *Server) serveReleaseMedia(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; object-src 'none'; sandbox")

	name, ok := safeReleaseMediaName(c.Param("name"))
	if !ok || !releaseMediaExists(name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "media_not_found"})
		return
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.File(filepath.Join(releaseMediaDir, name))
}

// safeReleaseMediaName accepts only names this server could have generated: 32
// hex characters and an allowed image extension. Anything else, a path above
// all, is refused before the disk is touched.
func safeReleaseMediaName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || filepath.Base(name) != name {
		return "", false
	}
	extension := strings.ToLower(filepath.Ext(name))
	allowed := false
	for _, candidate := range releaseMediaTypes {
		if extension == candidate {
			allowed = true
			break
		}
	}
	stem := strings.TrimSuffix(name, extension)
	if !allowed || len(stem) != 32 {
		return "", false
	}
	for _, r := range stem {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", false
		}
	}
	return name, true
}

func releaseMediaExists(raw string) bool {
	name, ok := safeReleaseMediaName(raw)
	if !ok {
		return false
	}
	info, err := os.Stat(filepath.Join(releaseMediaDir, name))
	return err == nil && info.Mode().IsRegular()
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

// bindAppReleaseInput reads and checks an editor save. Turning on "minimum
// supported" forces everyone on an older build to update, so it is an owner's
// call even though the rest of a release is support's.
func bindAppReleaseInput(c *gin.Context, wasMinSupported bool) (appReleaseInput, bool) {
	var input appReleaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return input, false
	}
	if fields := input.normalize(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_release", "fields": fields})
		return input, false
	}
	if input.MinSupported && !wasMinSupported {
		if admin := currentAdmin(c); admin == nil || adminRoleRank(admin.Role) < adminRoleRank(models.AdminRoleOwner) {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin_role_required", "required_role": models.AdminRoleOwner})
			return input, false
		}
	}
	return input, true
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
		if !containsString(models.AppPlatforms, platform) {
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
	c.JSON(http.StatusOK, gin.H{"releases": appReleaseViews(c.Request, releases)})
}

func (s *Server) getAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, appReleaseView(c.Request, release))
}

// createAdminAppRelease starts a release as a draft; nothing reaches the app
// until it is published.
func (s *Server) createAdminAppRelease(c *gin.Context) {
	input, ok := bindAppReleaseInput(c, false)
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
	c.JSON(http.StatusCreated, appReleaseView(c.Request, release))
}

// updateAdminAppRelease replaces a release and all of its cards in one save,
// which is how the editor works: reordering, adding and removing cards are all
// one PUT. A published release can be edited too, to fix a typo in place.
func (s *Server) updateAdminAppRelease(c *gin.Context) {
	release, ok := loadAdminAppRelease(c)
	if !ok {
		return
	}
	input, ok := bindAppReleaseInput(c, release.MinSupported)
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
	c.JSON(http.StatusOK, appReleaseView(c.Request, release))
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
	c.JSON(http.StatusOK, appReleaseView(c.Request, release))
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
	c.JSON(http.StatusOK, appReleaseView(c.Request, release))
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

// uploadAdminReleaseMedia stores one screenshot and returns the name to put on
// a card. The type comes from the file's bytes, never from its name.
func (s *Server) uploadAdminReleaseMedia(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file provided"})
		return
	}
	if file.Size > maxReleaseMediaBytes || file.Size > s.cfg.MaxUploadMB*1024*1024 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large", "fields": gin.H{"file": "keep screenshots under 3 MB; export as WebP"}})
		return
	}
	extension, err := detectReleaseMediaExtension(file)
	if err != nil {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error":  "unsupported_file_type",
			"fields": gin.H{"file": "attach a PNG, JPEG, WebP or GIF image"},
		})
		return
	}
	name, err := randomUploadName(extension)
	if err != nil {
		log.Printf("failed_save_release_media stage=name err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}
	if err := os.MkdirAll(releaseMediaDir, 0o755); err != nil {
		log.Printf("failed_save_release_media stage=mkdir path=%s err=%v", releaseMediaDir, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}
	path := filepath.Join(releaseMediaDir, name)
	if err := c.SaveUploadedFile(file, path); err != nil {
		log.Printf("failed_save_release_media stage=write path=%s err=%v", path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"name": name, "url": releaseMediaURL(c.Request, name)})
}

func detectReleaseMediaExtension(file *multipart.FileHeader) (string, error) {
	opened, err := file.Open()
	if err != nil {
		return "", err
	}
	defer opened.Close()

	header := make([]byte, sniffLength)
	read, err := io.ReadFull(opened, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	mediaType, _, _ := strings.Cut(http.DetectContentType(header[:read]), ";")
	extension, ok := releaseMediaTypes[strings.TrimSpace(mediaType)]
	if !ok {
		return "", fmt.Errorf("unsupported release media type %q", mediaType)
	}
	return extension, nil
}
