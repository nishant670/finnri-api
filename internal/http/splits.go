package http

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"finnri/internal/database"
	"finnri/internal/identity"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	splitDirectionFriendOwesUser = "friend_owes_user"
	splitDirectionUserOwesFriend = "user_owes_friend"

	settlementDirectionFriendPaidUser = "friend_paid_user"
	settlementDirectionUserPaidFriend = "user_paid_friend"
)

type splitFriendInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

type splitParticipantInput struct {
	FriendID    uint         `json:"friend_id"`
	ShareAmount models.Money `json:"share_amount"`
	Direction   string       `json:"direction"`
}

type splitGroupInput struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// PhotoURL is a URL this server issued from POST /v1/upload. It is nil when
	// the client is not touching the photo, so an app build that does not know
	// about group photos cannot clear one by omission — an empty string is the
	// explicit "remove it".
	PhotoURL  *string `json:"photo_url"`
	FriendIDs []uint  `json:"friend_ids"`
}

type splitGroupDefaultSplitInput struct {
	// A nil DefaultSplit clears the group's default and sends every expense
	// back to the equal split.
	DefaultSplit *splitGroupDefaultSplitBody `json:"default_split"`
}

type splitGroupDefaultSplitBody struct {
	Payer        string                             `json:"payer"`
	FullAmount   bool                               `json:"full_amount"`
	Tab          string                             `json:"tab"`
	Participants []splitGroupDefaultSplitShareInput `json:"participants"`
}

type splitGroupDefaultSplitShareInput struct {
	Slot   string `json:"slot"`
	Weight string `json:"weight"`
}

type splitGroupDirectInviteInput struct {
	Email string `json:"email"`
	Phone string `json:"phone"`
}

type splitBillInput struct {
	EntryID      *uint                   `json:"entry_id"`
	GroupID      *uint                   `json:"group_id"`
	Title        string                  `json:"title"`
	TotalAmount  models.Money            `json:"total_amount"`
	Currency     string                  `json:"currency"`
	Date         string                  `json:"date"`
	Notes        string                  `json:"notes"`
	Participants []splitParticipantInput `json:"participants"`
}

type splitSettlementInput struct {
	FriendID uint `json:"friend_id"`
	// Optional: the group whose expenses this payment closes. Absent for a
	// settlement recorded straight against a friend.
	GroupID   *uint        `json:"group_id"`
	Amount    models.Money `json:"amount"`
	Direction string       `json:"direction"`
	Date      string       `json:"date"`
	Notes     string       `json:"notes"`
}

type splitBalance struct {
	Friend            models.SplitFriend `json:"friend"`
	TotalOwedByFriend models.Money       `json:"total_owed_by_friend"`
	TotalOwedToFriend models.Money       `json:"total_owed_to_friend"`
	NetBalance        models.Money       `json:"net_balance"`
}

type splitActivityItem struct {
	ID               string                    `json:"id"`
	Type             string                    `json:"type"`
	RecordID         uint                      `json:"record_id"`
	Title            string                    `json:"title"`
	Date             string                    `json:"date"`
	Amount           *models.Money             `json:"amount,omitempty"`
	GroupID          *uint                     `json:"group_id,omitempty"`
	Group            *models.SplitGroup        `json:"group,omitempty"`
	FriendID         *uint                     `json:"friend_id,omitempty"`
	Friend           *models.SplitFriend       `json:"friend,omitempty"`
	Direction        string                    `json:"direction,omitempty"`
	ParticipantCount int                       `json:"participant_count,omitempty"`
	Participants     []models.SplitParticipant `json:"participants,omitempty"`
	Notes            string                    `json:"notes,omitempty"`
	// Set only when somebody else recorded this, named the way the viewer names
	// them. A shared group's feed is otherwise silent about who did what.
	ActorName string    `json:"actor_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type splitGroupInviteResponse struct {
	Token     string            `json:"token"`
	URL       string            `json:"url"`
	DeepLink  string            `json:"deep_link"`
	Group     models.SplitGroup `json:"group"`
	ExpiresAt *time.Time        `json:"expires_at"`
}

type splitGroupDirectInviteResponse struct {
	ID               uint              `json:"id"`
	TargetEmail      string            `json:"target_email"`
	TargetPhone      string            `json:"target_phone"`
	MatchedUser      bool              `json:"matched_user"`
	NotificationSent bool              `json:"notification_sent"`
	URL              string            `json:"url"`
	DeepLink         string            `json:"deep_link"`
	Message          string            `json:"message"`
	Status           string            `json:"status"`
	Group            models.SplitGroup `json:"group"`
	CreatedAt        time.Time         `json:"created_at"`
}

// splitPendingInviteResponse is one invite waiting on the signed-in user, which
// is what the in-app prompt needs to ask "join this group?" without a second
// round trip for the group's name or who sent it.
type splitPendingInviteResponse struct {
	ID        uint      `json:"id"`
	Token     string    `json:"token"`
	GroupID   uint      `json:"group_id"`
	GroupName string    `json:"group_name"`
	OwnerName string    `json:"owner_name"`
	CreatedAt time.Time `json:"created_at"`
}

type splitGroupInviteDetailsResponse struct {
	Token       string            `json:"token"`
	Group       models.SplitGroup `json:"group"`
	OwnerName   string            `json:"owner_name"`
	MemberCount int               `json:"member_count"`
	Status      string            `json:"status"`
	ExpiresAt   *time.Time        `json:"expires_at"`
}

// splitGroupInvitePreviewResponse deliberately exposes only the information a
// logged-out recipient needs to decide whether to continue. Returning a
// partially populated SplitGroup would also serialize internal zero-value
// fields and make the public contract much broader than this landing page.
type splitGroupInvitePreviewResponse struct {
	Token       string     `json:"token"`
	GroupID     uint       `json:"group_id"`
	GroupName   string     `json:"group_name"`
	OwnerName   string     `json:"owner_name"`
	MemberCount int        `json:"member_count"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type splitGroupInviteAcceptResponse struct {
	Group  models.SplitGroup       `json:"group"`
	Friend models.SplitFriend      `json:"friend"`
	Member models.SplitGroupMember `json:"member"`
}

func (s *Server) createSplitFriend(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var input splitFriendInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_friend", "fields": fields})
		return
	}

	friend := input.toModel(userID)
	if err := database.DB.Create(&friend).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_friend"})
		return
	}
	c.JSON(http.StatusCreated, friend)
}

func (s *Server) getSplitGroupInvite(c *gin.Context) {
	invite, group, owner, ok := loadActiveSplitGroupInvite(c)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, splitGroupInviteDetailsResponse{
		Token:       invite.Token,
		Group:       group,
		OwnerName:   displayNameForUser(owner),
		MemberCount: len(group.Members) + 1,
		Status:      invite.Status,
		ExpiresAt:   invite.ExpiresAt,
	})
}

func (s *Server) previewSplitGroupInvite(c *gin.Context) {
	invite, group, owner, ok := loadActiveSplitGroupInvite(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, splitGroupInvitePreviewResponse{
		Token:       invite.Token,
		GroupID:     group.ID,
		GroupName:   group.Name,
		OwnerName:   displayNameForUser(owner),
		MemberCount: len(group.Members) + 1,
		Status:      invite.Status,
		ExpiresAt:   invite.ExpiresAt,
	})
}

func (s *Server) acceptSplitGroupInvite(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	_, group, owner, ok := loadActiveSplitGroupInvite(c)
	if !ok {
		return
	}
	if owner.ID == userID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cannot_accept_own_split_group_invite"})
		return
	}

	var user models.User
	if err := database.DB.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user_not_found"})
		return
	}

	var friend models.SplitFriend
	var member models.SplitGroupMember
	var userMember models.SplitGroupUserMember
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		found, err := resolveSplitGroupFriendForUser(tx, owner, group, user)
		if err != nil {
			return err
		}
		if found != nil {
			friend = *found
		}
		if found == nil {
			friend = models.SplitFriend{
				UserID:       owner.ID,
				Name:         displayNameForUser(user),
				Email:        stringFromPointer(user.Email),
				Phone:        stringFromPointer(user.Phone),
				LinkedUserID: &user.ID,
			}
			if err := tx.Create(&friend).Error; err != nil {
				return err
			}
		} else if friend.LinkedUserID == nil || *friend.LinkedUserID != user.ID {
			// The row already existed from before the link was recorded, or was
			// written by hand. Accepting the invite is the moment we know for
			// certain which account stands behind it.
			friend.LinkedUserID = &user.ID
			if err := tx.Save(&friend).Error; err != nil {
				return err
			}
		}

		// The invite has done its job; leaving it pending would keep it in the
		// owner's "pending invites" list after the person is already in.
		if err := tx.Model(&models.SplitGroupDirectInvite{}).
			Where("group_id = ? AND status = ?", group.ID, "pending").
			Where("invited_user_id = ? OR friend_id = ?", user.ID, friend.ID).
			Update("status", "accepted").Error; err != nil {
			return err
		}

		err = tx.Where("user_id = ? AND group_id = ? AND friend_id = ?", owner.ID, group.ID, friend.ID).
			First(&member).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == gorm.ErrRecordNotFound {
			member = models.SplitGroupMember{
				UserID:   owner.ID,
				GroupID:  group.ID,
				FriendID: friend.ID,
			}
			if err := tx.Create(&member).Error; err != nil {
				return err
			}
		}
		err = tx.Where("group_id = ? AND user_id = ?", group.ID, user.ID).
			First(&userMember).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == gorm.ErrRecordNotFound {
			userMember = models.SplitGroupUserMember{
				GroupID: group.ID,
				UserID:  user.ID,
				Role:    "member",
				Status:  "active",
			}
			if err := tx.Create(&userMember).Error; err != nil {
				return err
			}
		} else if userMember.Status != "active" || userMember.Role != "member" {
			userMember.Status = "active"
			userMember.Role = "member"
			if err := tx.Save(&userMember).Error; err != nil {
				return err
			}
		}
		// The arriving member needs a row of their own for everybody already in
		// the group, and everybody already in needs one for them. Without it
		// they can name nobody but themselves — see SplitGroupMemberLink.
		return syncSplitGroupMemberLinks(tx, group.ID)
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_accept_split_group_invite"})
		return
	}

	// Accepting is reading: the invite prompt, the notifications screen and a
	// deep link all land here, and none of them should leave the invitation
	// sitting unread afterwards.
	_ = database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ? AND read_at IS NULL", userID, "split.group_invite.received").
		Where("action_url = ?", fmt.Sprintf("/invite/split/%s", strings.TrimSpace(c.Param("token")))).
		Update("read_at", time.Now()).Error

	_ = createNotification(
		owner.ID,
		"split.group_invite.accepted",
		fmt.Sprintf("%s joined %s", displayNameForUser(user), group.Name),
		fmt.Sprintf("%s accepted your Finnri split group invite.", displayNameForUser(user)),
		fmt.Sprintf("/split/groups/%d", group.ID),
	)

	_ = database.DB.Preload("Members.Friend").First(&group, group.ID).Error
	applySplitGroupViewerPermissions(&group, userID)
	_ = decorateSplitGroupForViewer(database.DB, &group, userID)
	c.JSON(http.StatusOK, splitGroupInviteAcceptResponse{
		Group:  group,
		Friend: friend,
		Member: member,
	})
}

// resolveSplitGroupFriendForUser decides which of the owner's friend rows the
// arriving user already is.
//
// Order matters, strongest evidence first. Guessing wrongly is not a cosmetic
// slip: the owner has usually been splitting against that row for weeks, and a
// miss strands every one of those expenses on an orphan while the person joins
// under a fresh row with a zero balance.
func resolveSplitGroupFriendForUser(
	tx *gorm.DB,
	owner models.User,
	group models.SplitGroup,
	user models.User,
) (*models.SplitFriend, error) {
	ownerFriends := func() *gorm.DB {
		return tx.Where("user_id = ? AND archived = ?", owner.ID, false)
	}

	// 1. A recorded link is certain.
	var linked models.SplitFriend
	err := ownerFriends().Where("linked_user_id = ?", user.ID).First(&linked).Error
	if err == nil {
		return &linked, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	// 2. An invite raised by adding this person to the group names the row
	//    outright — the only signal that survives a friend saved as "Wife" with
	//    no contact details of her own.
	inviteQuery := tx.Where("group_id = ? AND friend_id IS NOT NULL", group.ID)
	if targetQuery, targetArgs := splitInviteTargetIdentityQuery(user); targetQuery != "" {
		// Also matched on the address the invite was sent to, so somebody who
		// signed up after being added still lands on their own row.
		inviteQuery = inviteQuery.Where(
			tx.Where("invited_user_id = ?", user.ID).Or(targetQuery, targetArgs...),
		)
	} else {
		inviteQuery = inviteQuery.Where("invited_user_id = ?", user.ID)
	}
	var directInvite models.SplitGroupDirectInvite
	err = inviteQuery.Order("created_at desc").First(&directInvite).Error
	if err == nil && directInvite.FriendID != nil {
		var invited models.SplitFriend
		if err := ownerFriends().First(&invited, *directInvite.FriendID).Error; err == nil {
			return &invited, nil
		} else if err != gorm.ErrRecordNotFound {
			return nil, err
		}
	} else if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	// 3. The email or phone the owner saved against the row.
	if identityQuery, identityArgs := splitInviteUserIdentityQuery(user); identityQuery != "" {
		var byIdentity models.SplitFriend
		err := ownerFriends().Where(identityQuery, identityArgs...).First(&byIdentity).Error
		if err == nil {
			return &byIdentity, nil
		}
		if err != gorm.ErrRecordNotFound {
			return nil, err
		}
	}

	// 4. Last resort: the name, and only among this group's own members, where
	//    a same-name collision with some unrelated friend cannot happen.
	var byName models.SplitFriend
	err = ownerFriends().
		Where("name = ?", displayNameForUser(user)).
		Where("id IN (?)", tx.Model(&models.SplitGroupMember{}).
			Select("friend_id").
			Where("group_id = ?", group.ID)).
		First(&byName).Error
	if err == nil {
		return &byName, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return nil, nil
}

func loadActiveSplitGroupInvite(c *gin.Context) (models.SplitGroupInvite, models.SplitGroup, models.User, bool) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_invite_token"})
		return models.SplitGroupInvite{}, models.SplitGroup{}, models.User{}, false
	}

	var invite models.SplitGroupInvite
	if err := database.DB.Where("token = ? AND status = ?", token, "active").First(&invite).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_invite_not_found"})
		return models.SplitGroupInvite{}, models.SplitGroup{}, models.User{}, false
	}
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
		c.JSON(http.StatusGone, gin.H{"error": "split_group_invite_expired"})
		return models.SplitGroupInvite{}, models.SplitGroup{}, models.User{}, false
	}

	var group models.SplitGroup
	if err := database.DB.Preload("Members.Friend").
		Where("id = ? AND archived = ?", invite.GroupID, false).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return models.SplitGroupInvite{}, models.SplitGroup{}, models.User{}, false
	}

	var owner models.User
	if err := database.DB.First(&owner, invite.UserID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_owner_not_found"})
		return models.SplitGroupInvite{}, models.SplitGroup{}, models.User{}, false
	}

	return invite, group, owner, true
}

