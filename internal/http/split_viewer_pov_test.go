package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// billViewerShare pulls one person's line out of a bill's restatement.
func billViewerShare(
	t *testing.T,
	bill models.SplitBill,
	match func(models.SplitBillViewerShare) bool,
) models.SplitBillViewerShare {
	t.Helper()
	for _, share := range bill.ViewerShares {
		if match(share) {
			return share
		}
	}
	t.Fatalf("no matching viewer share on bill %q: %#v", bill.Title, bill.ViewerShares)
	return models.SplitBillViewerShare{}
}

func groupBill(t *testing.T, router *gin.Engine, token string, billID uint) models.SplitBill {
	t.Helper()
	bills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	for _, bill := range bills {
		if bill.ID == billID {
			return bill
		}
	}
	t.Fatalf("bill %d is not in this user's list: %#v", billID, bills)
	return models.SplitBill{}
}

// The reported bug, from the screenshots: on the member's phone every expense
// the owner had entered read "You paid ₹5,880 … you lent ₹2,352". Both halves
// were the owner's line, shown to somebody who had neither paid nor lent.
func TestGroupBillReadsFromEachMembersOwnSide(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The owner pays 5880 and puts 2352 of it on the member — the 60/40 the
	// group is set to.
	created := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title": "Curtains final payment", "total_amount": "5880.00", "currency": "INR",
			"date": "2026-09-01", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    ownerFriendForMember.ID,
					"share_amount": "2352.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	ownerView := groupBill(t, router, ownerToken, created.ID)
	ownerSelf := billViewerShare(t, ownerView, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if ownerSelf.Paid.String() != "5880.00" || ownerSelf.Share.String() != "3528.00" {
		t.Fatalf("owner should have paid 5880 and carry 3528, got %#v", ownerSelf)
	}

	memberView := groupBill(t, router, memberToken, created.ID)
	memberSelf := billViewerShare(t, memberView, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if memberSelf.Paid.String() != "0.00" {
		t.Fatalf("the member paid nothing on this bill, got %q", memberSelf.Paid.String())
	}
	if memberSelf.Share.String() != "2352.00" {
		t.Fatalf("the member carries 2352 of this bill, got %q", memberSelf.Share.String())
	}

	// And the payer, on her phone, is the owner named by her own friend row —
	// the only name her screens can resolve.
	payer := billViewerShare(t, memberView, func(s models.SplitBillViewerShare) bool {
		return s.Paid.String() == "5880.00"
	})
	if payer.IsViewer {
		t.Fatalf("the member did not pay this bill: %#v", payer)
	}
	if payer.FriendID != memberFriendForOwner {
		t.Fatalf("payer should be the member's own row %d for the owner, got %#v",
			memberFriendForOwner, payer)
	}
	if payer.Share.String() != "3528.00" {
		t.Fatalf("the owner carries 3528 of his own bill, got %q", payer.Share.String())
	}
}

// The mirror image: an expense the member entered, read by the owner.
func TestGroupBillEnteredByAMemberReadsCorrectlyForTheOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	created := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Grocery", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-11", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	ownerView := groupBill(t, router, ownerToken, created.ID)
	ownerSelf := billViewerShare(t, ownerView, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if ownerSelf.Paid.String() != "0.00" || ownerSelf.Share.String() != "600.00" {
		t.Fatalf("the owner paid nothing and carries 600, got %#v", ownerSelf)
	}
	payer := billViewerShare(t, ownerView, func(s models.SplitBillViewerShare) bool {
		return s.Paid.String() == "1000.00"
	})
	if payer.FriendID != ownerFriendForMember.ID {
		t.Fatalf("payer should be the owner's row %d for the member, got %#v",
			ownerFriendForMember.ID, payer)
	}
	if payer.Share.String() != "400.00" {
		t.Fatalf("the member carries 400 of her own bill, got %q", payer.Share.String())
	}
}

// A bill somebody else paid, recorded by its author as their own debt. Only the
// author's share is on the record, so the payer's is the remainder — which is
// the whole of what is left, and stays right for the two-person case the app
// actually produces.
func TestGroupBillRecordedAgainstAnotherPayerReadsBothWays(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The member records that the owner paid 500 and 200 of it is hers.
	created := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Dinner", "total_amount": "500.00", "currency": "INR",
			"date": "2026-09-12", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "200.00",
					"direction":    splitDirectionUserOwesFriend,
				},
			},
		}, http.StatusCreated,
	)

	memberView := groupBill(t, router, memberToken, created.ID)
	memberSelf := billViewerShare(t, memberView, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if memberSelf.Paid.String() != "0.00" || memberSelf.Share.String() != "200.00" {
		t.Fatalf("the member paid nothing and carries 200, got %#v", memberSelf)
	}

	ownerView := groupBill(t, router, ownerToken, created.ID)
	ownerSelf := billViewerShare(t, ownerView, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if ownerSelf.Paid.String() != "500.00" {
		t.Fatalf("the owner is the payer on this bill, got %q", ownerSelf.Paid.String())
	}
	if ownerSelf.Share.String() != "300.00" {
		t.Fatalf("the owner carries the 300 the member's share leaves over, got %q",
			ownerSelf.Share.String())
	}
	other := billViewerShare(t, ownerView, func(s models.SplitBillViewerShare) bool { return !s.IsViewer })
	if other.FriendID != ownerFriendForMember.ID || other.Share.String() != "200.00" {
		t.Fatalf("the member should carry 200, named by the owner's own row: %#v", other)
	}
}

// The group settings screen listed one person — the reader — and left out the
// owner entirely, because it was reading the owner's own membership rows.
func TestGroupRosterNamesEveryoneForEveryMember(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	rosterFor := func(token string) []models.SplitGroupViewerMember {
		groups := performJSONRequest[[]models.SplitGroup](
			t, router, http.MethodGet, "/v1/split/groups", token, nil, http.StatusOK,
		)
		for _, candidate := range groups {
			if candidate.ID == group.ID {
				return candidate.ViewerMembers
			}
		}
		t.Fatalf("group %d missing from %s's list", group.ID, token)
		return nil
	}

	for label, roster := range map[string][]models.SplitGroupViewerMember{
		"owner":  rosterFor(ownerToken),
		"member": rosterFor(memberToken),
	} {
		if len(roster) != 2 {
			t.Fatalf("%s should see two people in the group, got %#v", label, roster)
		}
		viewers := 0
		for _, entry := range roster {
			if entry.IsViewer {
				viewers++
			}
		}
		if viewers != 1 {
			t.Fatalf("%s's roster should mark exactly one row as themselves: %#v", label, roster)
		}
	}

	ownerRoster := rosterFor(ownerToken)
	if ownerRoster[0].Slot != models.SplitGroupDefaultSplitOwnerSlot || !ownerRoster[0].IsViewer {
		t.Fatalf("the owner is their own group's owner slot: %#v", ownerRoster)
	}
	if ownerRoster[1].FriendID != ownerFriendForMember.ID {
		t.Fatalf("the owner names the member by their own row: %#v", ownerRoster)
	}

	memberRoster := rosterFor(memberToken)
	if memberRoster[0].FriendID != memberFriendForOwner {
		t.Fatalf("the member names the owner by their own row %d: %#v",
			memberFriendForOwner, memberRoster)
	}
	if memberRoster[0].IsViewer {
		t.Fatalf("the owner slot is not the member: %#v", memberRoster)
	}
	if !memberRoster[1].IsViewer || memberRoster[1].FriendID != 0 {
		t.Fatalf("the member's own slot carries no friend row of hers: %#v", memberRoster)
	}
	if memberRoster[1].Slot != fmt.Sprintf("%d", ownerFriendForMember.ID) {
		t.Fatalf("the member's slot is her row in the owner's namespace: %#v", memberRoster)
	}
}

// A bill with no group is only ever its own author's, but it has to come back
// in the same shape as a group bill or every screen needs two readings again.
func TestUngroupedBillStillCarriesAViewerRestatement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, token := createPaidBillingTestUserSession(t)
	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Ravi"}, http.StatusCreated,
	)
	created := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"title": "Cab", "total_amount": "300.00", "currency": "INR",
			"date": "2026-09-12",
			"participants": []map[string]any{
				{
					"friend_id":    friend.ID,
					"share_amount": "150.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	view := groupBill(t, router, token, created.ID)
	self := billViewerShare(t, view, func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if self.Paid.String() != "300.00" || self.Share.String() != "150.00" {
		t.Fatalf("the author paid 300 and carries 150, got %#v", self)
	}
	other := billViewerShare(t, view, func(s models.SplitBillViewerShare) bool { return !s.IsViewer })
	if other.FriendID != friend.ID || other.Share.String() != "150.00" {
		t.Fatalf("the friend carries 150, got %#v", other)
	}
}
