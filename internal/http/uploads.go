package http

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// uploadDir is the on-disk root for receipts, and the URL path they are served
// back under. Deployments mount a persistent volume here; without one the files
// vanish on redeploy.
//
// Deliberately one constant for both. Splitting the disk location from the
// public path would mean two things that must agree and no compiler to make
// them, and every read, write and delete below would have to remember which of
// the two it wanted.
const uploadDir = "uploads"

// EnsureUploadStorage fails loudly at boot when receipts have nowhere to go.
//
// This used to be discovered by a user: a volume mounted at the upload path
// arrives owned by root and shadows the image's own directory, so a server
// running unprivileged could not write into it — and the only symptom was a
// 500 and "Failed to save file" under a receipt somebody had just chosen. A
// storage problem should be visible in the deploy log, not in an attachment
// that silently refuses to save weeks later.
func EnsureUploadStorage() {
	absolute, err := filepath.Abs(uploadDir)
	if err != nil {
		absolute = uploadDir
	}
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Printf("upload_storage_unusable path=%s err=%v (receipts cannot be saved; "+
			"check the volume mounted here is writable by the server's user)", absolute, err)
		return
	}
	// Creating a file is the only honest test. A directory can be present,
	// listable and still refuse writes, which is precisely the failure mode
	// this exists to catch.
	probe := filepath.Join(uploadDir, ".write-probe")
	file, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("upload_storage_not_writable path=%s err=%v (receipts cannot be saved; "+
			"check the volume mounted here is writable by the server's user)", absolute, err)
		return
	}
	_ = file.Close()
	_ = os.Remove(probe)
	log.Printf("upload_storage_ready path=%s", absolute)
}

// sniffLength is what http.DetectContentType reads, and is also enough to cover
// the ISO base media file format header used by HEIC.
const sniffLength = 512

const signedUploadTTL = 5 * time.Minute

// allowedUploadTypes maps an accepted media type to the extension we store it
// under. The extension is always derived from the sniffed bytes, never from the
// client-supplied filename, so a file cannot be served back as something that
// executes in a browser.
var allowedUploadTypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"application/pdf": ".pdf",
}

// heicBrands are the ISO-BMFF major brands that indicate HEIC/HEIF payloads.
// http.DetectContentType does not recognise them and reports octet-stream.
var heicBrands = map[string]bool{
	"heic": true,
	"heix": true,
	"hevc": true,
	"hevx": true,
	"mif1": true,
	"msf1": true,
}

