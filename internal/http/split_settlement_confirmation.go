package http

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// A settlement is one person's word about money that moved outside Finnri, and
// it rewrites both sides of a shared ledger. Until this file existed that word
// was final: the friend it named was never asked, never told, and the only
// record was a line in an activity feed among everything else that had ever
// happened. This is the other half — who has to agree, how they say yes or no,
// and what each of them hears back.

// settlementStatusOrDefault reads a status column written before the column
// existed. Rows created under the old rule have been counting in both balances
// since the day they were made, so silence means agreed, not unanswered.
func settlementStatusOrDefault(status string) string {
	switch strings.TrimSpace(status) {
	case models.SplitSettlementPending:
		return models.SplitSettlementPending
	case models.SplitSettlementDenied:
		return models.SplitSettlementDenied
	default:
		return models.SplitSettlementConfirmed
	}
}

// splitSettlementCounterparty is the Finnri account on the other side of a
// payment, when there is one.
//
// Nil has a precise meaning here and is not a failure: a friend row with no
// linked account stands for somebody who is not on Finnri, and a settlement
// naming them has nobody to confirm it. Asking for a confirmation that can
// never arrive would leave the row pending forever, so those are born agreed.
func splitSettlementCounterparty(db *gorm.DB, userID, friendID uint) (*uint, error) {
	var friend models.SplitFriend
	if err := db.Select("id", "user_id", "linked_user_id").
		Where("id = ? AND user_id = ?", friendID, userID).
		First(&friend).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	if friend.LinkedUserID == nil || *friend.LinkedUserID == 0 || *friend.LinkedUserID == userID {
		return nil, nil
	}
	linked := *friend.LinkedUserID
	return &linked, nil
}

// settlementCounterpartySentence states the payment from the point of view of
// the person being asked about it, which is the opposite of the one it was
// written in. `friend_paid_user` means the recorder says *you* paid them.
func settlementCounterpartySentence(direction string, amount models.Money, recorder string) string {
	switch direction {
	case settlementDirectionFriendPaidUser:
		return fmt.Sprintf("%s says you paid them ₹%s.", recorder, amount.String())
	case settlementDirectionUserPaidFriend:
		return fmt.Sprintf("%s says they paid you ₹%s.", recorder, amount.String())
	default:
		return fmt.Sprintf("%s recorded a settlement of ₹%s.", recorder, amount.String())
	}
}

// settlementGroupName is only ever used to decorate a message, so a group that
// cannot be read degrades to no name rather than to an error.
func settlementGroupName(db *gorm.DB, groupID *uint) string {
	if groupID == nil {
		return ""
	}
	var group models.SplitGroup
	if err := db.Select("id", "name").First(&group, *groupID).Error; err != nil {
		return ""
	}
	return group.Name
}

// notifySettlementRecorded tells the other side that a payment has been
// recorded against them, and that it is theirs to accept.
func notifySettlementRecorded(db *gorm.DB, settlement models.SplitSettlement, recorder models.User) {
	if settlement.CounterpartyUserID == nil {
		return
	}
	name := displayNameForUser(recorder)
	title := fmt.Sprintf("%s recorded a settlement", name)
	if groupName := settlementGroupName(db, settlement.GroupID); groupName != "" {
		title = fmt.Sprintf("%s settled up in %s", name, groupName)
	}
	_ = createNotification(
		*settlement.CounterpartyUserID,
		"split.settlement.recorded",
		title,
		settlementCounterpartySentence(settlement.Direction, settlement.Amount, name)+" Confirm it if that is right, or deny it if it is not.",
		fmt.Sprintf("/split/settlements/%d", settlement.ID),
	)
}

// notifySettlementDecision closes the loop back to whoever recorded the
// payment. A denial that only changed a row would leave them looking at a
// balance that moved for no reason they can see.
func notifySettlementDecision(db *gorm.DB, settlement models.SplitSettlement, responder models.User, confirmed bool) {
	name := displayNameForUser(responder)
	suffix := ""
	if groupName := settlementGroupName(db, settlement.GroupID); groupName != "" {
		suffix = fmt.Sprintf(" in %s", groupName)
	}
	if confirmed {
		_ = createNotification(
			settlement.UserID,
			"split.settlement.confirmed",
			fmt.Sprintf("%s confirmed your settlement", name),
			fmt.Sprintf("The ₹%s settlement you recorded%s is confirmed. The balance is settled.", settlement.Amount.String(), suffix),
			fmt.Sprintf("/split/settlements/%d", settlement.ID),
		)
		return
	}
	_ = createNotification(
		settlement.UserID,
		"split.settlement.denied",
		fmt.Sprintf("%s denied your settlement", name),
		fmt.Sprintf("The ₹%s settlement you recorded%s was denied, so the balance is back to what it was. Check with %s.", settlement.Amount.String(), suffix, name),
		fmt.Sprintf("/split/settlements/%d", settlement.ID),
	)
}

