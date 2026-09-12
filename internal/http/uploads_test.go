package http

import (
	"bytes"
	"mime/multipart"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func uploadRequest(t *testing.T, filename string, contentType string, body []byte) *nethttp.Request {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)

	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	if contentType != "" {
		header["Content-Type"] = []string{contentType}
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	request := httptest.NewRequest(nethttp.MethodPost, "/v1/upload", buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Host = "finnri.up.railway.app"
	return request
}

// pngBytes is a minimal valid PNG signature plus filler so sniffing succeeds.
func pngBytes() []byte {
	return append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0}, 64)...)
}

func heicBytes() []byte {
	header := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	return append(header, bytes.Repeat([]byte{0}, 64)...)
}

func newUploadServer(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// handleUpload writes relative to the working directory.
	t.Chdir(t.TempDir())

	server := &Server{cfg: &config.Config{MaxUploadMB: 15, ReqTimeoutSec: 30}}
	router := gin.New()
	router.POST("/v1/upload", server.handleUpload)
	return router
}

func TestHandleUploadStoresImageUnderRandomName(t *testing.T) {
	router := newUploadServer(t)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, uploadRequest(t, "receipt.png", "image/png", pngBytes()))

	if response.Code != nethttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}

	entries, err := os.ReadDir(uploadDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one stored file, got %v (err %v)", entries, err)
	}
	stored := entries[0].Name()

	// 16 random bytes hex-encoded plus the extension, and nothing of the
	// caller-supplied name.
	if len(stored) != 32+len(".png") || !strings.HasSuffix(stored, ".png") {
		t.Fatalf("unexpected stored filename %q", stored)
	}
	if strings.Contains(stored, "receipt") {
		t.Fatalf("stored name leaks the client filename: %q", stored)
	}
	if got := response.Body.String(); !strings.Contains(got, "https://finnri.up.railway.app/uploads/"+stored) {
		t.Fatalf("unexpected response body %s", got)
	}
}

func TestHandleUploadAcceptsHEIC(t *testing.T) {
	router := newUploadServer(t)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, uploadRequest(t, "IMG_0001.HEIC", "image/heic", heicBytes()))

	if response.Code != nethttp.StatusOK {
		t.Fatalf("iPhone photos must be accepted, got %d: %s", response.Code, response.Body.String())
	}
	entries, _ := os.ReadDir(uploadDir)
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".heic" {
		t.Fatalf("expected a .heic file, got %v", entries)
	}
}

func TestHandleUploadRejectsHTMLDisguisedAsImage(t *testing.T) {
	router := newUploadServer(t)

	// The declared filename and Content-Type both claim PNG; only the bytes
	// tell the truth. Storing this as .png then serving it would be stored XSS.
	body := []byte("<html><script>alert(document.domain)</script></html>")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, uploadRequest(t, "receipt.png", "image/png", body))

	if response.Code != nethttp.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for HTML content, got %d: %s", response.Code, response.Body.String())
	}
	if entries, _ := os.ReadDir(uploadDir); len(entries) != 0 {
		t.Fatalf("rejected upload must not be written to disk, found %v", entries)
	}
}

func TestHandleUploadRejectsSVG(t *testing.T) {
	router := newUploadServer(t)

	body := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, uploadRequest(t, "logo.svg", "image/svg+xml", body))

	if response.Code != nethttp.StatusUnsupportedMediaType {
		t.Fatalf("SVG executes script and must be rejected, got %d", response.Code)
	}
}

func TestHandleUploadAcceptsPDF(t *testing.T) {
	router := newUploadServer(t)

	body := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 64)...)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, uploadRequest(t, "invoice.pdf", "application/pdf", body))

	if response.Code != nethttp.StatusOK {
		t.Fatalf("expected PDF to be accepted, got %d: %s", response.Code, response.Body.String())
	}
	entries, _ := os.ReadDir(uploadDir)
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".pdf" {
		t.Fatalf("expected a .pdf file, got %v", entries)
	}
}

func TestHandleUploadNamesAreUnique(t *testing.T) {
	router := newUploadServer(t)

	for range 5 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, uploadRequest(t, "same-name.png", "image/png", pngBytes()))
		if response.Code != nethttp.StatusOK {
			t.Fatalf("upload failed: %d", response.Code)
		}
	}

	entries, _ := os.ReadDir(uploadDir)
	if len(entries) != 5 {
		t.Fatalf("identical client filenames must not collide, got %d files", len(entries))
	}
}

func signedUploadTestRouter(t *testing.T) (*gin.Engine, models.User, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())

	user, token := createBillingTestUserSession(t)
	name := strings.Repeat("a", 32) + ".png"
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, name), pngBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := models.Entry{
		UserID: user.ID, Title: "Receipt", Type: "expense", Amount: models.Money(100),
		Currency: "INR", Source: "manual", Date: "2026-09-12",
		Attachment: "https://finnri.up.railway.app/uploads/" + name,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	server := &Server{cfg: &config.Config{}}
	router := gin.New()
	authorized := router.Group("/v1", AuthMiddleware())
	authorized.GET("/uploads/:name/url", server.createSignedUploadURL)
	router.GET("/uploads/:name", server.serveSignedUpload)
	return router, user, token, name
}

