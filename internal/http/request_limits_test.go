package http

import (
	"bytes"
	"context"
	"mime/multipart"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
)

func TestAuthRequestBodyLimitReturns413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{cfg: &config.Config{MaxJSONKB: 1, ReqTimeoutSec: 2}}
	router := gin.New()
	router.POST("/v1/auth/guest", jsonRequestLimits(server.cfg), server.authGuest)

	request := httptest.NewRequest(
		nethttp.MethodPost,
		"/v1/auth/guest",
		strings.NewReader(`{"device_id":"`+strings.Repeat("x", 2048)+`"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != nethttp.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "request_body_too_large") {
		t.Fatalf("expected request_body_too_large, got %s", response.Body.String())
	}
}

func TestParseRequestBodyLimitReturns413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{MaxUploadMB: 1, ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: []byte(`{}`)},
	}
	router := gin.New()
	router.POST("/v1/parse", uploadRequestLimits(server.cfg), server.handleParse)

	request := httptest.NewRequest(
		nethttp.MethodPost,
		"/v1/parse",
		strings.NewReader("hint_text="+strings.Repeat("x", 1024*1024+1)),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != nethttp.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "request_body_too_large") {
		t.Fatalf("expected request_body_too_large, got %s", response.Body.String())
	}
}

func TestUploadRequestBodyLimitReturns413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{cfg: &config.Config{MaxUploadMB: 1, ReqTimeoutSec: 2}}
	router := gin.New()
	router.POST("/v1/upload", uploadRequestLimits(server.cfg), server.handleUpload)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(strings.Repeat("x", 1024*1024+1))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(nethttp.MethodPost, "/v1/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != nethttp.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "request_body_too_large") {
		t.Fatalf("expected request_body_too_large, got %s", response.Body.String())
	}
}

func TestRequestLimitAddsTimeoutContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/timeout", requestLimits(0, 1), func(c *gin.Context) {
		if _, ok := c.Request.Context().Deadline(); !ok {
			t.Fatal("expected request deadline")
		}
		c.Status(nethttp.StatusOK)
	})

	request := httptest.NewRequestWithContext(context.Background(), nethttp.MethodGet, "/timeout", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