func (s *Server) createSplitGroupInvite(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	webBaseURL := s.webBaseURL()
	if strings.TrimSpace(webBaseURL) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "public_web_url_not_configured"})
		return
	}

	var group models.SplitGroup
	if err := ownedSplitGroups(database.DB.Preload("Members.Friend"), userID).
		Where("id = ?", groupID).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}

	invite, err := getOrCreateActiveSplitGroupInvite(database.DB, userID, group.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_group_invite"})
		return
	}
	url := splitInviteURL(webBaseURL, invite.Token)

	applySplitGroupViewerPermissions(&group, userID)
	c.JSON(http.StatusOK, splitGroupInviteResponse{
		Token:     invite.Token,
		URL:       url,
		DeepLink:  splitInviteDeepLink(invite.Token),
		Group:     group,
		ExpiresAt: invite.ExpiresAt,
	})
}

func (s *Server) createSplitGroupDirectInvite(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	webBaseURL := s.webBaseURL()
	if strings.TrimSpace(webBaseURL) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "public_web_url_not_configured"})
		return
	}

	var input splitGroupDirectInviteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	targetEmail := strings.TrimSpace(input.Email)
	targetPhone := strings.TrimSpace(input.Phone)
	if targetEmail == "" && targetPhone == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group_invite", "fields": gin.H{"email": "email or phone is required"}})
		return
	}
	if targetEmail != "" {
		identifierType, normalized, err := normalizeIdentifier(targetEmail)
		if err != nil || identifierType != "email" || len(normalized) > 254 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group_invite", "fields": gin.H{"email": "must be a valid email"}})
			return
		}
		targetEmail = normalized
	}
	if targetPhone != "" {
		identifierType, normalized, err := normalizeIdentifier(targetPhone)
		if err != nil || identifierType != "phone" || len(normalized) > 32 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group_invite", "fields": gin.H{"phone": "must be a valid phone"}})
			return
		}
		targetPhone = normalized
	}

	var group models.SplitGroup
	if err := ownedSplitGroups(database.DB.Preload("Members.Friend"), userID).
		Where("id = ?", groupID).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}

	var owner models.User
	if err := database.DB.First(&owner, userID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user_not_found"})
		return
	}

	var invitedUser models.User
	var invitedUserID *uint
	matchedUser := false
	userQuery := database.DB
	if targetEmail != "" && targetPhone != "" {
		userQuery = userQuery.Where("LOWER(email) = ? OR phone_normalized = ?", targetEmail, identity.NormalizePhone(targetPhone))
	} else if targetEmail != "" {
		userQuery = userQuery.Where("LOWER(email) = ?", targetEmail)
	} else {
		userQuery = userQuery.Where("phone_normalized = ?", identity.NormalizePhone(targetPhone))
	}
	if err := userQuery.First(&invitedUser).Error; err != nil && err != gorm.ErrRecordNotFound {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_lookup_invited_user"})
		return
	} else if err == nil {
		if invitedUser.ID == userID {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cannot_invite_self_to_split_group"})
			return
		}
		matchedUser = true
		invitedUserID = &invitedUser.ID
	}

	invite, err := getOrCreateActiveSplitGroupInvite(database.DB, userID, group.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_group_invite"})
		return
	}

	directInvite, err := getOrCreateSplitGroupDirectInvite(database.DB, userID, group.ID, invite.ID, targetEmail, targetPhone, invitedUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_group_direct_invite"})
		return
	}

	url := splitInviteURL(webBaseURL, invite.Token)
	deepLink := splitInviteDeepLink(invite.Token)
	message := fmt.Sprintf("%s invited you to join %s on Finnri to track shared expenses: %s", displayNameForUser(owner), group.Name, url)
	notificationSent := false
	if matchedUser {
		if err := createNotification(
			invitedUser.ID,
			"split.group_invite.received",
			fmt.Sprintf("Join %s on Finnri", group.Name),
			fmt.Sprintf("%s invited you to a split group.", displayNameForUser(owner)),
			fmt.Sprintf("/invite/split/%s", invite.Token),
		); err == nil {
			notificationSent = true
		}
	}

	applySplitGroupViewerPermissions(&group, userID)
	response := splitGroupDirectInviteToResponse(directInvite, group, owner, webBaseURL)
	response.MatchedUser = matchedUser
	response.NotificationSent = notificationSent
	response.URL = url
	response.DeepLink = deepLink
	response.Message = message
	c.JSON(http.StatusCreated, response)
}

// listPendingSplitGroupInvites returns the invites addressed to the caller.
//
// The owner-facing list answers "who have I invited"; this answers "who wants me
// in their group", which is the question the app has to ask on every launch to
// stop an invite from living only in a notifications screen nobody opens.
func (s *Server) listPendingSplitGroupInvites(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var invites []models.SplitGroupDirectInvite
	if err := database.DB.
		Preload("Group").
		Preload("Invite").
		Where("invited_user_id = ? AND status = ?", userID, "pending").
		Order("created_at desc").
		Find(&invites).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_pending_split_group_invites"})
		return
	}

	joinedGroupIDs, err := activeSharedSplitGroupIDs(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_pending_split_group_invites"})
		return
	}
	joined := map[uint]bool{}
	for _, groupID := range joinedGroupIDs {
		joined[groupID] = true
	}

	ownerNames := map[uint]string{}
	responses := make([]splitPendingInviteResponse, 0, len(invites))
	seenGroups := map[uint]bool{}
	for _, invite := range invites {
		// An invite is only worth surfacing while it can still do something:
		// the group has to exist, be live, be one the caller has not already
		// joined, and have an active link behind it.
		if invite.Group.ID == 0 || invite.Group.Archived || joined[invite.GroupID] {
			continue
		}
		if invite.Invite.ID == 0 || invite.Invite.Status != "active" || invite.Invite.Token == "" {
			continue
		}
		if invite.Invite.ExpiresAt != nil && invite.Invite.ExpiresAt.Before(time.Now()) {
			continue
		}
		// Two invites for the same group (email and phone) are one question.
		if seenGroups[invite.GroupID] {
			continue
		}
		seenGroups[invite.GroupID] = true

		if _, ok := ownerNames[invite.Group.UserID]; !ok {
			var owner models.User
			if err := database.DB.First(&owner, invite.Group.UserID).Error; err != nil {
				continue
			}
			ownerNames[invite.Group.UserID] = displayNameForUser(owner)
		}

		responses = append(responses, splitPendingInviteResponse{
			ID:        invite.ID,
			Token:     invite.Invite.Token,
			GroupID:   invite.GroupID,
			GroupName: invite.Group.Name,
			OwnerName: ownerNames[invite.Group.UserID],
			CreatedAt: invite.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, responses)
}

func (s *Server) listSplitGroupDirectInvites(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var group models.SplitGroup
	if err := ownedSplitGroups(database.DB, userID).
		Where("id = ? AND archived = ?", groupID, false).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}
	var owner models.User
	if err := database.DB.First(&owner, userID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user_not_found"})
		return
	}

	var invites []models.SplitGroupDirectInvite
	if err := database.DB.
		Preload("Invite").
		Where("user_id = ? AND group_id = ? AND status = ?", userID, group.ID, "pending").
		Order("created_at desc").
		Find(&invites).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_group_invites"})
		return
	}

	responses := make([]splitGroupDirectInviteResponse, 0, len(invites))
	for _, invite := range invites {
		responses = append(responses, splitGroupDirectInviteToResponse(invite, group, owner, s.webBaseURL()))
	}
	c.JSON(http.StatusOK, responses)
}

func (s *Server) revokeSplitGroupDirectInvite(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	inviteID, ok := parseUintParam(c, "invite_id")
	if !ok {
		return
	}

	var group models.SplitGroup
	if err := ownedSplitGroups(database.DB, userID).
		Where("id = ? AND archived = ?", groupID, false).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}

	result := database.DB.Model(&models.SplitGroupDirectInvite{}).
		Where("id = ? AND user_id = ? AND group_id = ? AND status = ?", inviteID, userID, group.ID, "pending").
		Update("status", "revoked")
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_revoke_split_group_invite"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_invite_not_found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "split group invite revoked"})
}

func (s *Server) listSplitFriends(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	query := database.DB.Where("user_id = ?", userID)
	if !strings.EqualFold(c.Query("status"), "all") {
		query = query.Where("archived = ?", false)
	}

	var friends []models.SplitFriend
	if err := query.Order("name asc, created_at desc").Find(&friends).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_friends"})
		return
	}
	c.JSON(http.StatusOK, friends)
}

func (s *Server) updateSplitFriend(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var input splitFriendInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_friend", "fields": fields})
		return
	}

	var friend models.SplitFriend
	if err := ownedSplitFriends(database.DB, userID).Where("id = ?", id).First(&friend).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_friend_not_found"})
		return
	}
	input.apply(&friend)
	if err := database.DB.Save(&friend).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_friend"})
		return
	}
	if matchedUser, err := splitFriendUser(database.DB, friend); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_reconcile_split_friend"})
		return
	} else if matchedUser != nil {
		if err := reconcileSplitIdentities(database.DB, matchedUser.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_reconcile_split_friend"})
			return
		}
		_ = database.DB.First(&friend, friend.ID).Error
	}
	c.JSON(http.StatusOK, friend)
}

// Archiving a friend takes them out of the groups they were in.
//
// It used to set `archived` and stop there, and the `split_group_members` rows
// survived — pointing at a friend `listSplitFriends` will never return again,
// because it filters on that same flag. Every group the person was in went on
// listing a member the app could not resolve to anybody.
//
// That is not a tidiness problem. The expense composer decides who *carries* a
// share from the group's member list and who gets a *row* from the friends
// list, so the two came apart at exactly this point: a split over two visible
// people totalled three people's worth, and the third had nothing on screen to
// edit. It is also how a duplicate gets cleaned up — the whole reason somebody
// archives a friend row is that it turned out to be a second copy of a person
// who is already in the group under another one.
//
// Their bills and participant rows are left alone. `buildSplitBalances` walks
// active friends and never reaches an archived one, so the balance is already
// zero; deleting the history as well would be a different and much larger
// promise than the button makes.
func (s *Server) archiveSplitFriend(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		result := ownedSplitFriends(tx.Model(&models.SplitFriend{}), userID).
			Where("id = ?", id).
			Update("archived", true)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		var groupIDs []uint
		if err := tx.Model(&models.SplitGroupMember{}).
			Where("user_id = ? AND friend_id = ?", userID, id).
			Pluck("group_id", &groupIDs).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND friend_id = ?", userID, id).
			Delete(&models.SplitGroupMember{}).Error; err != nil {
			return err
		}
		// Off the roster means off every member's list of people they can
		// name. The rows their links pointed at stay, carrying the history.
		for _, groupID := range groupIDs {
			if err := syncSplitGroupMemberLinks(tx, groupID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_friend_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_archive_split_friend"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "split friend archived"})
}

func (s *Server) createSplitGroup(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var input splitGroupInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group", "fields": fields})
		return
	}
	if fields, err := validateSplitGroupFriends(userID, input.FriendIDs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	} else if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group", "fields": fields})
		return
	}

	var group models.SplitGroup
	var addedFriendIDs []uint
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		group = models.SplitGroup{
			UserID: userID,
			Name:   strings.TrimSpace(input.Name),
			Kind:   normalizedSplitGroupKind(input.Kind),
		}
		if input.PhotoURL != nil {
			group.PhotoURL, _ = splitGroupPhotoURL(*input.PhotoURL)
		}
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		added, err := createSplitGroupMembers(tx, userID, group.ID, input.FriendIDs)
		addedFriendIDs = added
		return err
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_group"})
		return
	}
	// Invites are raised after the group is committed: a notification that
	// cannot be rolled back must not be sent for a group that never existed.
	memberInvites := inviteSplitGroupMembersForOwner(userID, group, addedFriendIDs)
	_ = database.DB.Preload("Members.Friend").First(&group, group.ID).Error
	applySplitGroupViewerPermissions(&group, userID)
	_ = decorateSplitGroupForViewer(database.DB, &group, userID)
	group.MemberInvites = memberInvites
	c.JSON(http.StatusCreated, group)
}