// listPendingSplitSettlements answers "what am I being asked to agree to?".
//
// Deliberately its own endpoint rather than a filter on the settlements list:
// that list is scoped to rows the caller *wrote*, and every row here was
// written by somebody else about the caller.
func (s *Server) listPendingSplitSettlements(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var settlements []models.SplitSettlement
	if err := database.DB.
		Where("counterparty_user_id = ? AND status = ?", userID, models.SplitSettlementPending).
		Order("created_at desc").
		Find(&settlements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_pending_split_settlements"})
		return
	}
	decorateSettlementsForCounterparty(database.DB, settlements)
	c.JSON(http.StatusOK, gin.H{"settlements": settlements})
}

// decorateSettlementsForCounterparty fills the response-only names a decision
// prompt has to say out loud. Without them the app can only offer "a
// settlement" — an amount with no author and no group behind it.
func decorateSettlementsForCounterparty(db *gorm.DB, settlements []models.SplitSettlement) {
	if len(settlements) == 0 {
		return
	}
	userIDs := map[uint]struct{}{}
	groupIDs := map[uint]struct{}{}
	for _, settlement := range settlements {
		userIDs[settlement.UserID] = struct{}{}
		if settlement.GroupID != nil {
			groupIDs[*settlement.GroupID] = struct{}{}
		}
	}
	names := map[uint]string{}
	if len(userIDs) > 0 {
		ids := make([]uint, 0, len(userIDs))
		for id := range userIDs {
			ids = append(ids, id)
		}
		var users []models.User
		if err := db.Where("id IN ?", ids).Find(&users).Error; err == nil {
			for _, user := range users {
				names[user.ID] = displayNameForUser(user)
			}
		}
	}
	groupNames := map[uint]string{}
	if len(groupIDs) > 0 {
		ids := make([]uint, 0, len(groupIDs))
		for id := range groupIDs {
			ids = append(ids, id)
		}
		var groups []models.SplitGroup
		if err := db.Select("id", "name").Where("id IN ?", ids).Find(&groups).Error; err == nil {
			for _, group := range groups {
				groupNames[group.ID] = group.Name
			}
		}
	}
	for i := range settlements {
		settlements[i].Status = settlementStatusOrDefault(settlements[i].Status)
		settlements[i].RecordedByName = names[settlements[i].UserID]
		if settlements[i].GroupID != nil {
			settlements[i].GroupName = groupNames[*settlements[i].GroupID]
		}
	}
}

func (s *Server) confirmSplitSettlement(c *gin.Context) {
	s.decideSplitSettlement(c, true)
}

func (s *Server) denySplitSettlement(c *gin.Context) {
	s.decideSplitSettlement(c, false)
}

// decideSplitSettlement is the one place a pending settlement stops being
// pending.
//
// Only the named counterparty may answer, and only once: the row is moved with
// a status-guarded update rather than a read-then-save, so two taps arriving
// together cannot both win and send contradictory notifications back.
func (s *Server) decideSplitSettlement(c *gin.Context, confirmed bool) {
	userID := c.MustGet("userID").(uint)
	settlementID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}

	var settlement models.SplitSettlement
	if err := database.DB.First(&settlement, settlementID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "split_settlement_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "split_settlement_lookup_failed"})
		return
	}
	// A settlement the caller is not the counterparty of is not theirs to know
	// about, so this is a 404 rather than a 403.
	if settlement.CounterpartyUserID == nil || *settlement.CounterpartyUserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "split_settlement_not_found"})
		return
	}
	if settlementStatusOrDefault(settlement.Status) != models.SplitSettlementPending {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "split_settlement_already_decided",
			"status": settlementStatusOrDefault(settlement.Status),
		})
		return
	}

	status := models.SplitSettlementDenied
	if confirmed {
		status = models.SplitSettlementConfirmed
	}
	now := time.Now().UTC()
	result := database.DB.Model(&models.SplitSettlement{}).
		Where("id = ? AND counterparty_user_id = ? AND status = ?", settlement.ID, userID, models.SplitSettlementPending).
		Updates(map[string]any{"status": status, "responded_at": now})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_split_settlement"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "split_settlement_already_decided"})
		return
	}
	settlement.Status = status
	settlement.RespondedAt = &now

	// The prompt that sent the user here has been answered. Leaving it unread
	// is how a decided settlement kept its dot on the bell.
	_ = database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ? AND read_at IS NULL", userID, "split.settlement.recorded").
		Where("action_url = ?", fmt.Sprintf("/split/settlements/%d", settlement.ID)).
		Update("read_at", now).Error

	var responder models.User
	if err := database.DB.First(&responder, userID).Error; err == nil {
		notifySettlementDecision(database.DB, settlement, responder, confirmed)
	}

	decorated := []models.SplitSettlement{settlement}
	decorateSettlementsForCounterparty(database.DB, decorated)
	c.JSON(http.StatusOK, decorated[0])
}
