package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var errSplitFriendMergeConflict = errors.New("split_friend_merge_conflict")

// mergeSplitFriendsTx moves every reference from loser onto survivor and then
// archives loser. Keeping the survivor's id preserves the row the owner
// originally created while the invite-time duplicate disappears without
// losing bills, settlements, memberships, or group defaults.
func mergeSplitFriendsTx(tx *gorm.DB, ownerID, loserID, survivorID uint) (models.SplitFriend, error) {
	if loserID == survivorID {
		return models.SplitFriend{}, errSplitFriendMergeConflict
	}

	var loser, survivor models.SplitFriend
	if err := tx.Where("id = ? AND user_id = ?", loserID, ownerID).First(&loser).Error; err != nil {
		return models.SplitFriend{}, err
	}
	if err := tx.Where("id = ? AND user_id = ? AND archived = ?", survivorID, ownerID, false).First(&survivor).Error; err != nil {
		return models.SplitFriend{}, err
	}
	if loser.LinkedUserID != nil && survivor.LinkedUserID != nil && *loser.LinkedUserID != *survivor.LinkedUserID {
		return models.SplitFriend{}, errSplitFriendMergeConflict
	}

	var memberships []models.SplitGroupMember
	if err := tx.Where("user_id = ? AND friend_id = ?", ownerID, loserID).Find(&memberships).Error; err != nil {
		return models.SplitFriend{}, err
	}
	for _, membership := range memberships {
		var existing int64
		if err := tx.Model(&models.SplitGroupMember{}).
			Where("user_id = ? AND group_id = ? AND friend_id = ?", ownerID, membership.GroupID, survivorID).
			Count(&existing).Error; err != nil {
			return models.SplitFriend{}, err
		}
		if existing > 0 {
			if err := tx.Delete(&membership).Error; err != nil {
				return models.SplitFriend{}, err
			}
		} else if err := tx.Model(&membership).Update("friend_id", survivorID).Error; err != nil {
			return models.SplitFriend{}, err
		}
	}

	if err := tx.Model(&models.SplitParticipant{}).
		Where("user_id = ? AND friend_id = ?", ownerID, loserID).
		Update("friend_id", survivorID).Error; err != nil {
		return models.SplitFriend{}, err
	}
	if err := tx.Model(&models.SplitSettlement{}).
		Where("user_id = ? AND friend_id = ?", ownerID, loserID).
		Update("friend_id", survivorID).Error; err != nil {
		return models.SplitFriend{}, err
	}
	if err := tx.Model(&models.SplitGroupDirectInvite{}).
		Where("user_id = ? AND friend_id = ?", ownerID, loserID).
		Update("friend_id", survivorID).Error; err != nil {
		return models.SplitFriend{}, err
	}

	var groups []models.SplitGroup
	if err := tx.Where("user_id = ? AND default_split IS NOT NULL", ownerID).Find(&groups).Error; err != nil {
		return models.SplitFriend{}, err
	}
	loserSlot := strconv.FormatUint(uint64(loserID), 10)
	survivorSlot := strconv.FormatUint(uint64(survivorID), 10)
	for _, group := range groups {
		if group.DefaultSplit == nil {
			continue
		}
		changed := false
		if group.DefaultSplit.Payer == loserSlot {
			group.DefaultSplit.Payer = survivorSlot
			changed = true
		}
		for index := range group.DefaultSplit.Participants {
			if group.DefaultSplit.Participants[index].Slot == loserSlot {
				group.DefaultSplit.Participants[index].Slot = survivorSlot
				changed = true
			}
		}
		if changed {
			if err := tx.Model(&group).Update("default_split", group.DefaultSplit).Error; err != nil {
				return models.SplitFriend{}, err
			}
		}
	}

	if strings.TrimSpace(survivor.Email) == "" {
		survivor.Email = loser.Email
	}
	if strings.TrimSpace(survivor.Phone) == "" {
		survivor.Phone = loser.Phone
	}
	if survivor.LinkedUserID == nil {
		survivor.LinkedUserID = loser.LinkedUserID
	}
	if err := tx.Save(&survivor).Error; err != nil {
		return models.SplitFriend{}, err
	}
	if err := tx.Model(&loser).Updates(map[string]any{
		"archived":       true,
		"linked_user_id": nil,
	}).Error; err != nil {
		return models.SplitFriend{}, err
	}

	// The redirect outlives the row. Everything already holding the loser's id
	// — an expense composer open on another screen, a draft the app has not
	// sent yet — would otherwise be rejected with "must belong to the current
	// user" about somebody visibly in the group, and the row it names no longer
	// exists for the user to go and fix.
	//
	// Any redirect that pointed at the loser now points through it, so the
	// trail never grows a hop per merge and never loops.
	if err := tx.Model(&models.SplitFriendMerge{}).
		Where("user_id = ? AND to_friend_id = ?", ownerID, loserID).
		Update("to_friend_id", survivorID).Error; err != nil {
		return models.SplitFriend{}, err
	}
	redirect := models.SplitFriendMerge{UserID: ownerID, FromFriendID: loserID, ToFriendID: survivorID}
	if err := tx.Where("from_friend_id = ?", loserID).
		Assign(map[string]any{"user_id": ownerID, "to_friend_id": survivorID}).
		FirstOrCreate(&redirect).Error; err != nil {
		return models.SplitFriend{}, err
	}
	return survivor, nil
}

