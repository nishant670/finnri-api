package http

import (
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	feedbackTypeBug        = "bug"
	feedbackTypeIdea       = "idea"
	feedbackTypeImprove    = "improvement"
	feedbackTypeFeature    = "feature_request"
	feedbackTypeOther      = "other"
	feedbackImpactCritical = "critical"
	feedbackImpactHigh     = "high"
	feedbackImpactMedium   = "medium"
	feedbackImpactNice     = "nice_to_have"
)

type feedbackInput struct {
	Type    string `json:"type"`
	Area    string `json:"area"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Impact  string `json:"impact"`
	// Upload URLs from POST /v1/upload. Optional.
	Attachments []string `json:"attachments"`
}

// maxFeedbackAttachments is enough to show a before and after and the screen
// around it; a report that needs more is a conversation, not a form.
const maxFeedbackAttachments = 3

func (s *Server) createFeedback(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input feedbackInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}

	feedback, fields := input.toModel(userID)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_feedback", "fields": fields})
		return
	}

	if err := database.DB.Create(&feedback).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_feedback"})
		return
	}
	c.JSON(http.StatusCreated, feedback)
}

func (input feedbackInput) toModel(userID uint) (models.Feedback, gin.H) {
	fields := gin.H{}
	feedbackType := normalizeFeedbackType(input.Type)
	impact := normalizeFeedbackImpact(input.Impact)
	area := strings.TrimSpace(input.Area)
	title := strings.TrimSpace(input.Title)
	message := strings.TrimSpace(input.Message)

	if feedbackType == "" {
		fields["type"] = "must be bug, idea, improvement, feature_request, or other"
	}
	if impact == "" {
		fields["impact"] = "must be critical, high, medium, or nice_to_have"
	}
	if title == "" {
		fields["title"] = "is required"
	} else if utf8.RuneCountInString(title) > 140 {
		fields["title"] = "must be 140 characters or less"
	}
	if message == "" {
		fields["message"] = "is required"
	} else if utf8.RuneCountInString(message) > 2000 {
		fields["message"] = "must be 2000 characters or less"
	}
	if utf8.RuneCountInString(area) > 64 {
		fields["area"] = "must be 64 characters or less"
	}
	attachments, attachmentError := feedbackAttachments(input.Attachments)
	if attachmentError != "" {
		fields["attachments"] = attachmentError
	}

	return models.Feedback{
		UserID:      userID,
		Type:        feedbackType,
		Area:        area,
		Title:       title,
		Message:     message,
		Impact:      impact,
		Status:      "new",
		Attachments: attachments,
	}, fields
}

// feedbackAttachments keeps only files this server stored. Anything else — a
// link to somewhere else, a path outside the upload directory — is refused
// rather than dropped, so the person learns their screenshot did not go.
func feedbackAttachments(raw []string) (models.StringArray, string) {
	attachments := models.StringArray{}
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		path, ok := localUploadPathFromAttachment(value)
		if !ok {
			return nil, "must be files uploaded through Finnri"
		}
		if _, ok := safeUploadName(filepath.Base(path)); !ok {
			return nil, "must be files uploaded through Finnri"
		}
		attachments = append(attachments, value)
	}
	if len(attachments) > maxFeedbackAttachments {
		return nil, "attach at most 3 files"
	}
	return attachments, ""
}

func normalizeFeedbackType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case feedbackTypeBug:
		return feedbackTypeBug
	case feedbackTypeIdea:
		return feedbackTypeIdea
	case feedbackTypeImprove, "improve":
		return feedbackTypeImprove
	case feedbackTypeFeature, "feature":
		return feedbackTypeFeature
	case feedbackTypeOther, "":
		return feedbackTypeOther
	default:
		return ""
	}
}

func normalizeFeedbackImpact(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case feedbackImpactCritical:
		return feedbackImpactCritical
	case feedbackImpactHigh:
		return feedbackImpactHigh
	case feedbackImpactMedium:
		return feedbackImpactMedium
	case feedbackImpactNice, "":
		return feedbackImpactNice
	default:
		return ""
	}
}