// backfillSplitGroupMemberLinks provisions slot translations for shared groups
// the viewer joined before those translations existed.
//
// Membership used to be all a member got, so every group already in flight has
// a roster its members cannot name anybody in. Healing on read means an
// existing group fixes itself the first time it is opened, instead of waiting
// for its owner to happen to edit the roster. The lookup returns nothing once
// the links are there, which is every call after the first.
func backfillSplitGroupMemberLinks(db *gorm.DB, userID uint) error {
	var groupIDs []uint
	if err := db.Model(&models.SplitGroupUserMember{}).
		Where("split_group_user_members.user_id = ? AND split_group_user_members.status = ?", userID, "active").
		Where("NOT EXISTS (?)",
			db.Model(&models.SplitGroupMemberLink{}).
				Select("1").
				Where("split_group_member_links.group_id = split_group_user_members.group_id").
				Where("split_group_member_links.user_id = ?", userID),
		).
		Pluck("split_group_user_members.group_id", &groupIDs).Error; err != nil {
		return err
	}
	for _, groupID := range groupIDs {
		if err := db.Transaction(func(tx *gorm.DB) error {
			return syncSplitGroupMemberLinks(tx, groupID)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) listSplitGroups(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	if err := backfillSplitGroupMemberLinks(database.DB, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_groups"})
		return
	}

	sharedGroupIDs, err := activeSharedSplitGroupIDs(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_groups"})
		return
	}

	query := database.DB.Preload("Members.Friend")
	if len(sharedGroupIDs) > 0 {
		query = query.Where("user_id = ? OR id IN ?", userID, sharedGroupIDs)
	} else {
		query = query.Where("user_id = ?", userID)
	}
	if !strings.EqualFold(c.Query("status"), "all") {
		query = query.Where("archived = ?", false)
	}

	var groups []models.SplitGroup
	if err := query.Order("name asc, created_at desc").Find(&groups).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_groups"})
		return
	}
	applySplitGroupListViewerPermissions(groups, userID)
	if err := decorateSplitGroupsForViewer(database.DB, groups, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_groups"})
		return
	}
	groupBalances, err := buildSplitGroupBalances(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_groups"})
		return
	}
	for index := range groups {
		group := &groups[index]
		perFriend := groupBalances[group.ID]
		group.ViewerBalances = make([]models.SplitGroupFriendBalance, 0, len(perFriend))
		group.ViewerNetBalance = 0
		for friendID, net := range perFriend {
			group.ViewerBalances = append(group.ViewerBalances, models.SplitGroupFriendBalance{
				FriendID:   friendID,
				NetBalance: net,
			})
			group.ViewerNetBalance += net
		}
		// Map iteration order is random, and a list that reshuffles on every
		// poll makes the rows built from it animate for no reason.
		sort.SliceStable(group.ViewerBalances, func(i, j int) bool {
			return group.ViewerBalances[i].FriendID < group.ViewerBalances[j].FriendID
		})
	}
	c.JSON(http.StatusOK, groups)
}

func (s *Server) updateSplitGroup(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var input splitGroupInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group", "fields": fields})
		return
	}
	if fields, err := validateSplitGroupFriends(userID, input.FriendIDs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	} else if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group", "fields": fields})
		return
	}

	var group models.SplitGroup
	var addedFriendIDs []uint
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := ownedSplitGroups(tx, userID).Where("id = ?", id).First(&group).Error; err != nil {
			return err
		}
		group.Name = strings.TrimSpace(input.Name)
		group.Kind = normalizedSplitGroupKind(input.Kind)
		// Absent means "leave it alone", empty string means "remove it". An
		// older app build sends neither the field nor a photo, and must not
		// wipe one set from a newer build on another device.
		if input.PhotoURL != nil {
			group.PhotoURL, _ = splitGroupPhotoURL(*input.PhotoURL)
		}
		if err := tx.Save(&group).Error; err != nil {
			return err
		}
		var existingFriendIDs []uint
		if err := tx.Model(&models.SplitGroupMember{}).
			Where("user_id = ? AND group_id = ?", userID, group.ID).
			Pluck("friend_id", &existingFriendIDs).Error; err != nil {
			return err
		}
		// The roster is rewritten wholesale, so "who is new" has to be read
		// before the old rows go, not after.
		existing := map[uint]bool{}
		for _, friendID := range existingFriendIDs {
			existing[friendID] = true
		}
		if err := tx.Where("user_id = ? AND group_id = ?", userID, group.ID).Delete(&models.SplitGroupMember{}).Error; err != nil {
			return err
		}
		added, err := createSplitGroupMembers(tx, userID, group.ID, input.FriendIDs)
		if err != nil {
			return err
		}
		addedFriendIDs = nil
		for _, friendID := range added {
			if !existing[friendID] {
				addedFriendIDs = append(addedFriendIDs, friendID)
			}
		}
		return syncSplitGroupMemberLinks(tx, group.ID)
	}); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_group"})
		return
	}
	memberInvites := inviteSplitGroupMembersForOwner(userID, group, addedFriendIDs)
	_ = database.DB.Preload("Members.Friend").First(&group, group.ID).Error
	applySplitGroupViewerPermissions(&group, userID)
	_ = decorateSplitGroupForViewer(database.DB, &group, userID)
	group.MemberInvites = memberInvites
	c.JSON(http.StatusOK, group)
}

// updateSplitGroupDefaultSplit sets the split every new expense in the group
// starts from. Unlike renaming or re-membering a group, this is open to any
// active member: the default describes how the group divides its costs, and the
// people living under it are the ones who know when it changes.
func (s *Server) updateSplitGroupDefaultSplit(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var input splitGroupDefaultSplitInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}

	var group models.SplitGroup
	if err := database.DB.Preload("Members.Friend").
		Where("id = ? AND archived = ?", id, false).
		First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}

	accessible, err := viewerCanAccessSplitGroup(database.DB, group, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_group_default_split"})
		return
	}
	if !accessible {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}

	memberFriendIDs := map[string]bool{}
	for _, member := range group.Members {
		memberFriendIDs[strconv.FormatUint(uint64(member.FriendID), 10)] = true
	}

	normalized, fields := normalizeSplitGroupDefaultSplit(input.DefaultSplit, memberFriendIDs)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_group_default_split", "fields": fields})
		return
	}

	// A map update rather than a struct one: clearing the default writes NULL,
	// and GORM would read a typed nil pointer in a struct as "leave it alone".
	if err := database.DB.Model(&models.SplitGroup{ID: group.ID}).
		Updates(map[string]any{"default_split": normalized}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_group_default_split"})
		return
	}

	// Re-read into a fresh struct: scanning a NULL default split leaves the
	// destination pointer untouched, so reusing `group` would hand back the
	// value that was just cleared.
	var saved models.SplitGroup
	if err := database.DB.Preload("Members.Friend").First(&saved, group.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_group_default_split"})
		return
	}
	applySplitGroupViewerPermissions(&saved, userID)
	_ = decorateSplitGroupForViewer(database.DB, &saved, userID)
	c.JSON(http.StatusOK, saved)
}

func viewerCanAccessSplitGroup(db *gorm.DB, group models.SplitGroup, viewerUserID uint) (bool, error) {
	if group.UserID == viewerUserID {
		return true, nil
	}
	var count int64
	err := db.Model(&models.SplitGroupUserMember{}).
		Where("group_id = ? AND user_id = ? AND status = ?", group.ID, viewerUserID, "active").
		Count(&count).Error
	return count > 0, err
}

var splitGroupDefaultSplitTabs = map[string]bool{"equally": true, "percentages": true, "shares": true}

// normalizeSplitGroupDefaultSplit validates a default split against the group's
// actual roster and returns the value to store, or the fields that were wrong.
//
// The weights are checked here rather than at expense time on purpose: a
// default whose percentages do not reach 100 would otherwise sit in settings
// looking saved and fail on every expense that tried to use it.
func normalizeSplitGroupDefaultSplit(
	body *splitGroupDefaultSplitBody,
	memberFriendIDs map[string]bool,
) (*models.SplitGroupDefaultSplit, map[string]string) {
	if body == nil {
		return nil, nil
	}

	fields := map[string]string{}
	validSlot := func(slot string) bool {
		return slot == models.SplitGroupDefaultSplitOwnerSlot || memberFriendIDs[slot]
	}

	payer := strings.TrimSpace(body.Payer)
	if payer == "" {
		payer = models.SplitGroupDefaultSplitOwnerSlot
	}
	if !validSlot(payer) {
		fields["payer"] = "must be the group owner or a group member"
	}

	tab := strings.ToLower(strings.TrimSpace(body.Tab))
	if tab == "" {
		tab = "equally"
	}
	if !splitGroupDefaultSplitTabs[tab] {
		fields["tab"] = "must be one of equally, percentages, shares"
	}

	if len(body.Participants) == 0 {
		fields["participants"] = "must include at least one person"
	}

	seen := map[string]bool{}
	participants := make([]models.SplitGroupDefaultSplitShare, 0, len(body.Participants))
	weightTotal := 0.0
	for index, participant := range body.Participants {
		slot := strings.TrimSpace(participant.Slot)
		if !validSlot(slot) {
			fields[fmt.Sprintf("participants[%d].slot", index)] = "must be the group owner or a group member"
			continue
		}
		if seen[slot] {
			fields[fmt.Sprintf("participants[%d].slot", index)] = "duplicate participant"
			continue
		}
		seen[slot] = true

		weight := strings.TrimSpace(participant.Weight)
		if tab == "equally" {
			// An equal split carries no weights; storing them would leave stale
			// numbers to reappear when someone switches tabs later.
			participants = append(participants, models.SplitGroupDefaultSplitShare{Slot: slot})
			continue
		}
		parsed, err := strconv.ParseFloat(weight, 64)
		if err != nil || parsed <= 0 {
			fields[fmt.Sprintf("participants[%d].weight", index)] = "must be a positive number"
			continue
		}
		weightTotal += parsed
		participants = append(participants, models.SplitGroupDefaultSplitShare{Slot: slot, Weight: weight})
	}

	if tab == "percentages" && len(fields) == 0 && math.Abs(weightTotal-100) > 0.009 {
		fields["participants"] = "percentages must add up to 100"
	}

	if len(fields) > 0 {
		return nil, fields
	}
	return &models.SplitGroupDefaultSplit{
		Payer:        payer,
		FullAmount:   body.FullAmount,
		Tab:          tab,
		Participants: participants,
	}, nil
}

func inviteSplitGroupMembersForOwner(
	ownerID uint,
	group models.SplitGroup,
	addedFriendIDs []uint,
) []models.SplitGroupMemberInvite {
	if len(addedFriendIDs) == 0 {
		return nil
	}
	var owner models.User
	if err := database.DB.First(&owner, ownerID).Error; err != nil {
		return nil
	}
	return inviteSplitGroupMembers(database.DB, owner, group, addedFriendIDs)
}

// Deleting a group retires what it was keeping track of.
//
// Archiving the row alone was the whole of this handler, and it left every bill
// the group had raised sitting in `split_participants` — where
// `buildSplitBalances` sums them without ever looking at which group they came
// from. So a group the app showed as gone, on a screen that also said "settled
// up", kept contributing to the overall figure above it. The two statements
// were contradicting each other on the same screen.
//
// The bills go. What happens to the *transactions* behind them is the user's
// call, and it is a genuine choice rather than an implementation detail: a
// split bill and a Finnri entry are two different records of the same evening.
// The bill says who owes whom; the entry says money left the account, which it
// did, and which deleting a group does not undo. So the entries are kept unless
// the user says otherwise — see `entries=delete`.
func (s *Server) archiveSplitGroup(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	deleteEntries, ok := splitGroupEntryDisposition(c)
	if !ok {
		return
	}

	var removedEntries int64
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		result := ownedSplitGroups(tx.Model(&models.SplitGroup{}), userID).
			Where("id = ?", id).
			Update("archived", true)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}

		// Settlements first, and unconditionally: they are not attached to a
		// bill, so an early return on "this group had no bills" would leave
		// them behind. A settlement closes debts from this group's expenses,
		// and once those are gone it is applying its amount against nothing —
		// which is how a deleted group used to leave a permanent balance on a
		// screen with nothing on it.
		if err := tx.Where("user_id = ? AND group_id = ?", userID, id).
			Delete(&models.SplitSettlement{}).Error; err != nil {
			return err
		}

		var bills []models.SplitBill
		if err := tx.Where("user_id = ? AND group_id = ?", userID, id).Find(&bills).Error; err != nil {
			return err
		}
		if len(bills) == 0 {
			return nil
		}

		billIDs := make([]uint, 0, len(bills))
		entryIDs := make([]uint, 0, len(bills))
		for _, bill := range bills {
			billIDs = append(billIDs, bill.ID)
			if bill.EntryID != nil {
				entryIDs = append(entryIDs, *bill.EntryID)
			}
		}

		// Participants first: they are what the balances are summed from, and
		// the bill row is only the thing that groups them.
		if err := tx.Where("user_id = ? AND bill_id IN ?", userID, billIDs).
			Delete(&models.SplitParticipant{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND id IN ?", userID, billIDs).
			Delete(&models.SplitBill{}).Error; err != nil {
			return err
		}

		if !deleteEntries || len(entryIDs) == 0 {
			return nil
		}
		// Scoped by user_id as well as by id: the ids came from this user's own
		// bills, and the second predicate is what makes that a guarantee rather
		// than an inference.
		entriesResult := tx.Where("user_id = ? AND id IN ?", userID, entryIDs).Delete(&models.Entry{})
		if entriesResult.Error != nil {
			return entriesResult.Error
		}
		removedEntries = entriesResult.RowsAffected
		return nil
	})
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_archive_split_group"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "split group archived",
		// Reported rather than assumed, so the app can say what it actually did
		// instead of what it asked for.
		"deleted_entries": removedEntries,
	})
}

// splitGroupEntryDisposition reads the one choice the delete carries.
//
// `keep` is the default and the absent value, so an older app build — which
// sends no parameter at all — gets the behaviour that destroys nothing.
func splitGroupEntryDisposition(c *gin.Context) (deleteEntries bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(c.Query("entries"))) {
	case "", "keep":
		return false, true
	case "delete":
		return true, true
	}
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":  "invalid_entry_disposition",
		"fields": gin.H{"entries": "must be keep or delete"},
	})
	return false, false
}

