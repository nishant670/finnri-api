package models

import "time"

const (
	AppPlatformAndroid = "android"
	AppPlatformIOS     = "ios"

	AppReleaseStatusDraft     = "draft"
	AppReleaseStatusPublished = "published"

	// The four kinds of card on the app's "What's new" screen.
	AppReleaseItemNew      = "new"      // a feature: a live preview and a Try it link
	AppReleaseItemUI       = "ui"       // a new look: a live preview of the new screen
	AppReleaseItemFix      = "fix"      // a bug fix: what was happening, what we fixed
	AppReleaseItemImproved = "improved" // a one-line improvement
)

// AppPlatforms, AppReleaseStatuses and AppReleaseItemKinds are the allowed
// values, and schema.go builds its CHECK constraints from them, so the
// handlers and the database cannot disagree.
var (
	AppPlatforms        = []string{AppPlatformAndroid, AppPlatformIOS}
	AppReleaseStatuses  = []string{AppReleaseStatusDraft, AppReleaseStatusPublished}
	AppReleaseItemKinds = []string{AppReleaseItemNew, AppReleaseItemUI, AppReleaseItemFix, AppReleaseItemImproved}
)

// AppRelease is the release notes for one store build, written in the admin
// dashboard and read by the app.
//
// The notes have to live here rather than in the app: the update card is shown
// by the build the user already has, which cannot know what the next one
// contains. VersionCode is the store's build number (Android versionCode, iOS
// build number); VersionName is what users see ("1.1").
//
// MinSupported marks a critical fix. Every build below the highest published
// release with it set must update before it can be used.
type AppRelease struct {
	ID           uint             `gorm:"primaryKey" json:"id"`
	Platform     string           `gorm:"type:varchar(10);not null;uniqueIndex:idx_app_releases_platform_version" json:"platform"`
	VersionCode  int              `gorm:"not null;uniqueIndex:idx_app_releases_platform_version" json:"version_code"`
	VersionName  string           `gorm:"type:varchar(20);not null" json:"version_name"`
	Headline     string           `gorm:"type:varchar(140);not null" json:"headline"`
	Status       string           `gorm:"type:varchar(12);not null;default:draft" json:"status"`
	MinSupported bool             `gorm:"not null;default:false" json:"min_supported"`
	PublishedAt  *time.Time       `json:"published_at"`
	Items        []AppReleaseItem `gorm:"foreignKey:ReleaseID;constraint:OnDelete:CASCADE" json:"items"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// AppReleaseItem is one card on the "What's new" screen, as text. Which fields
// apply depends on Kind: Problem and Fix belong to a fix, DeepLink to a
// feature. Pictures are not stored: the build that has a feature draws its own
// live preview, and an older build shows the text.
type AppReleaseItem struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ReleaseID uint   `gorm:"not null;index" json:"release_id"`
	Position  int    `gorm:"not null;default:0" json:"position"`
	Kind      string `gorm:"type:varchar(12);not null" json:"kind"`
	Title     string `gorm:"type:varchar(80);not null" json:"title"`
	Body      string `gorm:"type:varchar(400);not null;default:''" json:"body"`
	Problem   string `gorm:"type:varchar(300);not null;default:''" json:"problem"`
	Fix       string `gorm:"type:varchar(300);not null;default:''" json:"fix"`
	DeepLink  string `gorm:"type:varchar(200);not null;default:''" json:"deep_link"`
}
