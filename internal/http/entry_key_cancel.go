package http

import (
	"errors"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

var errEntryKeyCancelled = errors.New("entry key cancelled")

// claimEntryKey takes the key's row for the rest of the transaction, creating
// it if need be, and returns it. The upsert changes nothing on a row that
// exists: it is there to lock it, and to wait for whoever holds it.
func claimEntryKey(tx *gorm.DB, userID uint, key string) (models.EntryKeyClaim, error) {
	var claim models.EntryKeyClaim
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "idempotency_key"}},
		DoUpdates: clause.Assignments(map[string]any{"cancelled": gorm.Expr("entry_key_claims.cancelled")}),
	}).Create(&models.EntryKeyClaim{UserID: userID, Key: key}).Error; err != nil {
		return claim, err
	}
	err := tx.Where("user_id = ? AND idempotency_key = ?", userID, key).First(&claim).Error
	return claim, err
}

// cancelEntryKey settles a create the client never heard back about. It answers
// with the entry when the key saved one, and otherwise cancels the key, so a
// create still on its way is refused. Either answer is final, which is what lets
// the client save that money another way without counting it twice.
//
// "Not saved" is a 200, so it can't be mistaken for the 404 an older server
// gives for a route it doesn't have.
func (s *Server) cancelEntryKey(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	key, fields := parseIdempotencyKey(c.Param("key"))
	if key == "" && len(fields) == 0 {
		fields = map[string]string{"idempotency_key": "is required"}
	}
	if len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": fields})
		return
	}
	var saved *models.Entry
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		// First the row, so a create holding it has finished before we look.
		if _, err := claimEntryKey(tx, userID, key); err != nil {
			return err
		}
		var entry models.Entry
		err := ownedEntries(tx, userID).Preload("Account").
			Where("idempotency_key = ?", key).First(&entry).Error
		if err == nil {
			saved = &entry
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Model(&models.EntryKeyClaim{}).
			Where("user_id = ? AND idempotency_key = ?", userID, key).
			Update("cancelled", true).Error
	})
	if err != nil {
		c.JSON(500, gin.H{"error": "entry_key_cancel_failed"})
		return
	}
	if saved != nil {
		c.JSON(200, gin.H{"saved": true, "cancelled": false, "entry": saved})
		return
	}
	c.JSON(200, gin.H{"saved": false, "cancelled": true})
}
