package http

import (
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// A stored file has no owner of its own: the records naming it decide who may
// read it and how long it lives. These are the four places a record can name one
// — an entry, a split bill, a group photo and a feedback report. A new one has
// to be added to uploadReferenceOwners, or its files can be adopted by another
// user and removed while it still shows them.

// uploadReferenceOwners returns the owner of every record that names this file.
// Each query narrows in SQL and then compares exactly in Go.
func uploadReferenceOwners(name string) ([]uint, error) {
	pattern := "%" + name + "%"
	var owners []uint
	for _, source := range []struct {
		model  any
		column string
	}{
		{&models.Entry{}, "attachment"},
		{&models.SplitBill{}, "attachment"},
		{&models.SplitGroup{}, "photo_url"},
	} {
		var rows []struct {
			UserID uint
			Value  string
		}
		if err := database.DB.Model(source.model).
			Select("user_id, "+source.column+" AS value").
			Where(source.column+" LIKE ?", pattern).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			if namesUpload(row.Value, name) {
				owners = append(owners, row.UserID)
			}
		}
	}

	var reports []models.Feedback
	if err := database.DB.Select("user_id", "attachments").
		Where("CAST(attachments AS TEXT) LIKE ?", pattern).
		Find(&reports).Error; err != nil {
		return nil, err
	}
	for _, report := range reports {
		if slices.ContainsFunc(report.Attachments, func(value string) bool { return namesUpload(value, name) }) {
			owners = append(owners, report.UserID)
		}
	}
	return owners, nil
}

func namesUpload(value, name string) bool {
	path, ok := localUploadPathFromAttachment(value)
	return ok && filepath.Base(path) == name
}

// userMayAttach answers whether this user may put this value on a record.
//
// Seeing a file's name is not owning it: a group member sees the name of a
// shared receipt, and naming it on a record of their own would keep their access
// after they left. So a file is theirs to use when a record of theirs already
// names it, or when no record does, which is what a fresh upload looks like.
// Values that are not files on this server grant nothing and pass through.
func userMayAttach(userID uint, value string) (bool, error) {
	path, ok := localUploadPathFromAttachment(value)
	if !ok {
		return true, nil
	}
	base := filepath.Base(path)
	name, ok := safeUploadName(strings.ToLower(base))
	if !ok {
		return true, nil
	}
	// Only the spelling the server issued. A capital one opens the same file on
	// a disk that ignores case, and an escaped one is invisible to the LIKE
	// searches above.
	if base != name || !strings.Contains(value, name) {
		return false, nil
	}
	owners, err := uploadReferenceOwners(name)
	if err != nil {
		return false, err
	}
	return len(owners) == 0 || slices.Contains(owners, userID), nil
}

// ensureAttachable writes the error response and returns false when any of the
// values is not this user's to use.
func ensureAttachable(c *gin.Context, userID uint, errorCode, field string, values ...string) bool {
	for _, value := range values {
		allowed, err := userMayAttach(userID, value)
		if err != nil {
			log.Printf("failed_attachment_lookup err=%v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "attachment_lookup_failed"})
			return false
		}
		if !allowed {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": errorCode, "fields": gin.H{field: "must be a file you uploaded"}})
			return false
		}
	}
	return true
}

// optionalValue lets a field the request omitted pass the check.
func optionalValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// isRespelledUpload reports a name that differs from one the server issues only
// by capitals. Removing it could remove that other file.
func isRespelledUpload(name string) bool {
	lower := strings.ToLower(name)
	_, ok := safeUploadName(lower)
	return ok && lower != name
}

// entryFiles lists the files named by these entries and by the split bills filed
// against them. Collect before the change that may drop them. A failed lookup
// is logged and costs at most a file left behind; inside a Postgres transaction
// it also fails that transaction, so nothing is removed.
func entryFiles(db *gorm.DB, userID uint, entryIDs ...uint) []string {
	if len(entryIDs) == 0 {
		return nil
	}
	var files []string
	if err := db.Model(&models.Entry{}).
		Where("user_id = ? AND id IN ? AND attachment <> ?", userID, entryIDs, "").
		Pluck("attachment", &files).Error; err != nil {
		log.Printf("failed_attachment_lookup stage=entries err=%v", err)
	}
	var billFiles []string
	if err := db.Model(&models.SplitBill{}).
		Where("user_id = ? AND entry_id IN ? AND attachment <> ?", userID, entryIDs, "").
		Pluck("attachment", &billFiles).Error; err != nil {
		log.Printf("failed_attachment_lookup stage=split_bills err=%v", err)
	}
	return append(files, billFiles...)
}

// deleteDroppedFiles is deleteLocalUploadFiles for an edit: a value the record
// still carries needs no lookup.
func deleteDroppedFiles(before []string, kept string) {
	deleteLocalUploadFiles(slices.DeleteFunc(before, func(value string) bool { return value == kept }))
}
