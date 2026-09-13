package http

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	defaultNotificationPageSize = 25
	maxNotificationPageSize     = 100
)

func createNotification(userID uint, notificationType, title, body, actionURL string) error {
	notification := models.Notification{
		UserID:    userID,
		Type:      strings.TrimSpace(notificationType),
		Title:     strings.TrimSpace(title),
		Body:      strings.TrimSpace(body),
		ActionURL: strings.TrimSpace(actionURL),
	}
	if notification.Type == "" || notification.Title == "" || notification.Body == "" {
		return fmt.Errorf("notification requires type, title, and body")
	}
	return database.DB.Create(&notification).Error
}

func parseNotificationPagination(pageParam, pageSizeParam string) (int, int, gin.H) {
	page := 1
	pageSize := defaultNotificationPageSize
	fields := gin.H{}

	if pageParam != "" {
		parsed, err := strconv.Atoi(pageParam)
		if err != nil || parsed < 1 {
			fields["page"] = "must be a positive integer"
		} else {
			page = parsed
		}
	}
	if pageSizeParam != "" {
		parsed, err := strconv.Atoi(pageSizeParam)
		if err != nil || parsed < 1 || parsed > maxNotificationPageSize {
			fields["page_size"] = fmt.Sprintf("must be between 1 and %d", maxNotificationPageSize)
		} else {
			pageSize = parsed
		}
	}
	return page, pageSize, fields
}

// escapeNotificationTypePrefix neutralises the caller's own LIKE wildcards, so
// a prefix can only ever be a prefix.
func escapeNotificationTypePrefix(typePrefix string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(typePrefix)
}

func notificationScope(db *gorm.DB, userID uint) *gorm.DB {
	return db.Where("user_id = ?", userID)
}

func unreadNotificationCount(userID uint) (int64, error) {
	return unreadNotificationCountByPrefix(userID, "")
}

// unreadNotificationCountByPrefix narrows the count to one family of
// notifications — "split." being the one the app asks for.
//
// The Splits screen needed a way to say "something in here wants you" without
// claiming it every time you yourself added an expense. Activity records
// everything, so a dot driven by activity would never go out; unread
// notifications are only ever written *about* the reader by somebody else, and
// clear when read, which is the signal the dot actually wanted.
func unreadNotificationCountByPrefix(userID uint, typePrefix string) (int64, error) {
	var unreadCount int64
	query := notificationScope(database.DB.Model(&models.Notification{}), userID).
		Where("read_at IS NULL")
	if typePrefix != "" {
		// LIKE, with the caller's own wildcards escaped, so a prefix is only
		// ever a prefix.
		query = query.Where("type LIKE ?", escapeNotificationTypePrefix(typePrefix)+"%")
	}
	err := query.Count(&unreadCount).Error
	return unreadCount, err
}

func (s *Server) listNotifications(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	page, pageSize, fields := parseNotificationPagination(c.Query("page"), c.Query("page_size"))
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": fields})
		return
	}

	query := notificationScope(database.DB.Model(&models.Notification{}), userID)
	switch strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", "all"))) {
	case "", "all":
	case "unread":
		query = query.Where("read_at IS NULL")
	case "read":
		query = query.Where("read_at IS NOT NULL")
	default:
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_filters",
			"fields": gin.H{"status": "must be all, unread, or read"},
		})
		return
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_notifications"})
		return
	}

	var notifications []models.Notification
	if err := query.
		Order("created_at desc").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&notifications).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_notifications"})
		return
	}

	unreadCount, err := unreadNotificationCount(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_unread_notifications"})
		return
	}

	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	c.JSON(http.StatusOK, gin.H{
		"notifications": notifications,
		"page":          page,
		"page_size":     pageSize,
		"total":         total,
		"total_pages":   totalPages,
		"unread_count":  unreadCount,
	})
}

func (s *Server) getUnreadNotificationCount(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	typePrefix := strings.TrimSpace(c.Query("type_prefix"))
	if len(typePrefix) > 40 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_filters",
			"fields": gin.H{"type_prefix": "must be at most 40 characters"},
		})
		return
	}
	unreadCount, err := unreadNotificationCountByPrefix(userID, typePrefix)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_unread_notifications"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"unread_count": unreadCount})
}

func (s *Server) markNotificationRead(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	now := time.Now().UTC()
	result := notificationScope(database.DB.Model(&models.Notification{}), userID).
		Where("id = ?", id).
		Where("read_at IS NULL").
		Update("read_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_mark_notification_read"})
		return
	}

	var notification models.Notification
	if err := notificationScope(database.DB, userID).Where("id = ?", id).First(&notification).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification not found"})
		return
	}
	c.JSON(http.StatusOK, notification)
}

func (s *Server) markAllNotificationsRead(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	typePrefix := strings.TrimSpace(c.Query("type_prefix"))
	if len(typePrefix) > 40 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_filters",
			"fields": gin.H{"type_prefix": "must be at most 40 characters"},
		})
		return
	}
	now := time.Now().UTC()
	query := notificationScope(database.DB.Model(&models.Notification{}), userID).
		Where("read_at IS NULL")
	// Narrowed so opening one screen clears only what that screen shows. The
	// Splits activity dot is fed by unread `split.` notifications, and clearing
	// it by marking *everything* read would silently dismiss an unrelated
	// budget alert the user has never opened.
	if typePrefix != "" {
		query = query.Where("type LIKE ?", escapeNotificationTypePrefix(typePrefix)+"%")
	}
	result := query.Update("read_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_mark_notifications_read"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": result.RowsAffected})
}

func (s *Server) deleteNotification(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	result := notificationScope(database.DB, userID).Where("id = ?", id).Delete(&models.Notification{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_notification"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "notification deleted"})
}
