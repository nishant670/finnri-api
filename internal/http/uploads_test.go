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

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
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
