package http

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// storedFeedbackUpload puts a file where POST /v1/upload would have, under the
// random name it would have given it, and returns the URL the app would send.
func storedFeedbackUpload(t *testing.T, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return "https://finnri.example/" + uploadDir + "/" + name
}

func feedbackGuestToken(t *testing.T, router *gin.Engine, device string) string {
	t.Helper()
	return performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": device}, http.StatusOK,
	).Token
}

func feedbackWith(attachments []string) map[string]any {
	return map[string]any{
		"type":        "improvement",
		"area":        "Other",
		"title":       "Show AI credits purchase popup",
		"message":     "When I run out of credits, offer to buy more right there.",
		"impact":      "medium",
		"attachments": attachments,
	}
}

// The report: feedback was words only, so a user could not show the screen
// they meant. An attached screenshot has to be kept and reach the team.
func TestFeedbackKeepsAScreenshotAndAdminsCanOpenIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	t.Chdir(t.TempDir())
	token := feedbackGuestToken(t, router, "feedback-attachment-device")
	_, adminToken := createAdminSessionForTest(t, models.AdminRoleViewer)

	screenshot := storedFeedbackUpload(t, "0123456789abcdef0123456789abcdef.png", pngBytes())
	feedback := performJSONRequest[models.Feedback](
		t, router, http.MethodPost, "/v1/feedback", token, feedbackWith([]string{screenshot}), http.StatusCreated,
	)
	if len(feedback.Attachments) != 1 || feedback.Attachments[0] != screenshot {
		t.Fatalf("the screenshot must be kept: %#v", feedback.Attachments)
	}

	response := adminRequest(t, router, http.MethodGet,
		fmt.Sprintf("/v1/admin/feedback/%d/attachments/0", feedback.ID),
		map[string]string{"Authorization": "Bearer " + adminToken})
	if response.Code != http.StatusOK {
		t.Fatalf("an admin should be able to open the screenshot, got %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(response.Body.Bytes(), pngBytes()) {
		t.Fatal("the admin route served something other than the stored file")
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("attachment served without its safety headers: %#v", response.Header())
	}

	// Only the files the feedback names, by position.
	missing := adminRequest(t, router, http.MethodGet,
		fmt.Sprintf("/v1/admin/feedback/%d/attachments/1", feedback.ID),
		map[string]string{"Authorization": "Bearer " + adminToken})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("an index past the end must be 404, got %d", missing.Code)
	}
}

func TestFeedbackAttachmentIsNotOpenToTheUserSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	t.Chdir(t.TempDir())
	token := feedbackGuestToken(t, router, "feedback-attachment-user-device")

	screenshot := storedFeedbackUpload(t, "fedcba9876543210fedcba9876543210.png", pngBytes())
	feedback := performJSONRequest[models.Feedback](
		t, router, http.MethodPost, "/v1/feedback", token, feedbackWith([]string{screenshot}), http.StatusCreated,
	)

	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/admin/feedback/%d/attachments/0", feedback.ID), nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("a user session must not reach the admin attachment route")
	}
}

func TestFeedbackRejectsAttachmentsFinnriDidNotStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	token := feedbackGuestToken(t, router, "feedback-foreign-attachment-device")

	for _, attachment := range []string{
		"https://example.com/cat.png",
		"https://finnri.example/uploads/../etc/passwd",
		"https://finnri.example/uploads/receipt.png",
	} {
		response := performJSONRequest[map[string]any](
			t, router, http.MethodPost, "/v1/feedback", token, feedbackWith([]string{attachment}), http.StatusUnprocessableEntity,
		)
		if fields, _ := response["fields"].(map[string]any); fields["attachments"] == nil {
			t.Fatalf("%q should be refused with an attachments error, got %#v", attachment, response)
		}
	}
}

func TestFeedbackAllowsAtMostThreeAttachments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	token := feedbackGuestToken(t, router, "feedback-many-attachments-device")

	four := []string{}
	for i := 0; i < 4; i++ {
		four = append(four, fmt.Sprintf("https://finnri.example/uploads/%032x.png", i))
	}
	response := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/feedback", token, feedbackWith(four), http.StatusUnprocessableEntity,
	)
	if fields, _ := response["fields"].(map[string]any); fields["attachments"] == nil {
		t.Fatalf("four attachments should be refused, got %#v", response)
	}
}

func TestFeedbackWithoutAttachmentsStillWorks(t *testing.T) {
	// Every app build before this one.
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	token := feedbackGuestToken(t, router, "feedback-no-attachment-device")

	payload := feedbackWith(nil)
	delete(payload, "attachments")
	feedback := performJSONRequest[models.Feedback](t, router, http.MethodPost, "/v1/feedback", token, payload, http.StatusCreated)
	if len(feedback.Attachments) != 0 {
		t.Fatalf("expected no attachments, got %#v", feedback.Attachments)
	}
}
