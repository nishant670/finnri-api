package http

import (
	"finnri/internal/models"

	"gorm.io/gorm"
)

// This file answers one question for every screen that draws a split expense:
// what does this bill say about *me*?
//
// It is not the question the database stores. A bill records the debts of
// whoever wrote it, against friend rows only they own, with `direction` stated
// from their side. That is a complete record and a useless one to everybody
// else: read literally by another member, every expense the group's owner
// entered claimed "You paid ₹5,880", counted her ₹2,352 of it as money she had
// lent, and named the payer with a friend row belonging to a different
// account. The group header was right the whole time, because the header comes
// from the folded ledger the server computes — and the list underneath it
// disagreed with it, line by line.
//
// So the restatement happens here, once, next to the fold that already gets
// the balances right, rather than being attempted again in every sheet.

// decorateSplitBillsForViewer fills in ViewerShares on every bill it is given.
//
// Bills whose group the viewer is not part of, and bills written by somebody
// whose place in the group cannot be established, are left with no shares
// rather than a guess: an empty restatement is a screen that says less, and a
// wrong one is a screen that lies.
func decorateSplitBillsForViewer(db *gorm.DB, bills []models.SplitBill, viewerUserID uint) error {
	if len(bills) == 0 {
		return nil
	}

	groupIDSet := map[uint]bool{}
	for index := range bills {
		if bills[index].GroupID != nil && *bills[index].GroupID != 0 {
			groupIDSet[*bills[index].GroupID] = true
		}
	}
	groupIDs := make([]uint, 0, len(groupIDSet))
	for groupID := range groupIDSet {
		groupIDs = append(groupIDs, groupID)
	}
	frames, err := loadSplitGroupFrames(db, groupIDs)
	if err != nil {
		return err
	}

	// Every name on these screens is the reader's own name for the person, not
	// the one the bill's author uses. Loaded whole rather than per bill: a
	// group's expense list walks the same handful of people over and over.
	var viewerFriends []models.SplitFriend
	if err := db.Where("user_id = ?", viewerUserID).Find(&viewerFriends).Error; err != nil {
		return err
	}
	nameByFriendID := make(map[uint]string, len(viewerFriends))
	for _, friend := range viewerFriends {
		nameByFriendID[friend.ID] = fallbackSplitFriendName(friend)
	}

	for index := range bills {
		bill := &bills[index]
		if bill.GroupID == nil || *bill.GroupID == 0 {
			bill.ViewerShares = ownSplitBillViewerShares(*bill, viewerUserID, nameByFriendID)
			continue
		}
		frame, ok := frames[*bill.GroupID]
		if !ok {
			continue
		}
		bill.ViewerShares = groupSplitBillViewerShares(frame, *bill, viewerUserID, nameByFriendID)
	}
	return nil
}

// splitBillSlotTotals is who paid and who carries what, in slot space.
type splitBillSlotTotals struct {
	payerSlot string
	order     []string
	share     map[string]models.Money
}

// splitBillSlots reads a bill's participant rows into slot space.
//
// A bill states only its author's own debts, so it comes in two shapes and the
// same walk covers both:
//
//   - The author paid. Every row is somebody who owes them, and the author's
//     own share is whatever the rows leave over.
//   - Somebody else paid. There is one row — what the author owes that person —
//     and the payer's share is, again, the remainder.
//
// Hence the single rule at the end: the payer carries what nobody else was
// recorded as carrying. It is exact when the author paid, and the best the
// record supports when they did not, because a bill has no way to name what a
// third person owes a second one.
func splitBillSlots(bill models.SplitBill, authorSlot string, slotOfFriend map[uint]string) splitBillSlotTotals {
	totals := splitBillSlotTotals{
		payerSlot: authorSlot,
		order:     []string{authorSlot},
		share:     map[string]models.Money{},
	}
	seen := map[string]bool{authorSlot: true}

	for _, participant := range bill.Participants {
		slot, ok := slotOfFriend[participant.FriendID]
		if !ok {
			continue
		}
		if !seen[slot] {
			seen[slot] = true
			totals.order = append(totals.order, slot)
		}
		if participant.Direction == splitDirectionUserOwesFriend {
			totals.payerSlot = slot
			totals.share[authorSlot] += participant.ShareAmount
			continue
		}
		totals.share[slot] += participant.ShareAmount
	}

	var allocated models.Money
	for _, amount := range totals.share {
		allocated += amount
	}
	if remainder := bill.TotalAmount - allocated; remainder > 0 {
		totals.share[totals.payerSlot] += remainder
	}
	return totals
}

// groupSplitBillViewerShares restates one group bill for one reader.
func groupSplitBillViewerShares(
	frame splitGroupFrame,
	bill models.SplitBill,
	viewerUserID uint,
	nameByFriendID map[uint]string,
) []models.SplitBillViewerShare {
	authorSlot, ok := frame.slotOfUser[bill.UserID]
	if !ok {
		return nil
	}
	viewerSlot, ok := frame.slotOfUser[viewerUserID]
	if !ok {
		return nil
	}

	totals := splitBillSlots(bill, authorSlot, frame.slotsForUser(bill.UserID))
	shares := make([]models.SplitBillViewerShare, 0, len(totals.order))
	for _, slot := range totals.order {
		share := models.SplitBillViewerShare{
			Slot:     slot,
			IsViewer: slot == viewerSlot,
			Share:    totals.share[slot],
		}
		if slot == totals.payerSlot {
			share.Paid = bill.TotalAmount
		}
		if !share.IsViewer {
			share.FriendID = frame.friendFor(slot, viewerUserID)
			share.Name = nameByFriendID[share.FriendID]
			if share.Name == "" {
				share.Name = frame.slotName[slot]
			}
		}
		shares = append(shares, share)
	}
	return shares
}

// ownSplitBillViewerShares restates a bill that names no group.
//
// Those are only ever the reader's own, so the friend ids on them are already
// the reader's — but the shape of the answer has to match a group bill's, or
// every screen would need two readings of the same list again.
func ownSplitBillViewerShares(
	bill models.SplitBill,
	viewerUserID uint,
	nameByFriendID map[uint]string,
) []models.SplitBillViewerShare {
	if bill.UserID != viewerUserID {
		return nil
	}
	slotOfFriend := make(map[uint]string, len(bill.Participants))
	for _, participant := range bill.Participants {
		slotOfFriend[participant.FriendID] = splitGroupSlotForFriend(participant.FriendID)
	}

	totals := splitBillSlots(bill, models.SplitBillViewerSelfSlot, slotOfFriend)
	shares := make([]models.SplitBillViewerShare, 0, len(totals.order))
	for _, slot := range totals.order {
		share := models.SplitBillViewerShare{
			Slot:     slot,
			IsViewer: slot == models.SplitBillViewerSelfSlot,
			Share:    totals.share[slot],
		}
		if slot == totals.payerSlot {
			share.Paid = bill.TotalAmount
		}
		if !share.IsViewer {
			for _, participant := range bill.Participants {
				if splitGroupSlotForFriend(participant.FriendID) == slot {
					share.FriendID = participant.FriendID
					break
				}
			}
			share.Name = nameByFriendID[share.FriendID]
			if share.Name == "" {
				// An archived row, or one merged away: still on the bill, and
				// still worth naming as somebody rather than as nothing.
				share.Name = "Friend"
			}
		}
		shares = append(shares, share)
	}
	return shares
}
