package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"finnri/internal/database"
	"finnri/internal/identity"
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

	// A link pointing at the row that just disappeared would name something the
	// server refuses, which is the very failure the redirect below exists to
	// prevent — so links move with everything else.
	if err := tx.Model(&models.SplitGroupMemberLink{}).
		Where("user_id = ? AND friend_id = ?", ownerID, loserID).
		Update("friend_id", survivorID).Error; err != nil {
		return models.SplitFriend{}, err
	}

	// The merged row's slot no longer exists in any roster it was part of, so
	// every other member's translation for it has to be rebuilt against the
	// survivor.
	for _, membership := range memberships {
		if err := syncSplitGroupMemberLinks(tx, membership.GroupID); err != nil {
			return models.SplitFriend{}, err
		}
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

// splitGroupSlotForFriend renders a member's friend id in slot space.
func splitGroupSlotForFriend(friendID uint) string {
	return strconv.FormatUint(uint64(friendID), 10)
}

// syncSplitGroupMemberLinks makes sure every active member of a shared group
// can name every other person in it.
//
// Run after anything that changes who is in a group — an invite accepted, the
// roster rewritten, a duplicate merged away. It adds only what is missing and
// drops links to slots that no longer exist, so running it twice costs nothing
// and running it on an unshared group costs two queries.
//
// The owner is skipped throughout: the roster is already written in their
// namespace, so every slot resolves for them without a translation.
func syncSplitGroupMemberLinks(tx *gorm.DB, groupID uint) error {
	var group models.SplitGroup
	if err := tx.First(&group, groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	var memberUsers []models.SplitGroupUserMember
	if err := tx.Where("group_id = ? AND status = ?", group.ID, "active").
		Find(&memberUsers).Error; err != nil {
		return err
	}
	if len(memberUsers) == 0 {
		return nil
	}

	var owner models.User
	if err := tx.First(&owner, group.UserID).Error; err != nil {
		return err
	}

	// Scoped to the owner's own membership rows. Anything else in this table is
	// a stray written by a member's expense composer before that path was
	// closed, and provisioning links for it would make the stray permanent.
	var members []models.SplitGroupMember
	if err := tx.Preload("Friend").
		Where("group_id = ? AND user_id = ?", group.ID, group.UserID).
		Find(&members).Error; err != nil {
		return err
	}

	for _, memberUser := range memberUsers {
		if memberUser.UserID == group.UserID {
			continue
		}
		if err := syncSplitGroupMemberLinksForUser(tx, group, owner, members, memberUser.UserID); err != nil {
			return err
		}
	}
	return nil
}

func syncSplitGroupMemberLinksForUser(
	tx *gorm.DB,
	group models.SplitGroup,
	owner models.User,
	members []models.SplitGroupMember,
	viewerID uint,
) error {
	// Which slot the viewer occupies. They need no link to it — in their own
	// composer they are "you", and offering them a row for themselves is the
	// bug this whole table exists to fix.
	selfSlot := ""
	for index := range members {
		friend := members[index].Friend
		if friend.LinkedUserID != nil && *friend.LinkedUserID == viewerID {
			selfSlot = splitGroupSlotForFriend(members[index].FriendID)
			break
		}
	}

	// slot -> the owner-side person it stands for; nil means the owner, who is
	// a user rather than one of their own friend rows.
	wanted := map[string]*models.SplitFriend{
		models.SplitGroupDefaultSplitOwnerSlot: nil,
	}
	for index := range members {
		friend := members[index].Friend
		if friend.ID == 0 || friend.Archived {
			continue
		}
		if friend.LinkedUserID != nil && *friend.LinkedUserID == viewerID {
			continue
		}
		slot := splitGroupSlotForFriend(members[index].FriendID)
		if slot == selfSlot {
			continue
		}
		wanted[slot] = &members[index].Friend
	}

	var existing []models.SplitGroupMemberLink
	if err := tx.Where("group_id = ? AND user_id = ?", group.ID, viewerID).
		Find(&existing).Error; err != nil {
		return err
	}
	have := map[string]models.SplitGroupMemberLink{}
	for _, link := range existing {
		have[link.Slot] = link
	}

	for slot, person := range wanted {
		if _, ok := have[slot]; ok {
			continue
		}
		friendID, err := localSplitFriendForSlot(tx, viewerID, owner, person)
		if err != nil {
			return err
		}
		if friendID == 0 {
			continue
		}
		if err := tx.Create(&models.SplitGroupMemberLink{
			GroupID:  group.ID,
			Slot:     slot,
			UserID:   viewerID,
			FriendID: friendID,
		}).Error; err != nil {
			return err
		}
	}

	// A slot that is gone — somebody the owner removed from the roster — stops
	// being nameable on a new bill. The friend row itself stays: it carries the
	// history of everything already split with that person.
	for slot, link := range have {
		if _, ok := wanted[slot]; ok {
			continue
		}
		if err := tx.Delete(&models.SplitGroupMemberLink{}, link.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// localSplitFriendForSlot finds, or creates, the row in the viewer's own friend
// list that stands for one person in a shared group.
//
// `person` is the owner-side friend row for a member slot, and nil for the
// owner slot. Matching before creating is the point: the viewer may already
// have a row for this person from splitting with them outside the group, and a
// second row would strand that history on an orphan with a zero balance —
// exactly the duplicate-identity problem the merge tooling exists to clean up.
func localSplitFriendForSlot(
	tx *gorm.DB,
	viewerID uint,
	owner models.User,
	person *models.SplitFriend,
) (uint, error) {
	var (
		name         string
		email        string
		phone        string
		linkedUserID *uint
	)
	if person == nil {
		ownerID := owner.ID
		name = displayNameForUser(owner)
		email = stringFromPointer(owner.Email)
		phone = stringFromPointer(owner.Phone)
		linkedUserID = &ownerID
	} else {
		name = fallbackSplitFriendName(*person)
		email = person.Email
		phone = person.Phone
		if person.LinkedUserID != nil {
			linked := *person.LinkedUserID
			linkedUserID = &linked
		}
	}
	// Never hand somebody a row standing for themselves.
	if linkedUserID != nil && *linkedUserID == viewerID {
		return 0, nil
	}

	owned := func() *gorm.DB {
		return tx.Where("user_id = ? AND archived = ?", viewerID, false)
	}

	// 1. A recorded link is certain.
	if linkedUserID != nil {
		var byLink models.SplitFriend
		err := owned().Where("linked_user_id = ?", *linkedUserID).First(&byLink).Error
		if err == nil {
			return byLink.ID, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
	}

	// 2. Contact details, which is all there is for somebody with no account.
	conditions := []string{}
	args := []any{}
	if trimmed := strings.ToLower(strings.TrimSpace(email)); trimmed != "" {
		conditions = append(conditions, "LOWER(email) = ?")
		args = append(args, trimmed)
	}
	if normalized := identity.NormalizePhone(phone); normalized != "" {
		conditions = append(conditions, "phone_normalized = ?")
		args = append(args, normalized)
	}
	if len(conditions) > 0 {
		var byContact models.SplitFriend
		contactMatch := owned().Where("("+strings.Join(conditions, " OR ")+")", args...)
		// Contact details are supporting evidence, not permission to relink a
		// row already known to represent a different Finnri account.
		if linkedUserID != nil {
			contactMatch = contactMatch.Where("linked_user_id IS NULL OR linked_user_id = ?", *linkedUserID)
		}
		err := contactMatch.First(&byContact).Error
		if err == nil {
			if byContact.LinkedUserID == nil && linkedUserID != nil {
				if err := tx.Model(&byContact).Update("linked_user_id", *linkedUserID).Error; err != nil {
					return 0, err
				}
			}
			return byContact.ID, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
	}

	created := models.SplitFriend{
		UserID:       viewerID,
		Name:         name,
		Email:        email,
		Phone:        phone,
		LinkedUserID: linkedUserID,
	}
	if err := tx.Create(&created).Error; err != nil {
		return 0, err
	}
	return created.ID, nil
}

// splitGroupSlotFriendIDs is the viewer's slot translation for one group.
// Empty for the group's owner, who needs none.
func splitGroupSlotFriendIDs(db *gorm.DB, groupID, viewerID uint) (map[string]uint, error) {
	var links []models.SplitGroupMemberLink
	if err := db.Where("group_id = ? AND user_id = ?", groupID, viewerID).
		Find(&links).Error; err != nil {
		return nil, err
	}
	slots := make(map[string]uint, len(links))
	for _, link := range links {
		slots[link.Slot] = link.FriendID
	}
	return slots, nil
}