func TestKnownUploadPathIsNotWorldReadable(t *testing.T) {
	router, _, _, name := signedUploadTestRouter(t)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(nethttp.MethodGet, "/uploads/"+name, nil))
	if response.Code != nethttp.StatusUnauthorized {
		t.Fatalf("logged-out upload status = %d, want 401", response.Code)
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("protected upload response dropped its hardening headers")
	}
}

func TestOwnerCanExchangeStoredPathForExpiringURL(t *testing.T) {
	router, _, token, name := signedUploadTestRouter(t)
	signed := performJSONRequest[struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}](t, router, nethttp.MethodGet, "/v1/uploads/"+name+"/url", token, nil, nethttp.StatusOK)
	if signed.URL == "" || !signed.ExpiresAt.After(time.Now()) {
		t.Fatalf("invalid signed upload response: %#v", signed)
	}

	request := httptest.NewRequest(nethttp.MethodGet, signed.URL, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != nethttp.StatusOK || !bytes.HasPrefix(response.Body.Bytes(), pngBytes()[:8]) {
		t.Fatalf("signed owner fetch status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestOtherUserCannotSignOwnersUpload(t *testing.T) {
	router, _, _, name := signedUploadTestRouter(t)
	_, otherToken := createBillingTestUserSession(t)
	performJSONRequest[map[string]any](
		t, router, nethttp.MethodGet, "/v1/uploads/"+name+"/url", otherToken, nil, nethttp.StatusNotFound,
	)
}

// splitGroupPhotoTestRouter stores a group photo rather than a receipt. A group
// photo has no owning entry at all, so it exercises the claim that ownership of
// an entry is not the only way to be allowed to see an upload.
func splitGroupPhotoTestRouter(t *testing.T) (*gin.Engine, models.User, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	t.Chdir(t.TempDir())

	owner, ownerToken := createBillingTestUserSession(t)
	name := strings.Repeat("b", 32) + ".png"
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, name), pngBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	group := models.SplitGroup{
		UserID: owner.ID, Name: "Goa Trip", Kind: "trip",
		PhotoURL: "https://finnri.up.railway.app/uploads/" + name,
	}
	if err := database.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}

	server := &Server{cfg: &config.Config{}}
	router := gin.New()
	authorized := router.Group("/v1", AuthMiddleware())
	authorized.GET("/uploads/:name/url", server.createSignedUploadURL)
	router.GET("/uploads/:name", server.serveSignedUpload)
	return router, owner, ownerToken, name
}

func TestGroupOwnerCanDisplayGroupPhoto(t *testing.T) {
	router, _, ownerToken, name := splitGroupPhotoTestRouter(t)
	signed := performJSONRequest[struct {
		URL string `json:"url"`
	}](t, router, nethttp.MethodGet, "/v1/uploads/"+name+"/url", ownerToken, nil, nethttp.StatusOK)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(nethttp.MethodGet, signed.URL, nil))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("owner group-photo fetch status = %d, want 200", response.Code)
	}
}

func TestActiveGroupMemberCanDisplayGroupPhoto(t *testing.T) {
	router, _, _, name := splitGroupPhotoTestRouter(t)
	var group models.SplitGroup
	if err := database.DB.Where("photo_url <> ?", "").First(&group).Error; err != nil {
		t.Fatal(err)
	}
	member, memberToken := createBillingTestUserSession(t)
	if err := database.DB.Create(&models.SplitGroupUserMember{
		GroupID: group.ID, UserID: member.ID, Role: "member", Status: "active",
	}).Error; err != nil {
		t.Fatal(err)
	}

	signed := performJSONRequest[struct {
		URL string `json:"url"`
	}](t, router, nethttp.MethodGet, "/v1/uploads/"+name+"/url", memberToken, nil, nethttp.StatusOK)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(nethttp.MethodGet, signed.URL, nil))
	if response.Code != nethttp.StatusOK {
		t.Fatalf("member group-photo fetch status = %d, want 200", response.Code)
	}
}

func TestNonMemberCannotDisplayGroupPhoto(t *testing.T) {
	router, _, _, name := splitGroupPhotoTestRouter(t)
	_, strangerToken := createBillingTestUserSession(t)
	performJSONRequest[map[string]any](
		t, router, nethttp.MethodGet, "/v1/uploads/"+name+"/url", strangerToken, nil, nethttp.StatusNotFound,
	)
}

// TestUploadBytesRouteSkipsStaticBearer pins the routing half of the fix. The
// signature is the authentication on this route, and the clients that fetch it
// — expo-image, an <img> tag — cannot send a header at all, so gating it on
// AUTH_BEARER made every receipt and group photo a broken image in every
// deployed environment.
func TestUploadBytesRouteSkipsStaticBearer(t *testing.T) {
	if !skipsStaticBearer("/uploads/" + strings.Repeat("a", 32) + ".png") {
		t.Fatal("/uploads/:name is gated by the static bearer; signed URLs can never be fetched")
	}
	if !skipsStaticBearer("/v1/uploads/" + strings.Repeat("a", 32) + ".png/url") {
		t.Fatal("/v1/uploads/:name/url is gated by the static bearer")
	}
	if skipsStaticBearer("/v1/admin/overview") {
		t.Fatal("admin must stay behind the static bearer")
	}
}
