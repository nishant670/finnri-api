package http

import (
	"fmt"
	"log"

	"finnri/internal/models"

	"gorm.io/gorm"
)

// maxSplitGroupBalanceAlert keeps the limit to something a group could
// plausibly reach: ₹1 crore.
const maxSplitGroupBalanceAlert = models.Money(1_00_00_000 * 100)

// checkSplitGroupBalanceAlerts tells each member of a group about any balance
// in it that has reached the group's limit since they were last told.
//
// It is run after anything that moves a balance in the group — an expense
// added, edited or deleted, a settlement recorded, confirmed or denied — and
// after the limit itself changes. It reads the ledger as it now stands rather
// than diffing a before and after, so every one of those paths only has to
// call it once at the end, and running it twice is harmless.
//
// Balances are per member and per person: a member is told when what one
// person owes them, or what they owe one person, reaches the limit. A row in
// split_group_balance_alerts marks the pair as told; it is cleared once the
// balance falls back under the limit, so the next time it gets there is news
// again.
func checkSplitGroupBalanceAlerts(db *gorm.DB, groupIDs ...uint) {
	for _, groupID := range groupIDs {
		if groupID == 0 {
			continue
		}
		if err := checkSplitGroupBalanceAlert(db, groupID); err != nil {
			log.Printf("[WARN] split balance alert check failed group=%d: %v", groupID, err)
		}
	}
}

func checkSplitGroupBalanceAlert(db *gorm.DB, groupID uint) error {
	var group models.SplitGroup
	if err := db.Where("id = ? AND archived = ?", groupID, false).First(&group).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return err
	}
	if group.BalanceAlertAmount <= 0 {
		// Turning the alert off forgets who was told, so turning it back on
		// starts clean.
		return db.Where("group_id = ?", groupID).Delete(&models.SplitGroupBalanceAlert{}).Error
	}

	userIDs := []uint{group.UserID}
	var memberIDs []uint
	if err := db.Model(&models.SplitGroupUserMember{}).
		Where("group_id = ? AND status = ?", groupID, "active").
		Pluck("user_id", &memberIDs).Error; err != nil {
		return err
	}
	userIDs = append(userIDs, memberIDs...)

	for _, userID := range userIDs {
		if err := checkSplitGroupBalanceAlertFor(db, group, userID); err != nil {
			return err
		}
	}
	return nil
}

func checkSplitGroupBalanceAlertFor(db *gorm.DB, group models.SplitGroup, userID uint) error {
	allBalances, err := buildSplitGroupBalances(db, userID)
	if err != nil {
		return err
	}
	balances := allBalances[group.ID]

	var told []models.SplitGroupBalanceAlert
	if err := db.Where("group_id = ? AND user_id = ?", group.ID, userID).Find(&told).Error; err != nil {
		return err
	}
	alreadyTold := map[uint]models.SplitGroupBalanceAlert{}
	for _, row := range told {
		alreadyTold[row.FriendID] = row
	}

	var reached []uint
	for friendID, balance := range balances {
		if absMoney(balance) < group.BalanceAlertAmount {
			continue
		}
		if _, ok := alreadyTold[friendID]; ok {
			delete(alreadyTold, friendID)
			continue
		}
		reached = append(reached, friendID)
	}
	// Whatever is left was told before and is back under the limit.
	for _, row := range alreadyTold {
		if err := db.Delete(&models.SplitGroupBalanceAlert{}, row.ID).Error; err != nil {
			return err
		}
	}
	if len(reached) == 0 {
		return nil
	}

	var friends []models.SplitFriend
	if err := db.Where("id IN ?", reached).Find(&friends).Error; err != nil {
		return err
	}
	names := map[uint]string{}
	for _, friend := range friends {
		names[friend.ID] = fallbackSplitFriendName(friend)
	}

	for _, friendID := range reached {
		row := models.SplitGroupBalanceAlert{GroupID: group.ID, UserID: userID, FriendID: friendID}
		// The unique pair makes a concurrent second check a no-op rather than a
		// second notification.
		result := db.Where(row).FirstOrCreate(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		title, body := splitBalanceAlertCopy(group, names[friendID], balances[friendID])
		actionURL := fmt.Sprintf("/split/groups/%d", group.ID)
		if err := createNotification(userID, "split.balance_alert", title, body, actionURL); err != nil {
			return err
		}
	}
	return nil
}

// splitBalanceAlertCopy words the alert for the member reading it. A positive
// balance is money the person owes the reader.
func splitBalanceAlertCopy(group models.SplitGroup, name string, balance models.Money) (string, string) {
	if name == "" {
		name = "Someone"
	}
	title := fmt.Sprintf("Balance alert in %s", group.Name)
	limit := group.BalanceAlertAmount.String()
	if balance > 0 {
		return title, fmt.Sprintf("%s owes you ₹%s in %s, past the ₹%s alert.", name, balance.String(), group.Name, limit)
	}
	return title, fmt.Sprintf("You owe %s ₹%s in %s, past the ₹%s alert.", name, absMoney(balance).String(), group.Name, limit)
}

func derefUint(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}