// resolveMergedSplitFriendID follows the merge trail from a friend id the
// caller is holding to the row that absorbed it.
//
// Returns the id unchanged when there is no redirect, which is the common case
// and costs one indexed lookup. The hop limit is defence against a cycle the
// merge path is not supposed to be able to create; hitting it means giving back
// the last id reached rather than spinning.
func resolveMergedSplitFriendID(db *gorm.DB, userID, friendID uint) (uint, error) {
	current := friendID
	for hops := 0; hops < 8; hops++ {
		var redirect models.SplitFriendMerge
		err := db.Where("user_id = ? AND from_friend_id = ?", userID, current).First(&redirect).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return current, nil
		}
		if err != nil {
			return friendID, err
		}
		if redirect.ToFriendID == current {
			return current, nil
		}
		current = redirect.ToFriendID
	}
	return current, nil
}

// reconcileSplitIdentities revisits shared groups after stronger evidence
// arrives. It only auto-merges a single unlinked match; ambiguous evidence is
// left for the owner-facing merge action instead of guessing.
func reconcileSplitIdentities(db *gorm.DB, userID uint) error {
	var user models.User
	if err := db.First(&user, userID).Error; err != nil {
		return err
	}

	groupIDs, err := activeSharedSplitGroupIDs(db, userID)
	if err != nil || len(groupIDs) == 0 {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, groupID := range groupIDs {
			var group models.SplitGroup
			if err := tx.Where("id = ? AND archived = ?", groupID, false).First(&group).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}

			var linked models.SplitFriend
			linkedErr := tx.Where("user_id = ? AND linked_user_id = ? AND archived = ?", group.UserID, user.ID, false).
				First(&linked).Error
			if linkedErr != nil && !errors.Is(linkedErr, gorm.ErrRecordNotFound) {
				return linkedErr
			}

			identityQuery, identityArgs := splitInviteUserIdentityQuery(user)
			if identityQuery == "" {
				continue
			}
			var candidates []models.SplitFriend
			query := tx.Where("user_id = ? AND archived = ? AND linked_user_id IS NULL", group.UserID, false).
				Where("id IN (?)", tx.Model(&models.SplitGroupMember{}).Select("friend_id").Where("group_id = ?", group.ID)).
				Where(identityQuery, identityArgs...)
			if linked.ID != 0 {
				query = query.Where("id <> ?", linked.ID)
			}
			if err := query.Find(&candidates).Error; err != nil {
				return err
			}
			if len(candidates) != 1 {
				continue
			}

			if linked.ID == 0 {
				if err := tx.Model(&candidates[0]).Update("linked_user_id", user.ID).Error; err != nil {
					return err
				}
				continue
			}
			if _, err := mergeSplitFriendsTx(tx, group.UserID, linked.ID, candidates[0].ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Server) mergeSplitFriend(c *gin.Context) {
	ownerID := c.MustGet("userID").(uint)
	loserID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	survivorID, ok := parseUintParam(c, "target")
	if !ok {
		return
	}

	var survivor models.SplitFriend
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		merged, err := mergeSplitFriendsTx(tx, ownerID, loserID, survivorID)
		survivor = merged
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "split_friend_not_found"})
		case errors.Is(err, errSplitFriendMergeConflict):
			c.JSON(http.StatusConflict, gin.H{"error": errSplitFriendMergeConflict.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_merge_split_friend"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"friend":  survivor,
		"message": fmt.Sprintf("Merged into %s", fallbackSplitFriendName(survivor)),
	})
}
