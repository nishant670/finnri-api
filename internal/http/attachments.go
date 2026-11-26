package http

import (
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

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
	name, ok := safeUploadName(filepath.Base(path))
	if !ok {
		return true, nil
	}
	// An escaped spelling would be invisible to the LIKE searches above. The
	// server never issues one.
	if !strings.Contains(value, name) {
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
