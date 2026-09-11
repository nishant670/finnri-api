package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"finnri/internal/identity"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type SplitFriend struct {
	ID              uint   `gorm:"primaryKey" json:"id"`
	UserID          uint   `gorm:"index;not null" json:"user_id"`
	Name            string `gorm:"not null" json:"name"`
	Email           string `json:"email"`
	Phone           string `json:"phone"`
	PhoneNormalized string `gorm:"type:varchar(10);index" json:"phone_normalized,omitempty"`
	Archived        bool   `gorm:"not null;default:false" json:"archived"`
	// Set when the person behind this friend row has their own Finnri account.
	// A friend row belongs to whoever wrote it, so this is the only link that
	// lets a shared group say "this member is you" to the right viewer.
	LinkedUserID *uint     `gorm:"index" json:"linked_user_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (friend *SplitFriend) BeforeSave(_ *gorm.DB) error {
	friend.PhoneNormalized = identity.NormalizePhone(friend.Phone)
	return nil
}

// SplitGroupDefaultSplitSlot names the group owner in a default split. Every
// other slot is a group member's friend id rendered as a string.
const SplitGroupDefaultSplitOwnerSlot = "owner"

// SplitGroupDefaultSplit is the group-wide split every new expense starts from.
//
// It is deliberately anchored on the owner rather than on "you": the row is
// read by every member, and a viewer-relative default would mean a different
// thing to each of them. Slots are the group owner plus the member friend ids,
// which is the same namespace group bills already validate participants
// against, so the stored default names the same people for everybody.
type SplitGroupDefaultSplit struct {
	// Payer is a slot: the owner, or a member friend id.
	Payer string `json:"payer"`
	// FullAmount is the "is owed the full amount" split, where the payer
	// carries none of the expense.
	FullAmount bool `json:"full_amount"`
	// Tab is one of equally, percentages, shares.
	Tab          string                        `json:"tab"`
	Participants []SplitGroupDefaultSplitShare `json:"participants"`
}

type SplitGroupDefaultSplitShare struct {
	Slot   string `json:"slot"`
	Weight string `json:"weight"`
}

// The default split is stored as one JSON document rather than a serializer
// tag: it is written through single-column updates as well as full saves, and
// only a Valuer/Scanner pair covers both paths.
func (split *SplitGroupDefaultSplit) Value() (driver.Value, error) {
	if split == nil {
		return nil, nil
	}
	raw, err := json.Marshal(*split)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

func (split *SplitGroupDefaultSplit) Scan(value any) error {
	if split == nil {
		return errors.New("split: cannot scan a default split into a nil pointer")
	}
	switch typed := value.(type) {
	case nil:
		*split = SplitGroupDefaultSplit{}
		return nil
	case []byte:
		return json.Unmarshal(typed, split)
	case string:
		return json.Unmarshal([]byte(typed), split)
	default:
		return fmt.Errorf("split: cannot scan %T into a default split", value)
	}
}

type SplitGroup struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	UserID   uint   `gorm:"index;not null" json:"user_id"`
	Name     string `gorm:"not null" json:"name"`
	Archived bool   `gorm:"not null;default:false" json:"archived"`
	// Kind is what the group is for — trip, home, couple, other. It shapes the
	// group screen (only a trip has dates), so it belongs to the group rather
	// than to whoever happens to be looking at it.
	Kind string `gorm:"type:varchar(16);not null;default:other" json:"kind"`
	// PhotoURL is the group's picture, hosted on our own upload origin. It is a
	// property of the group rather than of whoever is looking at it: a photo
	// kept on the device that picked it would be invisible to every other
	// member, which is not a group photo but a private note.
	PhotoURL string `gorm:"type:text;not null;default:''" json:"photo_url"`
	// DefaultSplit is stored as JSON text rather than a column per field: it is
	// read and written whole, and the shape is the client's to interpret.
	DefaultSplit *SplitGroupDefaultSplit `gorm:"type:text" json:"default_split"`
	Members      []SplitGroupMember      `json:"members,omitempty" gorm:"foreignKey:GroupID"`
	// What happened to the people just added, so the app never implies somebody
	// was told when nobody could be. Response-only, and only on a save.
	MemberInvites  []SplitGroupMemberInvite `gorm:"-" json:"member_invites,omitempty"`
	OwnerName      string                   `gorm:"-" json:"owner_name,omitempty"`
	ViewerFriendID *uint                    `gorm:"-" json:"viewer_friend_id,omitempty"`
	// How each of this group's slots reads in the viewer's own friend list.
	// Response-only, and only for a member: the owner needs no translation,
	// because the roster is already written in their namespace.
	ViewerSlotFriends map[string]uint `gorm:"-" json:"viewer_slot_friends,omitempty"`
	// This group's ledger as the viewer sees it, in their own friend rows.
	// Response-only, and filled on the groups list.
	ViewerBalances      []SplitGroupFriendBalance `gorm:"-" json:"viewer_balances,omitempty"`
	ViewerNetBalance    Money                     `gorm:"-" json:"viewer_net_balance"`
	ViewerRole          string                    `gorm:"-" json:"viewer_role,omitempty"`
	ViewerCanAddExpense bool                      `gorm:"-" json:"viewer_can_add_expense,omitempty"`
	ViewerCanManage     bool                      `gorm:"-" json:"viewer_can_manage,omitempty"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

// SplitGroupMemberInviteStatus values.
const (
	// The person has a Finnri account and now has an invite waiting in it.
	SplitMemberInviteNotified = "notified"
	// No account yet: the invite exists but the link has to be handed over.
	SplitMemberInviteLinkNeeded = "invite_created"
	// The friend row carries no email or phone, so there was nobody to reach.
	SplitMemberInviteNoContact = "no_contact"
)

type SplitGroupMemberInvite struct {
	FriendID uint   `json:"friend_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
}

type SplitGroupMember struct {
	ID        uint        `gorm:"primaryKey" json:"id"`
	UserID    uint        `gorm:"index;not null" json:"user_id"`
	GroupID   uint        `gorm:"index;not null" json:"group_id"`
	Group     SplitGroup  `json:"-" gorm:"foreignKey:GroupID;constraint:OnDelete:CASCADE"`
	FriendID  uint        `gorm:"index;not null" json:"friend_id"`
	Friend    SplitFriend `json:"friend,omitempty" gorm:"foreignKey:FriendID;constraint:OnDelete:CASCADE"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type SplitGroupInvite struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	UserID    uint       `gorm:"index;not null" json:"user_id"`
	GroupID   uint       `gorm:"index;not null" json:"group_id"`
	Group     SplitGroup `json:"group,omitempty" gorm:"foreignKey:GroupID;constraint:OnDelete:CASCADE"`
	Token     string     `gorm:"uniqueIndex;not null" json:"token"`
	Status    string     `gorm:"type:varchar(16);not null;default:active" json:"status"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type SplitGroupDirectInvite struct {
	ID       uint             `gorm:"primaryKey" json:"id"`
	UserID   uint             `gorm:"index;not null" json:"user_id"`
	GroupID  uint             `gorm:"index;not null" json:"group_id"`
	Group    SplitGroup       `json:"group,omitempty" gorm:"foreignKey:GroupID;constraint:OnDelete:CASCADE"`
	InviteID uint             `gorm:"index;not null" json:"invite_id"`
	Invite   SplitGroupInvite `json:"-" gorm:"foreignKey:InviteID;constraint:OnDelete:CASCADE"`
	// Which friend row this invite was raised for, when it came from adding a
	// specific person to the group. It is what lets acceptance reuse the row the
	// owner has already been splitting against instead of guessing at one.
	FriendID              *uint     `gorm:"index" json:"friend_id,omitempty"`
	TargetEmail           string    `gorm:"type:varchar(254);not null;default:''" json:"target_email"`
	TargetPhone           string    `gorm:"type:varchar(32);not null;default:''" json:"target_phone"`
	TargetPhoneNormalized string    `gorm:"type:varchar(10);index" json:"-"`
	InvitedUserID         *uint     `gorm:"index" json:"invited_user_id"`
	InvitedUser           *User     `json:"-" gorm:"foreignKey:InvitedUserID;constraint:OnDelete:SET NULL"`
	Status                string    `gorm:"type:varchar(16);not null;default:pending" json:"status"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func (invite *SplitGroupDirectInvite) BeforeSave(_ *gorm.DB) error {
	invite.TargetPhoneNormalized = identity.NormalizePhone(invite.TargetPhone)
	return nil
}

type SplitGroupUserMember struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	GroupID   uint       `gorm:"index;not null" json:"group_id"`
	Group     SplitGroup `json:"group,omitempty" gorm:"foreignKey:GroupID;constraint:OnDelete:CASCADE"`
	UserID    uint       `gorm:"index;not null" json:"user_id"`
	User      User       `json:"user,omitempty" gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	Role      string     `gorm:"type:varchar(16);not null;default:member" json:"role"`
	Status    string     `gorm:"type:varchar(16);not null;default:active" json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// SplitGroupMemberLink translates one slot of a shared group into a friend row
// the viewer actually owns.
//
// A group's roster is written in the *owner's* namespace: the owner themselves,
// plus their own friend rows. That namespace is unusable by anybody else,
// because `split_participants.friend_id` has to name a row its author owns. So
// a member had no way to record that the owner owed them, and their expense
// composer could only offer them their own member row back — which is how a
// member came to be splitting a bill between two copies of herself while the
// owner was not on the list at all.
//
// This table is the missing half. For every non-owner member there is one row
// per slot they are not, pointing at the row in their own friend list that
// stands for that person. The owner has no rows here and needs none.
type SplitGroupMemberLink struct {
	ID      uint       `gorm:"primaryKey" json:"id"`
	GroupID uint       `gorm:"index;not null" json:"group_id"`
	Group   SplitGroup `json:"-" gorm:"foreignKey:GroupID;constraint:OnDelete:CASCADE"`
	// Slot names the person in the owner's namespace: the owner slot, or an
	// owner-side split_friends.id rendered as text. Deliberately the same
	// namespace SplitGroupDefaultSplit uses, so a stored default split and a
	// link always mean the same person by the same name.
	Slot string `gorm:"type:varchar(32);not null" json:"slot"`
	// The member this row translates for. Never the group's owner.
	UserID uint `gorm:"index;not null" json:"user_id"`
	User   User `json:"-" gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	// That member's own friend row standing for the person in the slot.
	FriendID uint        `gorm:"index;not null" json:"friend_id"`
	Friend   SplitFriend `json:"friend,omitempty" gorm:"foreignKey:FriendID;constraint:OnDelete:CASCADE"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (SplitGroupMemberLink) TableName() string { return "split_group_member_links" }

// SplitGroupFriendBalance is one person's net inside one group, from the
// viewer's side: positive means that person owes the viewer.
//
// Served rather than derived on the client. A bill's `direction` is written
// relative to whoever recorded it, so summing a group's participant rows
// without knowing each bill's author inverts the sign of everybody else's
// expenses — which is what the group cards were doing.
type SplitGroupFriendBalance struct {
	FriendID   uint  `json:"friend_id"`
	NetBalance Money `json:"net_balance"`
}

type SplitBill struct {
	ID              uint               `gorm:"primaryKey" json:"id"`
	UserID          uint               `gorm:"index;not null" json:"user_id"`
	EntryID         *uint              `gorm:"index" json:"entry_id"`
	Entry           *Entry             `json:"entry,omitempty" gorm:"foreignKey:EntryID;constraint:OnDelete:SET NULL"`
	GroupID         *uint              `gorm:"index" json:"group_id"`
	Group           *SplitGroup        `json:"group,omitempty" gorm:"foreignKey:GroupID;constraint:OnDelete:SET NULL"`
	Title           string             `gorm:"not null" json:"title"`
	TotalAmount     Money              `gorm:"type:numeric(19,2);not null" json:"total_amount"`
	Currency        string             `gorm:"type:char(3);not null;default:INR" json:"currency"`
	Date            string             `gorm:"not null" json:"date"`
	Notes           string             `json:"notes"`
	Participants    []SplitParticipant `json:"participants,omitempty" gorm:"foreignKey:BillID"`
	ViewerCanEdit   bool               `gorm:"-" json:"viewer_can_edit,omitempty"`
	ViewerCanDelete bool               `gorm:"-" json:"viewer_can_delete,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type SplitParticipant struct {
	ID          uint        `gorm:"primaryKey" json:"id"`
	UserID      uint        `gorm:"index;not null" json:"user_id"`
	BillID      uint        `gorm:"index;not null" json:"bill_id"`
	Bill        SplitBill   `json:"-" gorm:"foreignKey:BillID;constraint:OnDelete:CASCADE"`
	FriendID    uint        `gorm:"index;not null" json:"friend_id"`
	Friend      SplitFriend `json:"friend,omitempty" gorm:"foreignKey:FriendID;constraint:OnDelete:CASCADE"`
	ShareAmount Money       `gorm:"type:numeric(19,2);not null" json:"share_amount"`
	Direction   string      `gorm:"type:varchar(24);not null" json:"direction"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// SplitSettlement records a payment that closes part of a running balance.
//
// GroupID is what ties it to the expenses it settles. Without it a settlement
// outlived the ledger it belonged to: deleting a group removes its bills and
// participants, and a friend-level settlement left behind kept contributing its
// full amount to the running total — so an account with no groups, no bills and
// nothing on screen still reported being owed money, with nothing anywhere to
// explain the figure.
type SplitSettlement struct {
	ID       uint        `gorm:"primaryKey" json:"id"`
	UserID   uint        `gorm:"index;not null" json:"user_id"`
	FriendID uint        `gorm:"index;not null" json:"friend_id"`
	Friend   SplitFriend `json:"friend,omitempty" gorm:"foreignKey:FriendID;constraint:OnDelete:CASCADE"`
	// Nil for a settlement recorded straight against a friend rather than
	// inside a group. Those are still deleted with their friend, but they
	// survive a group being removed, which is correct: they never named one.
	//
	// Deliberately carries no `index` tag and no association struct. The
	// partial index and the cascading foreign key are created by
	// `EnsureRuntimeSchema`, and declaring them here as well is not duplication
	// so much as a fight over names: AutoMigrate expects
	// `fk_split_settlements_group`, hand-written SQL gets Postgres's
	// `split_settlements_group_id_fkey`, and AutoMigrate then tries to drop a
	// constraint that does not exist under the name it looked for and aborts
	// boot. Nothing preloads the group, so the association bought nothing.
	GroupID   *uint     `json:"group_id,omitempty"`
	Amount    Money     `gorm:"type:numeric(19,2);not null" json:"amount"`
	Direction string    `gorm:"type:varchar(24);not null" json:"direction"`
	Date      string    `gorm:"not null" json:"date"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SplitFriendMerge remembers that one friend row was folded into another.
//
// A merge archives the loser, and every id the app was already holding — an
// expense composer left open, a draft on another device — instantly names a row
// the server will refuse. The user sees "must belong to the current user" about
// somebody who is plainly in the group, with no way to act on it: the row it
// names no longer exists to fix.
//
// Keeping the redirect is what lets a stale id be resolved to the row that
// absorbed it instead of rejected. Merges chain (a into b, later b into c), so
// resolution follows the trail rather than taking one hop.
type SplitFriendMerge struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// The owner of both rows. A merge is always within one user's friend list.
	//
	// Every index on this table is created by `EnsureRuntimeSchema` — see
	// SplitSettlement.GroupID for why the schema owns them rather than the
	// tags. The composite (user_id, to_friend_id) serves the rewrite a chained
	// merge performs; single-column tags here would only duplicate half of it.
	UserID uint `gorm:"not null" json:"user_id"`
	// FromFriendID is the row that was archived. A row can only be merged away
	// once, and that uniqueness is a unique index the schema creates — not a
	// `uniqueIndex` tag, for the reason spelled out on SplitSettlement.GroupID.
	FromFriendID uint      `gorm:"not null" json:"from_friend_id"`
	ToFriendID   uint      `gorm:"not null" json:"to_friend_id"`
	CreatedAt    time.Time `json:"created_at"`
}