func (s *Server) leaveSplitGroup(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var group models.SplitGroup
	if err := database.DB.Where("id = ? AND archived = ?", id, false).First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_not_found"})
		return
	}
	if group.UserID == userID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "split_group_owner_cannot_leave"})
		return
	}

	result := database.DB.Model(&models.SplitGroupUserMember{}).
		Where("group_id = ? AND user_id = ? AND status = ?", id, userID, "active").
		Update("status", "removed")
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_leave_split_group"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_group_membership_not_found"})
		return
	}
	// The slot translations go with the membership. The friend rows they point
	// at stay: they carry whatever was already split in this group, and that
	// history outlives leaving it.
	if err := database.DB.Where("group_id = ? AND user_id = ?", id, userID).
		Delete(&models.SplitGroupMemberLink{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_leave_split_group"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "split group left"})
}

func (s *Server) createSplitBill(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var input splitBillInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": fields})
		return
	}
	if input.EntryID != nil {
		if ok, err := userOwnsEntry(userID, *input.EntryID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "entry_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": gin.H{"entry_id": "must belong to the current user"}})
			return
		}
	}
	if err := resolveMergedSplitBillParticipants(userID, input.GroupID, input.Participants); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	}
	if fields, err := validateSplitBillParticipantFriends(userID, input.GroupID, input.Participants); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	} else if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": fields})
		return
	}
	if input.GroupID != nil {
		if ok, err := userCanAccessActiveSplitGroup(userID, *input.GroupID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "split_group_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": gin.H{"group_id": "must be a group you can access"}})
			return
		}
	}

	var bill models.SplitBill
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		bill = input.toModel(userID)
		if err := tx.Create(&bill).Error; err != nil {
			return err
		}
		participants := make([]models.SplitParticipant, 0, len(input.Participants))
		for _, participant := range input.Participants {
			participants = append(participants, participant.toModel(userID, bill.ID))
		}
		if err := tx.Create(&participants).Error; err != nil {
			return err
		}
		return tx.Preload("Group").Preload("Participants.Friend").First(&bill, bill.ID).Error
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_bill"})
		return
	}

	applySplitBillViewerPermissions(&bill, userID)
	c.JSON(http.StatusCreated, bill)
}

func (s *Server) listSplitBills(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	accessibleGroupIDs, err := accessibleActiveSplitGroupIDs(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_bills"})
		return
	}

	query := database.DB.Preload("Group").Preload("Participants.Friend")
	if len(accessibleGroupIDs) > 0 {
		query = query.Where("(user_id = ? AND group_id IS NULL) OR group_id IN ?", userID, accessibleGroupIDs)
	} else {
		query = query.Where("user_id = ? AND group_id IS NULL", userID)
	}

	var bills []models.SplitBill
	if err := query.
		Order("date desc, created_at desc").
		Find(&bills).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_bills"})
		return
	}
	applySplitBillListViewerPermissions(bills, userID)
	c.JSON(http.StatusOK, bills)
}

func (s *Server) getSplitBillByEntry(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	entryID, ok := parseUintParam(c, "entry_id")
	if !ok {
		return
	}

	var bill models.SplitBill
	err := database.DB.
		Preload("Group").
		Preload("Participants.Friend").
		Where("user_id = ? AND entry_id = ?", userID, entryID).
		First(&bill).Error
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusOK, nil)
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_get_split_bill"})
		return
	}

	applySplitBillViewerPermissions(&bill, userID)
	c.JSON(http.StatusOK, bill)
}

func (s *Server) updateSplitBill(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var input splitBillInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": fields})
		return
	}
	if input.EntryID != nil {
		if ok, err := userOwnsEntry(userID, *input.EntryID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "entry_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": gin.H{"entry_id": "must belong to the current user"}})
			return
		}
	}
	if err := resolveMergedSplitBillParticipants(userID, input.GroupID, input.Participants); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	}
	if fields, err := validateSplitBillParticipantFriends(userID, input.GroupID, input.Participants); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	} else if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": fields})
		return
	}
	if input.GroupID != nil {
		if ok, err := userCanAccessActiveSplitGroup(userID, *input.GroupID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "split_group_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_bill", "fields": gin.H{"group_id": "must be a group you can access"}})
			return
		}
	}

	var bill models.SplitBill
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND id = ?", userID, id).First(&bill).Error; err != nil {
			return err
		}
		bill.EntryID = input.EntryID
		bill.GroupID = input.GroupID
		bill.Title = strings.TrimSpace(input.Title)
		bill.TotalAmount = input.TotalAmount
		bill.Currency = normalizedSplitCurrency(input.Currency)
		bill.Date = input.Date
		bill.Notes = strings.TrimSpace(input.Notes)
		if err := tx.Save(&bill).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND bill_id = ?", userID, bill.ID).Delete(&models.SplitParticipant{}).Error; err != nil {
			return err
		}
		participants := make([]models.SplitParticipant, 0, len(input.Participants))
		for _, participant := range input.Participants {
			participants = append(participants, participant.toModel(userID, bill.ID))
		}
		if err := tx.Create(&participants).Error; err != nil {
			return err
		}
		return tx.Preload("Group").Preload("Participants.Friend").First(&bill, bill.ID).Error
	}); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_bill_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_bill"})
		return
	}

	applySplitBillViewerPermissions(&bill, userID)
	c.JSON(http.StatusOK, bill)
}

func (s *Server) deleteSplitBill(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND bill_id = ?", userID, id).Delete(&models.SplitParticipant{}).Error; err != nil {
			return err
		}
		result := tx.Where("user_id = ? AND id = ?", userID, id).Delete(&models.SplitBill{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	}); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_bill_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_split_bill"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "split bill deleted"})
}

func (s *Server) createSplitSettlement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var input splitSettlementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_settlement", "fields": fields})
		return
	}
	// Checked before the friend, because whether the caller can reach the group
	// decides which rule the friend is judged by.
	if input.GroupID != nil {
		if ok, err := userCanAccessActiveSplitGroup(userID, *input.GroupID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "split_group_lookup_failed"})
			return
		} else if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  "invalid_split_settlement",
				"fields": gin.H{"group_id": "must be a group you can access"},
			})
			return
		}
	}

	// Same two repairs the bill path makes: an owner-namespace id from an older
	// build becomes the caller's own row for the same person, and a row merged
	// away since the screen was opened becomes the row that absorbed it.
	rewrite, err := splitGroupLocalFriendRewrite(userID, input.GroupID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	}
	if local, ok := rewrite[input.FriendID]; ok {
		input.FriendID = local
	}
	resolvedFriendID, err := resolveMergedSplitFriendID(database.DB, userID, input.FriendID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	}
	input.FriendID = resolvedFriendID

	// Inside a group a settlement follows the rule its expenses follow — the
	// roster, read in the caller's own frame. Ownership alone would let a
	// member close a group balance against a friend who has nothing to do with
	// it. Outside a group there is no roster, so ownership is all there is.
	if input.GroupID != nil {
		allowed, err := splitGroupBillableFriendIDs(*input.GroupID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
			return
		}
		if !allowed[input.FriendID] {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  "invalid_split_settlement",
				"fields": gin.H{"friend_id": "must belong to this group"},
			})
			return
		}
	} else if ok, err := userOwnsActiveSplitFriend(userID, input.FriendID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_friend_lookup_failed"})
		return
	} else if !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_split_settlement", "fields": gin.H{"friend_id": "must belong to the current user"}})
		return
	}

	settlement := input.toModel(userID)
	if err := database.DB.Create(&settlement).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_split_settlement"})
		return
	}
	_ = database.DB.Preload("Friend").First(&settlement, settlement.ID).Error
	c.JSON(http.StatusCreated, settlement)
}

func (s *Server) listSplitSettlements(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var settlements []models.SplitSettlement
	if err := activeLedgerSettlements(database.DB, userID).Preload("Friend").
		Order("split_settlements.date desc, split_settlements.created_at desc").
		Find(&settlements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_settlements"})
		return
	}
	c.JSON(http.StatusOK, settlements)
}

func (s *Server) listSplitActivity(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))

	// Activity is a history of the ledger the user still has, not of everything
	// that ever happened to it. A deleted group kept narrating itself here —
	// "Ma Beta created", the expenses inside it, the settlements that closed
	// them — for a group no other screen would open.
	// Scoped like the bills list rather than to the viewer's own rows. A shared
	// group's feed used to narrate only half of itself: the expenses the viewer
	// had entered, and none of what anybody else in the group had — while the
	// very same expenses were listed on the group's own screen.
	accessibleGroupIDs, err := accessibleActiveSplitGroupIDs(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}
	frames, err := loadSplitGroupFrames(database.DB, accessibleGroupIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}

	var bills []models.SplitBill
	billQuery := database.DB.Preload("Group").Preload("Participants.Friend")
	if len(accessibleGroupIDs) > 0 {
		billQuery = billQuery.Where(
			"(split_bills.user_id = ? AND split_bills.group_id IS NULL) OR split_bills.group_id IN ?",
			userID, accessibleGroupIDs)
	} else {
		billQuery = billQuery.Where("split_bills.user_id = ? AND split_bills.group_id IS NULL", userID)
	}
	if err := billQuery.Find(&bills).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}
	var settlements []models.SplitSettlement
	if err := activeLedgerSettlements(database.DB, userID).Preload("Friend").
		Find(&settlements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}
	// Payments the other side recorded. Restated below, because a settlement's
	// direction and title are written from its author's point of view.
	var foreignSettlements []models.SplitSettlement
	if len(accessibleGroupIDs) > 0 {
		if err := database.DB.
			Where("group_id IN ? AND user_id <> ?", accessibleGroupIDs, userID).
			Find(&foreignSettlements).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
			return
		}
	}
	var groups []models.SplitGroup
	if err := database.DB.Preload("Members.Friend").
		Where("user_id = ? AND archived = ?", userID, false).
		Find(&groups).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}
	// Groups the viewer joined rather than made. Dated from when they joined,
	// which is the moment it entered *their* history — the group's own creation
	// date belongs to somebody else's.
	var sharedMemberships []models.SplitGroupUserMember
	if err := database.DB.Preload("Group").Preload("Group.Members").
		Where("user_id = ? AND status = ?", userID, "active").
		Find(&sharedMemberships).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}
	var friends []models.SplitFriend
	if err := database.DB.
		Where("user_id = ? AND archived = ?", userID, false).
		Find(&friends).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_activity"})
		return
	}

	items := make([]splitActivityItem, 0, len(bills)+len(settlements)+len(groups)+len(friends))
	for _, group := range groups {
		groupCopy := group
		items = append(items, splitActivityItem{
			ID:               fmt.Sprintf("group-%d", group.ID),
			Type:             "group_created",
			RecordID:         group.ID,
			Title:            fmt.Sprintf("%s created", group.Name),
			Date:             group.CreatedAt.Format("2006-01-02"),
			GroupID:          &group.ID,
			Group:            &groupCopy,
			ParticipantCount: len(group.Members),
			CreatedAt:        group.CreatedAt,
		})
	}
	for _, friend := range friends {
		friendID := friend.ID
		friendCopy := friend
		items = append(items, splitActivityItem{
			ID:        fmt.Sprintf("friend-%d", friend.ID),
			Type:      "friend_created",
			RecordID:  friend.ID,
			Title:     fmt.Sprintf("%s added", fallbackSplitFriendName(friend)),
			Date:      friend.CreatedAt.Format("2006-01-02"),
			FriendID:  &friendID,
			Friend:    &friendCopy,
			CreatedAt: friend.CreatedAt,
		})
	}
	friendsByID := map[uint]models.SplitFriend{}
	for _, friend := range friends {
		friendsByID[friend.ID] = friend
	}
	// How the viewer knows whoever wrote a line in a shared group. Their account
	// name would be a name the viewer has never seen; the friend row they keep
	// for that person is the one every other screen shows.
	actorName := func(groupID *uint, authorID uint) string {
		if groupID == nil || authorID == userID {
			return ""
		}
		frame, ok := frames[*groupID]
		if !ok {
			return ""
		}
		authorSlot, ok := frame.slotOfUser[authorID]
		if !ok {
			return ""
		}
		friend, ok := friendsByID[frame.friendFor(authorSlot, userID)]
		if !ok {
			return ""
		}
		return fallbackSplitFriendName(friend)
	}

	for _, bill := range bills {
		amount := bill.TotalAmount
		item := splitActivityItem{
			ID:               fmt.Sprintf("bill-%d", bill.ID),
			Type:             "bill",
			RecordID:         bill.ID,
			Title:            bill.Title,
			Date:             bill.Date,
			Amount:           &amount,
			GroupID:          bill.GroupID,
			ParticipantCount: len(bill.Participants),
			Participants:     bill.Participants,
			Notes:            bill.Notes,
			ActorName:        actorName(bill.GroupID, bill.UserID),
			CreatedAt:        bill.CreatedAt,
		}
		if bill.Group != nil && bill.Group.ID != 0 {
			item.Group = bill.Group
		}
		items = append(items, item)
	}

	for _, settlement := range foreignSettlements {
		if settlement.GroupID == nil {
			continue
		}
		frame, ok := frames[*settlement.GroupID]
		if !ok {
			continue
		}
		counterpartID, aboutViewer := restateForeignSplitRow(frame, userID, settlement.UserID, settlement.FriendID)
		direction := flipSettlementDirection(settlement.Direction)
		if !aboutViewer || direction == "" {
			continue
		}
		counterpart, ok := friendsByID[counterpartID]
		if !ok {
			continue
		}
		amount := settlement.Amount
		title := "Settlement"
		if direction == settlementDirectionFriendPaidUser {
			title = fmt.Sprintf("%s paid you", fallbackSplitFriendName(counterpart))
		} else if direction == settlementDirectionUserPaidFriend {
			title = fmt.Sprintf("You paid %s", fallbackSplitFriendName(counterpart))
		}
		friendID := counterpart.ID
		friendCopy := counterpart
		items = append(items, splitActivityItem{
			ID:        fmt.Sprintf("settlement-%d", settlement.ID),
			Type:      "settlement",
			RecordID:  settlement.ID,
			Title:     title,
			Date:      settlement.Date,
			Amount:    &amount,
			GroupID:   settlement.GroupID,
			FriendID:  &friendID,
			Friend:    &friendCopy,
			Direction: direction,
			Notes:     settlement.Notes,
			ActorName: actorName(settlement.GroupID, settlement.UserID),
			CreatedAt: settlement.CreatedAt,
		})
	}

	for _, membership := range sharedMemberships {
		group := membership.Group
		if group.ID == 0 || group.Archived || group.UserID == userID {
			continue
		}
		groupID := group.ID
		groupCopy := group
		items = append(items, splitActivityItem{
			ID:       fmt.Sprintf("group-joined-%d", group.ID),
			Type:     "group_created",
			RecordID: group.ID,
			Title:    fmt.Sprintf("Joined %s", group.Name),
			Date:     membership.CreatedAt.Format("2006-01-02"),
			GroupID:  &groupID,
			Group:    &groupCopy,
			// The roster count is what the card's caption reads; without it a
			// group you joined announces itself as having nobody in it.
			ParticipantCount: len(group.Members),
			CreatedAt:        membership.CreatedAt,
		})
	}
	for _, settlement := range settlements {
		friendID := settlement.FriendID
		amount := settlement.Amount
		title := "Settlement"
		if settlement.Direction == settlementDirectionFriendPaidUser {
			title = fmt.Sprintf("%s paid you", fallbackSplitFriendName(settlement.Friend))
		} else if settlement.Direction == settlementDirectionUserPaidFriend {
			title = fmt.Sprintf("You paid %s", fallbackSplitFriendName(settlement.Friend))
		}
		item := splitActivityItem{
			ID:        fmt.Sprintf("settlement-%d", settlement.ID),
			Type:      "settlement",
			RecordID:  settlement.ID,
			Title:     title,
			Date:      settlement.Date,
			Amount:    &amount,
			FriendID:  &friendID,
			Direction: settlement.Direction,
			Notes:     settlement.Notes,
			CreatedAt: settlement.CreatedAt,
		}
		if settlement.Friend.ID != 0 {
			friend := settlement.Friend
			item.Friend = &friend
		}
		items = append(items, item)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Date != items[j].Date {
			return items[i].Date > items[j].Date
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})

	total := len(items)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	c.JSON(http.StatusOK, gin.H{
		"items":     items[start:end],
		"page":      page,
		"page_size": pageSize,
		"total":     total,
	})
}

