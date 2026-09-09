package http

import (
	"net/http"
	"strings"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

type pushDeviceInput struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

func (s *Server) registerPushDevice(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input pushDeviceInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	token := strings.TrimSpace(input.Token)
	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	if (!strings.HasPrefix(token, "ExponentPushToken[") && !strings.HasPrefix(token, "ExpoPushToken[")) || !strings.HasSuffix(token, "]") {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_push_device", "fields": gin.H{"token": "must be an Expo push token"}})
		return
	}
	if platform != "ios" && platform != "android" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_push_device", "fields": gin.H{"platform": "must be ios or android"}})
		return
	}
	device := models.PushDevice{UserID: userID, Token: token, Platform: platform, Active: true}
	if err := database.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "token"}},
		DoUpdates: clause.Assignments(map[string]any{"user_id": userID, "platform": platform, "active": true}),
	}).Create(&device).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_register_push_device"})
		return
	}
	if err := database.DB.Where("token = ?", token).First(&device).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_push_device"})
		return
	}
	c.JSON(http.StatusOK, device)
}

func (s *Server) unregisterPushDevice(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input pushDeviceInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	result := database.DB.Model(&models.PushDevice{}).
		Where("user_id = ? AND token = ?", userID, strings.TrimSpace(input.Token)).
		Update("active", false)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_unregister_push_device"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": result.RowsAffected})
}
