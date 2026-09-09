package http

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm/clause"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

const adminSessionTTL = 8 * time.Hour
const adminContextKey = "adminUser"
const adminSessionKind = "admin"

// adminStaticActorID is the audit identity for the configured machine token.
// It is deliberately not a real user id: attributing a script's action to a
// named human is worse than recording that no human performed it.
const adminStaticActorID = uint(0)

func BootstrapAdminUsers(cfg *config.Config) error {
	for _, userID := range cfg.AdminBootstrapUserIDs {
		var count int64
		if err := database.DB.Model(&models.User{}).Where("id = ? AND is_guest = ?", userID, false).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("ADMIN_BOOTSTRAP_USER_IDS contains missing or guest user %d", userID)
		}
		row := models.AdminUser{UserID: userID, Role: models.AdminRoleOwner, CreatedBy: "bootstrap"}
		if err := database.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{"role": models.AdminRoleOwner, "disabled_at": nil, "updated_at": time.Now().UTC()}),
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func issueAdminSession(userID uint) (string, time.Time, error) {
	token := generateSessionToken()
	expiresAt := time.Now().UTC().Add(adminSessionTTL)
	session := models.AuthSession{UserID: userID, TokenHash: hashSessionToken(token), ExpiresAt: expiresAt, Kind: adminSessionKind}
	if err := database.DB.Create(&session).Error; err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (s *Server) adminLogin(c *gin.Context) {
	var input struct {
		Email string `json:"email" binding:"required"`
		PIN   string `json:"pin" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || !validPINFormat(input.PIN) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}

	var user models.User
	if err := database.DB.Where("LOWER(email) = ? AND is_guest = ?", strings.ToLower(strings.TrimSpace(input.Email)), false).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}
	now := time.Now().UTC()
	if err := clearExpiredLoginLock(&user, now); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_login_lock"})
		return
	}
	if user.LoginLockedUntil != nil && user.LoginLockedUntil.After(now) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "login_locked", "locked_until": user.LoginLockedUntil})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PinHash), []byte(input.PIN)) != nil {
		remaining, lockedUntil, err := recordFailedLoginAttempt(&user, now)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_login_attempts"})
			return
		}
		if lockedUntil != nil {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "login_locked", "locked_until": lockedUntil})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials", "attempts_remaining": remaining})
		return
	}

	var admin models.AdminUser
	if err := database.DB.Preload("User").Where("user_id = ? AND disabled_at IS NULL", user.ID).First(&admin).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin_access_denied"})
		return
	}
	if err := resetLoginLock(&user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_login_lock"})
		return
	}
	token, expiresAt, err := issueAdminSession(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "expires_at": expiresAt, "user": safeAdminIdentity(user), "role": admin.Role})
}

func (s *Server) adminLogout(c *gin.Context) {
	token := adminSessionToken(c)
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "admin_unauthorized"})
		return
	}
	now := time.Now().UTC()
	result := database.DB.Model(&models.AuthSession{}).
		Where("token_hash = ? AND revoked_at IS NULL", hashSessionToken(token)).
		Update("revoked_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_revoke_session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logged_out": true})
}

func (s *Server) requireAdminSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Machine access for trusted scripts is opt-in: it needs ADMIN_STATIC_TOKEN
		// to be configured AND to arrive in its own header.
		//
		// It used to key off `Authorization == AUTH_BEARER` with no X-Admin-Session,
		// which granted owner to every unauthenticated visitor. /v1/admin sits behind
		// the outer static-bearer guard, so the BFF must send AUTH_BEARER on every
		// request; a browser with no admin cookie therefore matched that branch
		// exactly and the login page decided nothing.
		if configured := strings.TrimSpace(s.cfg.AdminStaticToken); configured != "" {
			if presented := strings.TrimSpace(c.GetHeader("X-Admin-Token")); presented != "" {
				if subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) != 1 {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin_unauthorized"})
					return
				}
				c.Set(adminContextKey, &models.AdminUser{Role: models.AdminRoleOwner, CreatedBy: "admin_static_token"})
				c.Next()
				return
			}
		}
		token := adminSessionToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin_unauthorized"})
			return
		}
		var session models.AuthSession
		if err := database.DB.Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ? AND kind = ?", hashSessionToken(token), time.Now().UTC(), adminSessionKind).First(&session).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin_unauthorized"})
			return
		}
		var admin models.AdminUser
		if err := database.DB.Preload("User").Where("user_id = ? AND disabled_at IS NULL", session.UserID).First(&admin).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin_access_denied"})
			return
		}
		c.Set(adminContextKey, &admin)
		c.Set("userID", admin.UserID)
		c.Next()
	}
}

// Kept as a compatibility name for older tests and trusted integrations.
func (s *Server) requireAdminBearer() gin.HandlerFunc { return s.requireAdminSession() }

func adminSessionToken(c *gin.Context) string {
	if token := strings.TrimSpace(c.GetHeader("X-Admin-Session")); token != "" {
		return token
	}
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) == 2 && parts[0] == "Bearer" {
		return parts[1]
	}
	return ""
}

func currentAdmin(c *gin.Context) *models.AdminUser {
	admin, _ := c.Get(adminContextKey)
	row, _ := admin.(*models.AdminUser)
	return row
}

func requireAdminRole(minimum string) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin := currentAdmin(c)
		if admin == nil || adminRoleRank(admin.Role) < adminRoleRank(minimum) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin_role_required", "required_role": minimum})
			return
		}
		c.Next()
	}
}

func adminRoleRank(role string) int {
	switch role {
	case models.AdminRoleOwner:
		return 3
	case models.AdminRoleSupport:
		return 2
	case models.AdminRoleViewer:
		return 1
	default:
		return 0
	}
}

func safeAdminIdentity(user models.User) gin.H {
	return gin.H{"id": user.ID, "uuid": user.UUID, "username": user.Username, "email": user.Email, "profile_image": user.ProfileImage}
}

func (s *Server) getAdminMe(c *gin.Context) {
	admin := currentAdmin(c)
	if admin == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "admin_unauthorized"})
		return
	}
	if admin.UserID == 0 {
		c.JSON(http.StatusOK, gin.H{"user": gin.H{"id": 0, "username": "machine-token"}, "role": models.AdminRoleOwner, "machine": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": safeAdminIdentity(admin.User), "role": admin.Role})
}

func (s *Server) adminAuditMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Request.Method == http.MethodGet || c.Writer.Status() >= 400 {
			return
		}
		adminUserID, actor := auditActor(c)
		payload, _ := json.Marshal(gin.H{"method": c.Request.Method, "path": c.FullPath()})
		entry := models.AdminAuditLog{
			AdminUserID: adminUserID,
			Actor:       actor,
			Action:      auditAction(c),
			SubjectType: strings.Trim(strings.Split(strings.TrimPrefix(c.FullPath(), "/v1/admin/"), "/")[0], " "),
			SubjectID:   firstNonEmpty(c.Param("id"), c.Param("code")),
			Payload:     string(payload),
			IPHash:      s.hashAdminIP(c.ClientIP()),
		}
		_ = database.DB.Create(&entry).Error
	}
}

// auditActor names who performed the request. A machine-token action records a
// null admin id rather than borrowing a real owner's identity.
func auditActor(c *gin.Context) (*uint, string) {
	admin := currentAdmin(c)
	if admin == nil {
		return nil, "unknown"
	}
	if admin.UserID == adminStaticActorID {
		return nil, "admin_static_token"
	}
	userID := admin.UserID
	return &userID, "admin_user"
}

func auditAction(c *gin.Context) string {
	route := strings.TrimPrefix(c.FullPath(), "/v1/admin/")
	route = strings.NewReplacer("/:id", "", "/:code", "", "/", "_").Replace(route)
	return strings.ToLower(c.Request.Method) + "_" + route
}

func (s *Server) hashAdminIP(ip string) string {
	sum := sha256.Sum256([]byte(s.cfg.AdminAuditSalt + ":" + ip))
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseAdminID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return 0, false
	}
	return uint(id), true
}