func (s *Server) listSplitBalances(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	balances, err := buildSplitBalances(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_split_balances"})
		return
	}
	c.JSON(http.StatusOK, balances)
}

func fallbackSplitFriendName(friend models.SplitFriend) string {
	if strings.TrimSpace(friend.Name) == "" {
		return "Friend"
	}
	return friend.Name
}

func (input splitFriendInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Name) == "" {
		fields["name"] = "is required"
	}
	if len(strings.TrimSpace(input.Name)) > 120 {
		fields["name"] = "must not exceed 120 characters"
	}
	if len(strings.TrimSpace(input.Email)) > 254 {
		fields["email"] = "must not exceed 254 characters"
	}
	if len(strings.TrimSpace(input.Phone)) > 32 {
		fields["phone"] = "must not exceed 32 characters"
	}
	if strings.TrimSpace(input.Phone) != "" && identity.NormalizePhone(input.Phone) == "" {
		fields["phone"] = "must contain at least 10 digits"
	}
	return fields
}

func (input splitFriendInput) toModel(userID uint) models.SplitFriend {
	return models.SplitFriend{
		UserID: userID,
		Name:   strings.TrimSpace(input.Name),
		Email:  strings.TrimSpace(input.Email),
		Phone:  strings.TrimSpace(input.Phone),
	}
}

func (input splitFriendInput) apply(friend *models.SplitFriend) {
	friend.Name = strings.TrimSpace(input.Name)
	friend.Email = strings.TrimSpace(input.Email)
	friend.Phone = strings.TrimSpace(input.Phone)
}

var splitGroupKinds = map[string]bool{"trip": true, "home": true, "couple": true, "other": true}

func normalizedSplitGroupKind(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if normalized == "" {
		return "other"
	}
	return normalized
}

// splitGroupPhotoURL keeps a group photo pointing at our own upload origin.
//
// The value is rendered by every member's app, so accepting an arbitrary URL
// would let one member's group settings pull an image — and the request that
// fetches it — from anywhere. Uploading through POST /v1/upload is the only way
// to get a URL this accepts, and that handler already sniffs the bytes.
func splitGroupPhotoURL(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	if !strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), uploadDir+"/") {
		return "", false
	}
	return trimmed, true
}

func (input splitGroupInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Name) == "" {
		fields["name"] = "is required"
	}
	if len(strings.TrimSpace(input.Name)) > 120 {
		fields["name"] = "must not exceed 120 characters"
	}
	if !splitGroupKinds[normalizedSplitGroupKind(input.Kind)] {
		fields["kind"] = "must be one of trip, home, couple, other"
	}
	seen := map[uint]bool{}
	for index, friendID := range input.FriendIDs {
		if friendID == 0 {
			fields[fmt.Sprintf("friend_ids[%d]", index)] = "must be a positive integer"
		}
		if seen[friendID] {
			fields[fmt.Sprintf("friend_ids[%d]", index)] = "duplicate friend"
		}
		seen[friendID] = true
	}
	if input.PhotoURL != nil {
		if _, ok := splitGroupPhotoURL(*input.PhotoURL); !ok {
			fields["photo_url"] = "must be an image uploaded to Finnri"
		}
	}
	return fields
}

func (input splitBillInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Title) == "" {
		fields["title"] = "is required"
	}
	if !input.TotalAmount.IsPositive() {
		fields["total_amount"] = "must be positive"
	}
	currency := normalizedSplitCurrency(input.Currency)
	if currency != "INR" {
		fields["currency"] = "must be INR"
	}
	if _, err := time.Parse("2006-01-02", input.Date); err != nil {
		fields["date"] = "must use YYYY-MM-DD"
	}
	if input.GroupID != nil && *input.GroupID == 0 {
		fields["group_id"] = "must be a positive integer"
	}
	if len(input.Participants) == 0 {
		fields["participants"] = "must include at least one friend share"
	}

	seen := map[string]bool{}
	totalShares := models.Money(0)
	for index, participant := range input.Participants {
		prefix := fmt.Sprintf("participants[%d]", index)
		if participant.FriendID == 0 {
			fields[prefix+".friend_id"] = "must be a positive integer"
		}
		if !participant.ShareAmount.IsPositive() {
			fields[prefix+".share_amount"] = "must be positive"
		}
		direction := normalizeSplitDirection(participant.Direction)
		if direction == "" {
			fields[prefix+".direction"] = "must be friend_owes_user or user_owes_friend"
		}
		key := fmt.Sprintf("%d:%s", participant.FriendID, direction)
		if participant.FriendID != 0 && direction != "" {
			if seen[key] {
				fields[prefix+".friend_id"] = "duplicate friend and direction"
			}
			seen[key] = true
		}
		totalShares += participant.ShareAmount
	}
	if totalShares > input.TotalAmount {
		fields["participants"] = "shares must not exceed total_amount"
	}
	return fields
}

func (input splitBillInput) toModel(userID uint) models.SplitBill {
	return models.SplitBill{
		UserID:      userID,
		EntryID:     input.EntryID,
		GroupID:     input.GroupID,
		Title:       strings.TrimSpace(input.Title),
		TotalAmount: input.TotalAmount,
		Currency:    normalizedSplitCurrency(input.Currency),
		Date:        input.Date,
		Notes:       strings.TrimSpace(input.Notes),
	}
}

func (input splitParticipantInput) toModel(userID, billID uint) models.SplitParticipant {
	return models.SplitParticipant{
		UserID:      userID,
		BillID:      billID,
		FriendID:    input.FriendID,
		ShareAmount: input.ShareAmount,
		Direction:   normalizeSplitDirection(input.Direction),
	}
}

func (input splitSettlementInput) validate() map[string]string {
	fields := map[string]string{}
	if input.FriendID == 0 {
		fields["friend_id"] = "must be a positive integer"
	}
	if !input.Amount.IsPositive() {
		fields["amount"] = "must be positive"
	}
	if normalizeSettlementDirection(input.Direction) == "" {
		fields["direction"] = "must be friend_paid_user or user_paid_friend"
	}
	if _, err := time.Parse("2006-01-02", input.Date); err != nil {
		fields["date"] = "must use YYYY-MM-DD"
	}
	return fields
}

func (input splitSettlementInput) toModel(userID uint) models.SplitSettlement {
	return models.SplitSettlement{
		UserID:    userID,
		FriendID:  input.FriendID,
		GroupID:   input.GroupID,
		Amount:    input.Amount,
		Direction: normalizeSettlementDirection(input.Direction),
		Date:      input.Date,
		Notes:     strings.TrimSpace(input.Notes),
	}
}

// splitGroupFrame is one shared group's roster, in every namespace at once.
//
// A group names its people by slot — the owner, or one of the owner's friend
// rows — but every *bill* names them by a friend row belonging to whoever wrote
// it. Reading somebody else's bill therefore means two translations: which slot
// the line is about, and which of my own rows stands for the person who wrote
// it. This holds both directions so neither has to be queried per row.
type splitGroupFrame struct {
	ownerID uint
	// Which slot each Finnri account in this group occupies.
	slotOfUser map[uint]string
	// slot -> the owner's friend row for it. The owner's own slot is absent:
	// nobody is one of their own friend rows.
	ownerSlotFriend map[string]uint
	// slot -> member -> that member's own friend row for the slot.
	linkSlotFriend map[string]map[uint]uint
}

// friendFor is the row `forUser` would name to mean the person in `slot`, or 0
// when they have none — which is exactly what the owner has for themselves.
func (frame splitGroupFrame) friendFor(slot string, forUser uint) uint {
	if forUser == frame.ownerID {
		return frame.ownerSlotFriend[slot]
	}
	return frame.linkSlotFriend[slot][forUser]
}

func loadSplitGroupFrames(db *gorm.DB, groupIDs []uint) (map[uint]splitGroupFrame, error) {
	frames := map[uint]splitGroupFrame{}
	if len(groupIDs) == 0 {
		return frames, nil
	}

	var groups []models.SplitGroup
	if err := db.Where("id IN ?", groupIDs).Find(&groups).Error; err != nil {
		return nil, err
	}
	for _, group := range groups {
		frames[group.ID] = splitGroupFrame{
			ownerID:         group.UserID,
			slotOfUser:      map[uint]string{group.UserID: models.SplitGroupDefaultSplitOwnerSlot},
			ownerSlotFriend: map[string]uint{},
			linkSlotFriend:  map[string]map[uint]uint{},
		}
	}

	var members []models.SplitGroupMember
	if err := db.Preload("Friend").Where("group_id IN ?", groupIDs).Find(&members).Error; err != nil {
		return nil, err
	}
	for _, member := range members {
		frame, ok := frames[member.GroupID]
		// Only the owner's rows are the roster; a stray written by an older
		// build is not somebody this group can bill.
		if !ok || member.UserID != frame.ownerID || member.Friend.Archived {
			continue
		}
		slot := splitGroupSlotForFriend(member.FriendID)
		frame.ownerSlotFriend[slot] = member.FriendID
		if member.Friend.LinkedUserID != nil {
			frame.slotOfUser[*member.Friend.LinkedUserID] = slot
		}
	}

	var links []models.SplitGroupMemberLink
	if err := db.Where("group_id IN ?", groupIDs).Find(&links).Error; err != nil {
		return nil, err
	}
	for _, link := range links {
		frame, ok := frames[link.GroupID]
		if !ok {
			continue
		}
		if frame.linkSlotFriend[link.Slot] == nil {
			frame.linkSlotFriend[link.Slot] = map[uint]uint{}
		}
		frame.linkSlotFriend[link.Slot][link.UserID] = link.FriendID
	}
	return frames, nil
}

// restateForeignSplitRow answers, for a line somebody else wrote in a shared
// group: is it about the viewer, and if so which of the viewer's own friend
// rows stands for the person who wrote it?
//
// A line is about the viewer when the row it names is the one its author uses
// for the viewer's slot. The counterpart is then the author, named the way the
// viewer names them — which is the only name the viewer's screens can resolve.
func restateForeignSplitRow(frame splitGroupFrame, viewerID, authorID, friendID uint) (uint, bool) {
	viewerSlot, ok := frame.slotOfUser[viewerID]
	if !ok {
		return 0, false
	}
	authorSlot, ok := frame.slotOfUser[authorID]
	if !ok {
		return 0, false
	}
	if frame.friendFor(viewerSlot, authorID) != friendID {
		return 0, false
	}
	counterpart := frame.friendFor(authorSlot, viewerID)
	if counterpart == 0 {
		return 0, false
	}
	return counterpart, true
}

func flipSplitDirection(direction string) string {
	switch direction {
	case splitDirectionFriendOwesUser:
		return splitDirectionUserOwesFriend
	case splitDirectionUserOwesFriend:
		return splitDirectionFriendOwesUser
	}
	return ""
}

func flipSettlementDirection(direction string) string {
	switch direction {
	case settlementDirectionFriendPaidUser:
		return settlementDirectionUserPaidFriend
	case settlementDirectionUserPaidFriend:
		return settlementDirectionFriendPaidUser
	}
	return ""
}

// splitLedgerAdjustment is one line of somebody else's ledger, restated in the
// viewer's own terms: their friend row, and the direction seen from their side.
type splitLedgerAdjustment struct {
	FriendID  uint
	GroupID   uint
	Direction string
	Amount    models.Money
}

// foldForeignSplitLedger restates every line another member of a shared group
// wrote *about the viewer* as a line of the viewer's own ledger.
//
// A bill records the debts of its author and nobody else's, against friend rows
// only its author owns. So a group's ledger was only ever half-read: the owner
// summed their own bills and never saw a rupee of what a member recorded, and
// the member saw nothing of the owner's. Both sides showed "settled up" over a
// group with money moving through it.
//
// A line belongs to the viewer when the row it names is the one its author uses
// for the viewer's slot. The counterpart is then the author, named by whichever
// of the viewer's own rows stands for *them* — and the direction flips, because
// "they owe me" written by somebody else means "I owe them" here.
func foldForeignSplitLedger(db *gorm.DB, userID uint) (
	participants []splitLedgerAdjustment,
	settlements []splitLedgerAdjustment,
	err error,
) {
	groupIDs, err := accessibleActiveSplitGroupIDs(db, userID)
	if err != nil || len(groupIDs) == 0 {
		return nil, nil, err
	}
	frames, err := loadSplitGroupFrames(db, groupIDs)
	if err != nil {
		return nil, nil, err
	}

	type foreignRow struct {
		UserID    uint
		FriendID  uint
		Amount    models.Money
		Direction string
		GroupID   uint
	}

	translate := func(rows []foreignRow, flip func(string) string) []splitLedgerAdjustment {
		out := make([]splitLedgerAdjustment, 0, len(rows))
		for _, row := range rows {
			frame, ok := frames[row.GroupID]
			if !ok {
				continue
			}
			counterpart, aboutViewer := restateForeignSplitRow(frame, userID, row.UserID, row.FriendID)
			direction := flip(row.Direction)
			if !aboutViewer || direction == "" {
				continue
			}
			out = append(out, splitLedgerAdjustment{
				FriendID:  counterpart,
				GroupID:   row.GroupID,
				Direction: direction,
				Amount:    row.Amount,
			})
		}
		return out
	}

	var participantRows []foreignRow
	if err := db.Table("split_participants").
		Select(`split_participants.user_id,
			split_participants.friend_id,
			split_participants.share_amount AS amount,
			split_participants.direction,
			split_bills.group_id`).
		Joins("JOIN split_bills ON split_bills.id = split_participants.bill_id").
		Where("split_bills.group_id IN ?", groupIDs).
		Where("split_participants.user_id <> ?", userID).
		Scan(&participantRows).Error; err != nil {
		return nil, nil, err
	}

	// Settlements fold for the same reason the expenses do. Leaving them out
	// would show the viewer everything they are owed and none of what has
	// already been paid back against it, which is worse than showing neither.
	var settlementRows []foreignRow
	if err := db.Table("split_settlements").
		Select("user_id, friend_id, amount, direction, group_id").
		Where("group_id IN ?", groupIDs).
		Where("user_id <> ?", userID).
		Scan(&settlementRows).Error; err != nil {
		return nil, nil, err
	}

	return translate(participantRows, flipSplitDirection),
		translate(settlementRows, flipSettlementDirection),
		nil
}

