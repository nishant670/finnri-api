package http

import (
	"net/http"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestCreateFeedbackPersistsAuthenticatedSuggestion(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "feedback-test-device",
		}, http.StatusOK,
	)

	feedback := performJSONRequest[models.Feedback](
		t, router, http.MethodPost, "/v1/feedback", authResponse.Token, map[string]string{
			"type":    "feature_request",
			"area":    "Insights",
			"title":   "Weekly summary",
			"message": "A weekly summary would help me understand where my money went.",
			"impact":  "high",
		}, http.StatusCreated,
	)

	if feedback.ID == 0 || feedback.UserID == 0 {
		t.Fatalf("expected persisted feedback with ids, got %#v", feedback)
	}
	if feedback.Type != "feature_request" || feedback.Area != "Insights" || feedback.Impact != "high" || feedback.Status != "new" {
		t.Fatalf("unexpected feedback payload: %#v", feedback)
	}

	var count int64
	if err := database.DB.Model(&models.Feedback{}).Where("user_id = ? AND title = ?", feedback.UserID, "Weekly summary").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("feedback row count = %d, want 1", count)
	}
}

func TestCreateFeedbackValidatesRequiredMessage(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "feedback-validation-device",
		}, http.StatusOK,
	)

	response := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/feedback", authResponse.Token, map[string]string{
			"type":  "bug",
			"title": "Broken flow",
		}, http.StatusUnprocessableEntity,
	)
	if response["error"] != "invalid_feedback" {
		t.Fatalf("unexpected validation response: %#v", response)
	}
}