func (s *Server) handleUpload(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file provided"})
		return
	}
	if file.Size > s.cfg.MaxUploadMB*1024*1024 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large"})
		return
	}

	extension, err := detectUploadExtension(file)
	if err != nil {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error":  "unsupported_file_type",
			"fields": gin.H{"file": "attach a JPEG, PNG, HEIC, WebP image or a PDF"},
		})
		return
	}

	name, err := randomUploadName(extension)
	if err != nil {
		log.Printf("failed_save_upload stage=name err=%v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}

	// Logged with the path, because the two ways this fails in production are a
	// volume the server's user does not own and a volume that is not mounted at
	// all — and the error alone cannot tell them apart.
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Printf("failed_save_upload stage=mkdir path=%s err=%v", uploadDir, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}

	path := filepath.Join(uploadDir, name)
	if err := c.SaveUploadedFile(file, path); err != nil {
		log.Printf("failed_save_upload stage=write path=%s err=%v", path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": fmt.Sprintf("%s/%s/%s", publicOrigin(c.Request), uploadDir, name)})
}

// detectUploadExtension sniffs the leading bytes and returns the extension the
// file may be stored under. The client's declared Content-Type is ignored.
func detectUploadExtension(file *multipart.FileHeader) (string, error) {
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
	header = header[:read]

	if isHEIC(header) {
		return allowedUploadTypes["image/heic"], nil
	}

	mediaType, _, _ := strings.Cut(http.DetectContentType(header), ";")
	extension, ok := allowedUploadTypes[strings.TrimSpace(mediaType)]
	if !ok {
		return "", fmt.Errorf("unsupported upload type %q", mediaType)
	}
	return extension, nil
}

// isHEIC reports whether the bytes carry an ISO-BMFF ftyp box with a HEIC brand,
// which is what iPhone cameras produce by default.
func isHEIC(header []byte) bool {
	if len(header) < 12 {
		return false
	}
	if string(header[4:8]) != "ftyp" {
		return false
	}
	return heicBrands[string(header[8:12])]
}

// randomUploadName returns an unguessable filename as defence in depth. Access
// is independently constrained by the owner-session signature on the read path.
func randomUploadName(extension string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer) + extension, nil
}

func (s *Server) createSignedUploadURL(c *gin.Context) {
	name, ok := safeUploadName(c.Param("name"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload_not_found"})
		return
	}
	userID, ok := c.Get("userID")
	if !ok || !userCanReadUpload(userID.(uint), name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload_not_found"})
		return
	}
	sessionID, ok := c.Get("authSessionID")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_session"})
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_session"})
		return
	}

	expires := time.Now().UTC().Add(signedUploadTTL).Unix()
	signature := uploadSignature(hashSessionToken(token), name, expires)
	url := fmt.Sprintf("%s/%s/%s?sid=%d&expires=%d&signature=%s",
		publicOrigin(c.Request), uploadDir, name, sessionID.(uint), expires, signature)
	c.JSON(http.StatusOK, gin.H{"url": url, "expires_at": time.Unix(expires, 0).UTC()})
}

func (s *Server) serveSignedUpload(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; object-src 'none'; sandbox")
	c.Header("Cache-Control", "private, no-store")

	name, ok := safeUploadName(c.Param("name"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload_not_found"})
		return
	}
	sessionID, err := strconv.ParseUint(c.Query("sid"), 10, 64)
	if err != nil || sessionID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_upload_signature"})
		return
	}
	expires, err := strconv.ParseInt(c.Query("expires"), 10, 64)
	now := time.Now().UTC()
	if err != nil || expires <= now.Unix() || expires > now.Add(signedUploadTTL+time.Minute).Unix() {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "expired_upload_signature"})
		return
	}

	var session models.AuthSession
	if err := database.DB.Where("id = ? AND revoked_at IS NULL AND expires_at > ?", sessionID, now).First(&session).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_upload_signature"})
		return
	}
	expected := uploadSignature(session.TokenHash, name, expires)
	provided, err := hex.DecodeString(c.Query("signature"))
	if err != nil || !hmac.Equal([]byte(expected), []byte(hex.EncodeToString(provided))) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_upload_signature"})
		return
	}
	if !userCanReadUpload(session.UserID, name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload_not_found"})
		return
	}

	path := filepath.Join(uploadDir, name)
	if _, err := os.Stat(path); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload_not_found"})
		return
	}
	c.File(path)
}

func uploadSignature(sessionTokenHash, name string, expires int64) string {
	mac := hmac.New(sha256.New, []byte(sessionTokenHash))
	_, _ = fmt.Fprintf(mac, "%s\n%d", name, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

func safeUploadName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || filepath.Base(name) != name {
		return "", false
	}
	extension := strings.ToLower(filepath.Ext(name))
	allowed := false
	for _, candidate := range allowedUploadTypes {
		if extension == candidate {
			allowed = true
			break
		}
	}
	stem := strings.TrimSuffix(name, extension)
	if !allowed || len(stem) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(stem); err != nil {
		return "", false
	}
	return name, true
}

// userCanReadUpload answers whether this user may see these bytes.
//
// Ownership is not the only claim. A receipt belongs to the one person whose
// entry carries it, but a split group's photo is a property of the group and is
// drawn by every member — so gating uploads on ownership alone turned every
// group photo into a broken image, including for the member who chose it.
func userCanReadUpload(userID uint, name string) bool {
	return userOwnsEntryUpload(userID, name) || userCanReadSplitGroupPhoto(userID, name)
}

func userOwnsEntryUpload(userID uint, name string) bool {
	var attachments []string
	if err := database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND attachment <> ?", userID, "").
		Pluck("attachment", &attachments).Error; err != nil && err != gorm.ErrRecordNotFound {
		return false
	}
	for _, attachment := range attachments {
		path, ok := localUploadPathFromAttachment(attachment)
		if ok && filepath.Base(path) == name {
			return true
		}
	}
	return false
}

// userCanReadSplitGroupPhoto matches the photo to a group and then asks the
// same question the group screens ask — owner, or an active member.
func userCanReadSplitGroupPhoto(userID uint, name string) bool {
	var groups []models.SplitGroup
	if err := database.DB.
		Where("photo_url <> ?", "").
		Find(&groups).Error; err != nil && err != gorm.ErrRecordNotFound {
		return false
	}
	for _, group := range groups {
		path, ok := localUploadPathFromAttachment(group.PhotoURL)
		if !ok || filepath.Base(path) != name {
			continue
		}
		allowed, err := viewerCanAccessSplitGroup(database.DB, group, userID)
		if err == nil && allowed {
			return true
		}
	}
	return false
}