// buildSplitGroupBalances is every group's ledger, per person, from the
// viewer's side. Keyed group id -> the viewer's friend id -> net.
//
// It exists because the group cards used to work this out on the client by
// summing every participant row in the group and reading `direction` as if it
// were absolute. It is not: a bill states the debts of whoever wrote it, so
// somebody else's expense came out inverted — a card telling the owner that a
// member owed him money she had in fact laid out for him. The same fold that
// fixes the headline balances is the only thing that can answer this correctly,
// so it is answered here and sent, rather than guessed at twice.
func buildSplitGroupBalances(db *gorm.DB, userID uint) (map[uint]map[uint]models.Money, error) {
	balances := map[uint]map[uint]models.Money{}
	add := func(groupID, friendID uint, amount models.Money) {
		if groupID == 0 || friendID == 0 {
			return
		}
		if balances[groupID] == nil {
			balances[groupID] = map[uint]models.Money{}
		}
		balances[groupID][friendID] += amount
	}

	type ledgerRow struct {
		FriendID  uint
		GroupID   uint
		Amount    models.Money
		Direction string
	}

	// The viewer's own bills, which already name their own friend rows.
	var ownParticipants []ledgerRow
	if err := db.Table("split_participants").
		Select(`split_participants.friend_id,
			split_bills.group_id,
			split_participants.share_amount AS amount,
			split_participants.direction`).
		Joins("JOIN split_bills ON split_bills.id = split_participants.bill_id").
		Joins("JOIN split_groups ON split_groups.id = split_bills.group_id").
		Where("split_participants.user_id = ?", userID).
		Where("split_groups.archived = ?", false).
		Scan(&ownParticipants).Error; err != nil {
		return nil, err
	}
	for _, row := range ownParticipants {
		switch row.Direction {
		case splitDirectionFriendOwesUser:
			add(row.GroupID, row.FriendID, row.Amount)
		case splitDirectionUserOwesFriend:
			add(row.GroupID, row.FriendID, -row.Amount)
		}
	}

	var ownSettlements []ledgerRow
	if err := db.Table("split_settlements").
		Select("split_settlements.friend_id, split_settlements.group_id, split_settlements.amount, split_settlements.direction").
		Joins("JOIN split_groups ON split_groups.id = split_settlements.group_id").
		Where("split_settlements.user_id = ?", userID).
		Where("split_groups.archived = ?", false).
		Scan(&ownSettlements).Error; err != nil {
		return nil, err
	}
	for _, row := range ownSettlements {
		switch row.Direction {
		case settlementDirectionFriendPaidUser:
			add(row.GroupID, row.FriendID, -row.Amount)
		case settlementDirectionUserPaidFriend:
			add(row.GroupID, row.FriendID, row.Amount)
		}
	}

	// And everybody else's, restated in the viewer's terms.
	foreignParticipants, foreignSettlements, err := foldForeignSplitLedger(db, userID)
	if err != nil {
		return nil, err
	}
	for _, adjustment := range foreignParticipants {
		switch adjustment.Direction {
		case splitDirectionFriendOwesUser:
			add(adjustment.GroupID, adjustment.FriendID, adjustment.Amount)
		case splitDirectionUserOwesFriend:
			add(adjustment.GroupID, adjustment.FriendID, -adjustment.Amount)
		}
	}
	for _, adjustment := range foreignSettlements {
		switch adjustment.Direction {
		case settlementDirectionFriendPaidUser:
			add(adjustment.GroupID, adjustment.FriendID, -adjustment.Amount)
		case settlementDirectionUserPaidFriend:
			add(adjustment.GroupID, adjustment.FriendID, adjustment.Amount)
		}
	}
	return balances, nil
}

func buildSplitBalances(db *gorm.DB, userID uint) ([]splitBalance, error) {
	var friends []models.SplitFriend
	if err := ownedSplitFriends(db, userID).Order("name asc, created_at desc").Find(&friends).Error; err != nil {
		return nil, err
	}

	balancesByFriend := map[uint]*splitBalance{}
	for _, friend := range friends {
		friend := friend
		balancesByFriend[friend.ID] = &splitBalance{Friend: friend}
	}

	// Joined to the bill rather than read on its own. A participant row is a
	// line on a bill and has no meaning without one: when the bill is gone the
	// share is not an unpaid debt, it is a fragment of a deleted record. Read
	// flat, those fragments moved the headline figure while appearing on no
	// screen that could explain or clear them — an account with every group
	// deleted still reported an outstanding balance.
	//
	// Bills belonging to an archived group are excluded for the same reason:
	// the group is off every list, so nothing that sums it can be reconciled.
	var participants []models.SplitParticipant
	if err := db.Model(&models.SplitParticipant{}).
		Joins("JOIN split_bills ON split_bills.id = split_participants.bill_id").
		Joins("LEFT JOIN split_groups ON split_groups.id = split_bills.group_id").
		Where("split_participants.user_id = ?", userID).
		Where("split_bills.group_id IS NULL OR (split_groups.id IS NOT NULL AND split_groups.archived = ?)", false).
		Find(&participants).Error; err != nil {
		return nil, err
	}
	for _, participant := range participants {
		balance := balancesByFriend[participant.FriendID]
		if balance == nil {
			continue
		}
		switch participant.Direction {
		case splitDirectionFriendOwesUser:
			balance.TotalOwedByFriend += participant.ShareAmount
			balance.NetBalance += participant.ShareAmount
		case splitDirectionUserOwesFriend:
			balance.TotalOwedToFriend += participant.ShareAmount
			balance.NetBalance -= participant.ShareAmount
		}
	}

	// Same rule for the other side of the ledger. A settlement recorded inside
	// a group is only meaningful while that group's expenses exist; once the
	// group is gone it has nothing left to settle, and applying it anyway is
	// what turned a deleted group into a permanent phantom balance.
	var settlements []models.SplitSettlement
	if err := db.Model(&models.SplitSettlement{}).
		Joins("LEFT JOIN split_groups ON split_groups.id = split_settlements.group_id").
		Where("split_settlements.user_id = ?", userID).
		Where("split_settlements.group_id IS NULL OR (split_groups.id IS NOT NULL AND split_groups.archived = ?)", false).
		Find(&settlements).Error; err != nil {
		return nil, err
	}
	for _, settlement := range settlements {
		balance := balancesByFriend[settlement.FriendID]
		if balance == nil {
			continue
		}
		switch settlement.Direction {
		case settlementDirectionFriendPaidUser:
			balance.TotalOwedByFriend -= settlement.Amount
			balance.NetBalance -= settlement.Amount
		case settlementDirectionUserPaidFriend:
			balance.TotalOwedToFriend -= settlement.Amount
			balance.NetBalance += settlement.Amount
		}
	}

	// And now the other half of every shared group: what the people in it wrote
	// about this user. Their rows name friend ids this user does not own, so
	// nothing above reaches them — which is how a group with money moving
	// through it managed to report "settled up" to both people in it.
	foreignParticipants, foreignSettlements, err := foldForeignSplitLedger(db, userID)
	if err != nil {
		return nil, err
	}
	for _, adjustment := range foreignParticipants {
		balance := balancesByFriend[adjustment.FriendID]
		if balance == nil {
			continue
		}
		switch adjustment.Direction {
		case splitDirectionFriendOwesUser:
			balance.TotalOwedByFriend += adjustment.Amount
			balance.NetBalance += adjustment.Amount
		case splitDirectionUserOwesFriend:
			balance.TotalOwedToFriend += adjustment.Amount
			balance.NetBalance -= adjustment.Amount
		}
	}
	for _, adjustment := range foreignSettlements {
		balance := balancesByFriend[adjustment.FriendID]
		if balance == nil {
			continue
		}
		switch adjustment.Direction {
		case settlementDirectionFriendPaidUser:
			balance.TotalOwedByFriend -= adjustment.Amount
			balance.NetBalance -= adjustment.Amount
		case settlementDirectionUserPaidFriend:
			balance.TotalOwedToFriend -= adjustment.Amount
			balance.NetBalance += adjustment.Amount
		}
	}

	result := make([]splitBalance, 0, len(friends))
	for _, friend := range friends {
		result = append(result, *balancesByFriend[friend.ID])
	}
	return result, nil
}

// resolveMergedSplitBillParticipants rewrites friend ids the caller is holding
// to the rows that absorbed them, in place.
//
// The composer reads its people list once and is then open for as long as the
// user takes. A duplicate merged away in between leaves it naming an archived
// row, and rejecting that is a validation error about somebody the user can see
// on screen and cannot do anything about.
func resolveMergedSplitBillParticipants(userID uint, groupID *uint, participants []splitParticipantInput) error {
	rewrite, err := splitGroupLocalFriendRewrite(userID, groupID)
	if err != nil {
		return err
	}
	for index := range participants {
		if participants[index].FriendID == 0 {
			continue
		}
		if local, ok := rewrite[participants[index].FriendID]; ok {
			participants[index].FriendID = local
		}
		resolved, err := resolveMergedSplitFriendID(database.DB, userID, participants[index].FriendID)
		if err != nil {
			return err
		}
		participants[index].FriendID = resolved
	}
	return nil
}

func validateSplitParticipantFriends(userID uint, participants []splitParticipantInput) (gin.H, error) {
	fields := gin.H{}
	for index, participant := range participants {
		if participant.FriendID == 0 {
			continue
		}
		ok, err := userOwnsActiveSplitFriend(userID, participant.FriendID)
		if err != nil {
			return nil, err
		}
		if !ok {
			fields[fmt.Sprintf("participants[%d].friend_id", index)] = "must belong to the current user"
		}
	}
	return fields, nil
}

func validateSplitBillParticipantFriends(userID uint, groupID *uint, participants []splitParticipantInput) (gin.H, error) {
	if groupID == nil {
		return validateSplitParticipantFriends(userID, participants)
	}

	// Joined against the friend rows rather than read from the membership table
	// alone, because membership survives archiving. A share recorded against an
	// archived friend is money that leaves the payer's side of the ledger and
	// arrives nowhere: `buildSplitBalances` walks active friends and never
	// reaches that participant row, so the amount is gone from every figure the
	// app shows rather than merely misfiled.
	//
	// Scoped by the group and by `archived`, and deliberately not by `userID`.
	// Membership always names the *owner's* friend rows, and a member recording
	// an expense in a shared group names them too — so the caller is usually
	// not the person those rows belong to.
	allowedFriendIDs, err := splitGroupBillableFriendIDs(*groupID, userID)
	if err != nil {
		return nil, err
	}

	fields := gin.H{}
	for index, participant := range participants {
		if participant.FriendID == 0 {
			continue
		}
		if !allowedFriendIDs[participant.FriendID] {
			fields[fmt.Sprintf("participants[%d].friend_id", index)] = "must belong to this group"
		}
	}
	return fields, nil
}

func validateSplitGroupFriends(userID uint, friendIDs []uint) (gin.H, error) {
	fields := gin.H{}
	for index, friendID := range friendIDs {
		if friendID == 0 {
			continue
		}
		ok, err := userOwnsActiveSplitFriend(userID, friendID)
		if err != nil {
			return nil, err
		}
		if !ok {
			fields[fmt.Sprintf("friend_ids[%d]", index)] = "must belong to the current user"
		}
	}
	return fields, nil
}

// decorateSplitGroupsForViewer fills in the two things a shared group cannot
// answer from its own row: who owns it, and which of its member friend rows is
// the person reading it. The default split names people by the owner's friend
// ids, so without the second answer a member could not tell which slot is
// theirs.
func decorateSplitGroupsForViewer(db *gorm.DB, groups []models.SplitGroup, viewerUserID uint) error {
	if len(groups) == 0 {
		return nil
	}

	ownerIDs := map[uint]bool{}
	friendIDs := []uint{}
	groupIDs := make([]uint, 0, len(groups))
	for index := range groups {
		ownerIDs[groups[index].UserID] = true
		groupIDs = append(groupIDs, groups[index].ID)
		for _, member := range groups[index].Members {
			friendIDs = append(friendIDs, member.FriendID)
		}
	}

	// The slot translations this viewer holds, across every group at once.
	var links []models.SplitGroupMemberLink
	if err := db.Where("user_id = ? AND group_id IN ?", viewerUserID, groupIDs).
		Find(&links).Error; err != nil {
		return err
	}
	slotFriendsByGroup := map[uint]map[string]uint{}
	for _, link := range links {
		if slotFriendsByGroup[link.GroupID] == nil {
			slotFriendsByGroup[link.GroupID] = map[string]uint{}
		}
		slotFriendsByGroup[link.GroupID][link.Slot] = link.FriendID
	}

	ownerNames := map[uint]string{}
	if len(ownerIDs) > 0 {
		ids := make([]uint, 0, len(ownerIDs))
		for ownerID := range ownerIDs {
			ids = append(ids, ownerID)
		}
		var owners []models.User
		if err := db.Where("id IN ?", ids).Find(&owners).Error; err != nil {
			return err
		}
		for _, owner := range owners {
			ownerNames[owner.ID] = displayNameForUser(owner)
		}
	}

	viewerFriendIDs := map[uint]bool{}
	if len(friendIDs) > 0 {
		var linked []uint
		if err := db.Model(&models.SplitFriend{}).
			Where("id IN ? AND linked_user_id = ?", friendIDs, viewerUserID).
			Pluck("id", &linked).Error; err != nil {
			return err
		}
		for _, friendID := range linked {
			viewerFriendIDs[friendID] = true
		}
	}

	for index := range groups {
		group := &groups[index]
		group.OwnerName = ownerNames[group.UserID]
		group.ViewerFriendID = nil
		group.ViewerSlotFriends = nil
		// The owner is never one of their own friend rows, so they are always
		// the owner slot and never a member slot — and every slot already names
		// a row they own, so they need no translation either.
		if group.UserID == viewerUserID {
			continue
		}
		group.ViewerSlotFriends = slotFriendsByGroup[group.ID]
		for _, member := range group.Members {
			if viewerFriendIDs[member.FriendID] {
				friendID := member.FriendID
				group.ViewerFriendID = &friendID
				break
			}
		}
	}
	return nil
}

