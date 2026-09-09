package http

import (
	"net/http"
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
}

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

	return models.Feedback{
		UserID:  userID,
		Type:    feedbackType,
		Area:    area,
		Title:   title,
		Message: message,
		Impact:  impact,
		Status:  "new",
	}, fields
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