func decorateSplitGroupForViewer(db *gorm.DB, group *models.SplitGroup, viewerUserID uint) error {
	if group == nil {
		return nil
	}
	groups := []models.SplitGroup{*group}
	if err := decorateSplitGroupsForViewer(db, groups, viewerUserID); err != nil {
		return err
	}
	*group = groups[0]
	return nil
}

func applySplitGroupViewerPermissions(group *models.SplitGroup, viewerUserID uint) {
	if group == nil {
		return
	}
	group.ViewerCanAddExpense = true
	if group.UserID == viewerUserID {
		group.ViewerRole = "owner"
		group.ViewerCanManage = true
		return
	}
	group.ViewerRole = "member"
	group.ViewerCanManage = false
}

func applySplitGroupListViewerPermissions(groups []models.SplitGroup, viewerUserID uint) {
	for index := range groups {
		applySplitGroupViewerPermissions(&groups[index], viewerUserID)
	}
}

func applySplitBillViewerPermissions(bill *models.SplitBill, viewerUserID uint) {
	if bill == nil {
		return
	}
	canModify := bill.UserID == viewerUserID
	bill.ViewerCanEdit = canModify
	bill.ViewerCanDelete = canModify
	if bill.Group != nil {
		applySplitGroupViewerPermissions(bill.Group, viewerUserID)
	}
}

func applySplitBillListViewerPermissions(bills []models.SplitBill, viewerUserID uint) {
	for index := range bills {
		applySplitBillViewerPermissions(&bills[index], viewerUserID)
	}
}

// validateEntrySplitReferences checks the split attached to a transaction, and
// repairs the ids it can before judging them.
//
// It used to apply a stricter rule than the standalone split composer next to
// it, and rejected two things that composer accepts:
//
//   - A group somebody else owns. Membership of a shared group is exactly the
//     permission to record expenses in it, so "must belong to the current user"
//     was wrong about a group the user was actively splitting with.
//   - The owner's friend rows inside such a group. A member's own friend list
//     never contains them, so every participant in a shared group failed.
//
// The third failure was self-inflicted: merging two duplicate friends archives
// one of them, and a composer opened before the merge still names the archived
// id. All three arrived together as a wall of "must belong to the current
// user" under a Save button, about people plainly on screen, with nothing the
// user could do about any of it.
//
// Stale ids are rewritten in place rather than reported, so the bill this input
// goes on to create is recorded against the surviving row.
func validateEntrySplitReferences(userID uint, input *entrySplitInput) (gin.H, error) {
	fields := gin.H{}
	if input == nil {
		return fields, nil
	}
	if input.GroupID != nil {
		ok, err := userCanAccessActiveSplitGroup(userID, *input.GroupID)
		if err != nil {
			return nil, err
		}
		if !ok {
			fields["split.group_id"] = "must be a group you can access"
		}
	}

	for index := range input.Participants {
		if input.Participants[index].FriendID == nil {
			continue
		}
		resolved, err := resolveMergedSplitFriendID(database.DB, userID, *input.Participants[index].FriendID)
		if err != nil {
			return nil, err
		}
		input.Participants[index].FriendID = &resolved
	}

	// Inside a group the rule is the roster — the same one createSplitBill
	// uses — read in the caller's own frame: their friend rows if they own the
	// group, the rows their slot links gave them otherwise. Outside a group
	// there is no roster to check against, so ownership is all there is.
	if input.GroupID != nil {
		rewrite, err := splitGroupLocalFriendRewrite(userID, input.GroupID)
		if err != nil {
			return nil, err
		}
		for index := range input.Participants {
			friendID := input.Participants[index].FriendID
			if friendID == nil {
				continue
			}
			if local, ok := rewrite[*friendID]; ok {
				localID := local
				input.Participants[index].FriendID = &localID
			}
		}
		allowed, err := splitGroupBillableFriendIDs(*input.GroupID, userID)
		if err != nil {
			return nil, err
		}
		for index, participant := range input.Participants {
			if participant.FriendID == nil {
				continue
			}
			if !allowed[*participant.FriendID] {
				fields[fmt.Sprintf("split.participants[%d].friend_id", index)] = "must belong to this group"
			}
		}
		return fields, nil
	}

	for index, participant := range input.Participants {
		if participant.FriendID == nil {
			continue
		}
		ok, err := userOwnsActiveSplitFriend(userID, *participant.FriendID)
		if err != nil {
			return nil, err
		}
		if !ok {
			fields[fmt.Sprintf("split.participants[%d].friend_id", index)] = "must belong to the current user"
		}
	}
	return fields, nil
}

// activeSplitGroupFriendIDs is the set of friend rows a group expense may name.
//
// Joined against the friend rows rather than read from the membership table
// alone, because membership survives archiving: a share recorded against an
// archived friend is money that leaves the payer's side of the ledger and
// arrives nowhere, since buildSplitBalances walks active friends and never
// reaches that participant row.
//
// Scoped to the *owner's* membership rows. A member's expense composer used to
// be able to write membership into somebody else's group, and those strays
// survived every roster rewrite — which only deletes the owner's rows — leaving
// people permanently in a group with no way to take them out. They are excluded
// here so an old stray cannot widen who may be named on a bill.
func activeSplitGroupFriendIDs(groupID uint) (map[uint]bool, error) {
	var group models.SplitGroup
	if err := database.DB.First(&group, groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[uint]bool{}, nil
		}
		return nil, err
	}
	var friendIDs []uint
	if err := database.DB.Model(&models.SplitGroupMember{}).
		Joins("JOIN split_friends ON split_friends.id = split_group_members.friend_id").
		Where("split_group_members.group_id = ?", groupID).
		Where("split_group_members.user_id = ?", group.UserID).
		Where("split_friends.archived = ?", false).
		Pluck("split_group_members.friend_id", &friendIDs).Error; err != nil {
		return nil, err
	}
	allowed := make(map[uint]bool, len(friendIDs))
	for _, friendID := range friendIDs {
		allowed[friendID] = true
	}
	return allowed, nil
}

// splitGroupBillableFriendIDs is the set of friend rows `userID` may name on a
// bill in `groupID`, expressed in their own frame.
//
// The owner names their own friend rows, because the roster is written in their
// namespace. Everybody else names the rows `split_group_member_links` gave
// them — which is what finally lets a member record that the *owner* owes them,
// rather than only ever being able to name themselves.
func splitGroupBillableFriendIDs(groupID, userID uint) (map[uint]bool, error) {
	var group models.SplitGroup
	if err := database.DB.First(&group, groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[uint]bool{}, nil
		}
		return nil, err
	}
	if group.UserID == userID {
		return activeSplitGroupFriendIDs(groupID)
	}

	var friendIDs []uint
	if err := database.DB.Model(&models.SplitGroupMemberLink{}).
		Joins("JOIN split_friends ON split_friends.id = split_group_member_links.friend_id").
		Where("split_group_member_links.group_id = ?", groupID).
		Where("split_group_member_links.user_id = ?", userID).
		Where("split_friends.archived = ?", false).
		Pluck("split_group_member_links.friend_id", &friendIDs).Error; err != nil {
		return nil, err
	}
	allowed := make(map[uint]bool, len(friendIDs))
	for _, friendID := range friendIDs {
		allowed[friendID] = true
	}
	return allowed, nil
}

// splitGroupLocalFriendRewrite maps the owner's friend ids onto the caller's
// own rows for one group. Nil when the caller needs no translation.
//
// Members used to be handed the owner's rows to split against, because those
// were the only rows the roster named. Balances never reach them —
// buildSplitBalances walks the friends the viewer owns — so the share was
// written to the database and then shown on no screen at all. Anything still
// holding one, an old bill being edited or an app build from before the links
// existed, is rewritten to the row now standing for the same person, so the
// bill heals on its next save instead of failing validation about somebody
// plainly in the group.
func splitGroupLocalFriendRewrite(userID uint, groupID *uint) (map[uint]uint, error) {
	if groupID == nil {
		return nil, nil
	}
	var group models.SplitGroup
	if err := database.DB.First(&group, *groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if group.UserID == userID {
		return nil, nil
	}
	slots, err := splitGroupSlotFriendIDs(database.DB, group.ID, userID)
	if err != nil || len(slots) == 0 {
		return nil, err
	}
	rewrite := make(map[uint]uint, len(slots))
	for slot, localFriendID := range slots {
		ownerSide, parseErr := strconv.ParseUint(slot, 10, 64)
		if parseErr != nil {
			// The owner slot stands for a user, not a friend row, so there is
			// no owner-side id anybody could have been holding.
			continue
		}
		rewrite[uint(ownerSide)] = localFriendID
	}
	return rewrite, nil
}

func createEntrySplitBill(tx *gorm.DB, userID uint, entry models.Entry, input *entrySplitInput) error {
	if input == nil {
		return nil
	}

	friendIDs := make([]uint, 0, len(input.Participants))
	participants := make([]models.SplitParticipant, 0, len(input.Participants))
	for _, participant := range input.Participants {
		friendID := uint(0)
		if participant.FriendID != nil {
			friendID = *participant.FriendID
		} else {
			friend := participant.Friend.toModel(userID)
			if err := tx.Create(&friend).Error; err != nil {
				return err
			}
			friendID = friend.ID
		}
		friendIDs = append(friendIDs, friendID)
		direction := normalizeSplitDirection(participant.Direction)
		if direction == "" {
			direction = splitDirectionFriendOwesUser
		}
		participants = append(participants, models.SplitParticipant{
			UserID:      userID,
			FriendID:    friendID,
			ShareAmount: participant.ShareAmount,
			Direction:   direction,
		})
	}

	groupID := input.GroupID
	if groupID == nil && strings.TrimSpace(input.GroupName) != "" {
		group := models.SplitGroup{UserID: userID, Name: strings.TrimSpace(input.GroupName)}
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		groupID = &group.ID
	}
	if groupID != nil {
		// Only the group's owner writes its roster. A member splitting into a
		// shared group names people who are already in it, through their own
		// slot links — and adding membership from here wrote rows in the
		// member's name that the owner's roster rewrite can neither see nor
		// remove, leaving ghosts nobody could take out of the group.
		var group models.SplitGroup
		if err := tx.First(&group, *groupID).Error; err != nil {
			return err
		}
		if group.UserID == userID {
			if _, err := createSplitGroupMembers(tx, userID, *groupID, friendIDs); err != nil {
				return err
			}
			if err := syncSplitGroupMemberLinks(tx, *groupID); err != nil {
				return err
			}
		}
	}

	entryID := entry.ID
	bill := models.SplitBill{
		UserID:      userID,
		EntryID:     &entryID,
		GroupID:     groupID,
		Title:       entry.Title,
		TotalAmount: entry.Amount,
		Currency:    entry.Currency,
		Date:        entry.Date,
		Notes:       strings.TrimSpace(input.Notes),
	}
	if bill.Title == "" {
		bill.Title = "Split transaction"
	}
	if bill.Currency == "" {
		bill.Currency = "INR"
	}
	if err := tx.Create(&bill).Error; err != nil {
		return err
	}
	for index := range participants {
		participants[index].BillID = bill.ID
	}
	return tx.Create(&participants).Error
}

func replaceEntrySplitBill(tx *gorm.DB, userID uint, entry models.Entry, input *entrySplitInput) error {
	if err := deleteEntrySplitBills(tx, userID, entry.ID); err != nil {
		return err
	}
	return createEntrySplitBill(tx, userID, entry, input)
}

func deleteEntrySplitBills(tx *gorm.DB, userID, entryID uint) error {
	var billIDs []uint
	if err := tx.Model(&models.SplitBill{}).
		Where("user_id = ? AND entry_id = ?", userID, entryID).
		Pluck("id", &billIDs).Error; err != nil {
		return err
	}
	if len(billIDs) == 0 {
		return nil
	}
	if err := tx.Where("user_id = ? AND bill_id IN ?", userID, billIDs).
		Delete(&models.SplitParticipant{}).Error; err != nil {
		return err
	}
	return tx.Where("user_id = ? AND id IN ?", userID, billIDs).
		Delete(&models.SplitBill{}).Error
}

// createSplitGroupMembers returns the friends it actually added, so the caller
// can invite exactly those people and nobody gets told twice when a group is
// saved again with the same roster.
func createSplitGroupMembers(tx *gorm.DB, userID, groupID uint, friendIDs []uint) ([]uint, error) {
	seen := map[uint]bool{}
	members := make([]models.SplitGroupMember, 0, len(friendIDs))
	added := make([]uint, 0, len(friendIDs))
	for _, friendID := range friendIDs {
		if friendID == 0 || seen[friendID] {
			continue
		}
		seen[friendID] = true
		var count int64
		if err := tx.Model(&models.SplitGroupMember{}).
			Where("user_id = ? AND group_id = ? AND friend_id = ?", userID, groupID, friendID).
			Count(&count).Error; err != nil {
			return nil, err
		}
		if count > 0 {
			continue
		}
		members = append(members, models.SplitGroupMember{
			UserID:   userID,
			GroupID:  groupID,
			FriendID: friendID,
		})
		added = append(added, friendID)
	}
	if len(members) == 0 {
		return added, nil
	}
	return added, tx.Create(&members).Error
}

// splitFriendUser finds the account behind a friend row: the recorded link
// first, then the email or phone the owner saved. Returns nil when the row
// stands for somebody who has no Finnri account, or none we can identify.
func splitFriendUser(db *gorm.DB, friend models.SplitFriend) (*models.User, error) {
	if friend.LinkedUserID != nil {
		var linked models.User
		err := db.First(&linked, *friend.LinkedUserID).Error
		if err == nil {
			return &linked, nil
		}
		if err != gorm.ErrRecordNotFound {
			return nil, err
		}
	}

	email := strings.ToLower(strings.TrimSpace(friend.Email))
	phone := friend.PhoneNormalized
	if phone == "" {
		phone = identity.NormalizePhone(friend.Phone)
	}
	if email == "" && phone == "" {
		return nil, nil
	}

	query := db
	switch {
	case email != "" && phone != "":
		query = query.Where("LOWER(email) = ? OR phone_normalized = ?", email, phone)
	case email != "":
		query = query.Where("LOWER(email) = ?", email)
	default:
		query = query.Where("phone_normalized = ?", phone)
	}
	var matched models.User
	err := query.First(&matched).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &matched, nil
}

// inviteSplitGroupMembers raises an invite for each person just added to a
// group and tells whoever has an account about it.
//
// Adding somebody to a group has always been a private bookkeeping act — the
// owner can split against a friend who has never heard of Finnri. What it must
// not do is leave that person unaware that a group now exists in their name, so
// every added friend either gets an invite they can accept or is reported back
// as someone the owner has to reach themselves. Membership still only follows
// acceptance; nothing here grants sight of the group.
func inviteSplitGroupMembers(
	db *gorm.DB,
	owner models.User,
	group models.SplitGroup,
	friendIDs []uint,
) []models.SplitGroupMemberInvite {
	if len(friendIDs) == 0 {
		return nil
	}

	results := make([]models.SplitGroupMemberInvite, 0, len(friendIDs))
	var invite models.SplitGroupInvite
	inviteLoaded := false

	for _, friendID := range friendIDs {
		var friend models.SplitFriend
		if err := db.Where("user_id = ?", owner.ID).First(&friend, friendID).Error; err != nil {
			continue
		}

		result := models.SplitGroupMemberInvite{
			FriendID: friend.ID,
			Name:     fallbackSplitFriendName(friend),
			Status:   models.SplitMemberInviteNoContact,
		}

		targetEmail := strings.ToLower(strings.TrimSpace(friend.Email))
		targetPhone := strings.TrimSpace(friend.Phone)
		invitedUser, err := splitFriendUser(db, friend)
		if err != nil {
			results = append(results, result)
			continue
		}
		// Somebody the owner listed as a friend but who turns out to be the
		// owner's own account cannot be invited to their own group.
		if invitedUser != nil && invitedUser.ID == owner.ID {
			invitedUser = nil
		}
		if invitedUser != nil {
			if targetEmail == "" {
				targetEmail = strings.ToLower(strings.TrimSpace(stringFromPointer(invitedUser.Email)))
			}
			if targetPhone == "" {
				targetPhone = strings.TrimSpace(stringFromPointer(invitedUser.Phone))
			}
		}
		if targetEmail == "" && targetPhone == "" {
			results = append(results, result)
			continue
		}

		if !inviteLoaded {
			loaded, inviteErr := getOrCreateActiveSplitGroupInvite(db, owner.ID, group.ID)
			if inviteErr != nil {
				results = append(results, result)
				continue
			}
			invite = loaded
			inviteLoaded = true
		}

		var invitedUserID *uint
		if invitedUser != nil {
			invitedUserID = &invitedUser.ID
		}
		directInvite, inviteErr := getOrCreateSplitGroupDirectInvite(
			db, owner.ID, group.ID, invite.ID, targetEmail, targetPhone, invitedUserID,
		)
		if inviteErr != nil {
			results = append(results, result)
			continue
		}
		if directInvite.FriendID == nil || *directInvite.FriendID != friend.ID {
			directInvite.FriendID = &friend.ID
			_ = db.Save(&directInvite).Error
		}

		result.Status = models.SplitMemberInviteLinkNeeded
		if invitedUser != nil {
			// Record the link now that we know who this row stands for; it is
			// what lets the group tell them apart from the owner's other
			// friends when they accept.
			if friend.LinkedUserID == nil || *friend.LinkedUserID != invitedUser.ID {
				_ = db.Model(&models.SplitFriend{}).
					Where("id = ?", friend.ID).
					Update("linked_user_id", invitedUser.ID).Error
			}
			if err := createNotification(
				invitedUser.ID,
				"split.group_invite.received",
				fmt.Sprintf("Join %s on Finnri", group.Name),
				fmt.Sprintf("%s added you to a split group.", displayNameForUser(owner)),
				fmt.Sprintf("/invite/split/%s", invite.Token),
			); err == nil {
				result.Status = models.SplitMemberInviteNotified
			}
		}
		results = append(results, result)
	}

	return results
}

func normalizedSplitCurrency(currency string) string {
	if strings.TrimSpace(currency) == "" {
		return "INR"
	}
	return strings.ToUpper(strings.TrimSpace(currency))
}

func normalizeSplitDirection(direction string) string {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case splitDirectionFriendOwesUser:
		return splitDirectionFriendOwesUser
	case splitDirectionUserOwesFriend:
		return splitDirectionUserOwesFriend
	default:
		return ""
	}
}

func normalizeSettlementDirection(direction string) string {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case settlementDirectionFriendPaidUser:
		return settlementDirectionFriendPaidUser
	case settlementDirectionUserPaidFriend:
		return settlementDirectionUserPaidFriend
	default:
		return ""
	}
}

func generateSplitInviteToken() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func getOrCreateActiveSplitGroupInvite(db *gorm.DB, userID, groupID uint) (models.SplitGroupInvite, error) {
	var invite models.SplitGroupInvite
	err := db.
		Where("user_id = ? AND group_id = ? AND status = ?", userID, groupID, "active").
		Order("created_at desc").
		First(&invite).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return models.SplitGroupInvite{}, err
	}
	if err == nil {
		return invite, nil
	}

	token, tokenErr := generateSplitInviteToken()
	if tokenErr != nil {
		return models.SplitGroupInvite{}, tokenErr
	}
	invite = models.SplitGroupInvite{
		UserID:  userID,
		GroupID: groupID,
		Token:   token,
		Status:  "active",
	}
	if err := db.Create(&invite).Error; err != nil {
		return models.SplitGroupInvite{}, err
	}
	return invite, nil
}

func getOrCreateSplitGroupDirectInvite(db *gorm.DB, userID, groupID, inviteID uint, targetEmail, targetPhone string, invitedUserID *uint) (models.SplitGroupDirectInvite, error) {
	query := db.Where("user_id = ? AND group_id = ? AND status = ?", userID, groupID, "pending")
	if targetEmail != "" {
		query = query.Where("LOWER(target_email) = ?", strings.ToLower(targetEmail))
	} else {
		query = query.Where("target_phone_normalized = ?", identity.NormalizePhone(targetPhone))
	}

	var directInvite models.SplitGroupDirectInvite
	err := query.First(&directInvite).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return models.SplitGroupDirectInvite{}, err
	}
	if err == nil {
		changed := false
		if directInvite.InviteID != inviteID {
			directInvite.InviteID = inviteID
			changed = true
		}
		if invitedUserID != nil && (directInvite.InvitedUserID == nil || *directInvite.InvitedUserID != *invitedUserID) {
			directInvite.InvitedUserID = invitedUserID
			changed = true
		}
		if changed {
			return directInvite, db.Save(&directInvite).Error
		}
		return directInvite, nil
	}

	directInvite = models.SplitGroupDirectInvite{
		UserID:        userID,
		GroupID:       groupID,
		InviteID:      inviteID,
		TargetEmail:   targetEmail,
		TargetPhone:   targetPhone,
		InvitedUserID: invitedUserID,
		Status:        "pending",
	}
	if err := db.Create(&directInvite).Error; err != nil {
		return models.SplitGroupDirectInvite{}, err
	}
	return directInvite, nil
}

func splitGroupDirectInviteToResponse(invite models.SplitGroupDirectInvite, group models.SplitGroup, owner models.User, webBaseURL string) splitGroupDirectInviteResponse {
	token := invite.Invite.Token
	url := ""
	deepLink := ""
	message := ""
	if token != "" {
		url = splitInviteURL(webBaseURL, token)
		deepLink = splitInviteDeepLink(token)
		message = fmt.Sprintf("%s invited you to join %s on Finnri to track shared expenses: %s", displayNameForUser(owner), group.Name, url)
	}
	return splitGroupDirectInviteResponse{
		ID:          invite.ID,
		TargetEmail: invite.TargetEmail,
		TargetPhone: invite.TargetPhone,
		MatchedUser: invite.InvitedUserID != nil,
		URL:         url,
		DeepLink:    deepLink,
		Message:     message,
		Status:      invite.Status,
		Group:       group,
		CreatedAt:   invite.CreatedAt,
	}
}

func splitInviteURL(webBaseURL, token string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(webBaseURL), "/")
	if baseURL == "" || strings.TrimSpace(token) == "" {
		return ""
	}
	return fmt.Sprintf("%s/invite/split/%s", baseURL, token)
}

func (s *Server) webBaseURL() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.WebBaseURL
}

func splitInviteDeepLink(token string) string {
	return fmt.Sprintf("finnri://invite/split/%s", token)
}

func displayNameForUser(user models.User) string {
	if strings.TrimSpace(user.Username) != "" {
		return strings.TrimSpace(user.Username)
	}
	if user.Email != nil && strings.TrimSpace(*user.Email) != "" {
		return strings.TrimSpace(*user.Email)
	}
	if user.Phone != nil && strings.TrimSpace(*user.Phone) != "" {
		return strings.TrimSpace(*user.Phone)
	}
	return "Finnri user"
}

func stringFromPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// splitInviteTargetIdentityQuery is the same identity test as
// splitInviteUserIdentityQuery, against the address an invite was sent to
// rather than the columns of a friend row.
func splitInviteTargetIdentityQuery(user models.User) (string, []any) {
	parts := []string{}
	args := []any{}
	if user.Email != nil && strings.TrimSpace(*user.Email) != "" {
		parts = append(parts, "LOWER(target_email) = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(*user.Email)))
	}
	if user.PhoneNormalized != "" {
		parts = append(parts, "target_phone_normalized = ?")
		args = append(args, user.PhoneNormalized)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func splitInviteUserIdentityQuery(user models.User) (string, []any) {
	parts := []string{}
	args := []any{}
	if user.Email != nil && strings.TrimSpace(*user.Email) != "" {
		parts = append(parts, "LOWER(email) = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(*user.Email)))
	}
	if user.PhoneNormalized != "" {
		parts = append(parts, "phone_normalized = ?")
		args = append(args, user.PhoneNormalized)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func userOwnsActiveSplitFriend(userID, friendID uint) (bool, error) {
	var count int64
	err := ownedSplitFriends(database.DB.Model(&models.SplitFriend{}), userID).
		Where("id = ? AND archived = ?", friendID, false).
		Count(&count).Error
	return count == 1, err
}

func userOwnsActiveSplitGroup(userID, groupID uint) (bool, error) {
	var count int64
	err := ownedSplitGroups(database.DB.Model(&models.SplitGroup{}), userID).
		Where("id = ? AND archived = ?", groupID, false).
		Count(&count).Error
	return count == 1, err
}

func userCanAccessActiveSplitGroup(userID, groupID uint) (bool, error) {
	var count int64
	err := database.DB.Model(&models.SplitGroup{}).
		Where("id = ? AND archived = ?", groupID, false).
		Where(
			"user_id = ? OR id IN (?)",
			userID,
			database.DB.Model(&models.SplitGroupUserMember{}).
				Select("group_id").
				Where("user_id = ? AND status = ?", userID, "active"),
		).
		Count(&count).Error
	return count == 1, err
}

func activeSharedSplitGroupIDs(db *gorm.DB, userID uint) ([]uint, error) {
	var ids []uint
	err := db.Model(&models.SplitGroupUserMember{}).
		Where("user_id = ? AND status = ?", userID, "active").
		Pluck("group_id", &ids).Error
	return ids, err
}

func accessibleActiveSplitGroupIDs(db *gorm.DB, userID uint) ([]uint, error) {
	var ids []uint
	err := db.Model(&models.SplitGroup{}).
		Where("archived = ?", false).
		Where(
			"user_id = ? OR id IN (?)",
			userID,
			db.Model(&models.SplitGroupUserMember{}).
				Select("group_id").
				Where("user_id = ? AND status = ?", userID, "active"),
		).
		Pluck("id", &ids).Error
	return ids, err
}

func userOwnsEntry(userID, entryID uint) (bool, error) {
	var count int64
	err := ownedEntries(database.DB.Model(&models.Entry{}), userID).
		Where("id = ?", entryID).
		Count(&count).Error
	return count == 1, err
}

// activeLedgerSettlements scopes settlements to the ledger that still exists.
//
// A settlement recorded inside a group only means anything while that group's
// expenses do. Deleting a group removes its bills, and a settlement left
// applying its full amount against nothing is what made a Splits screen with no
// groups on it report an outstanding balance.
func activeLedgerSettlements(db *gorm.DB, userID uint) *gorm.DB {
	return db.Model(&models.SplitSettlement{}).
		Joins("LEFT JOIN split_groups ON split_groups.id = split_settlements.group_id").
		Where("split_settlements.user_id = ?", userID).
		Where("split_settlements.group_id IS NULL OR (split_groups.id IS NOT NULL AND split_groups.archived = ?)", false)
}

func ownedSplitFriends(db *gorm.DB, userID uint) *gorm.DB {
	return db.Where("user_id = ?", userID)
}

func ownedSplitGroups(db *gorm.DB, userID uint) *gorm.DB {
	return db.Where("user_id = ?", userID)
}

func parseUintParam(c *gin.Context, name string) (uint, bool) {
	value, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil || value == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return uint(value), true
}
