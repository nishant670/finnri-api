package http

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"finnri/internal/models"
)

type entryInput struct {
	Title       string             `json:"title"`
	Type        string             `json:"type"`
	Amount      models.Money       `json:"amount"`
	Currency    string             `json:"currency"`
	Source      string             `json:"source"`
	Mode        string             `json:"mode"`
	CardNetwork string             `json:"card_network"`
	Category    string             `json:"category"`
	Merchant    string             `json:"merchant"`
	PurposeType string             `json:"purpose_type"`
	Tag         string             `json:"tag"`
	Tags        models.StringArray `json:"tags"`
	Notes       string             `json:"notes"`
	Date        string             `json:"date"`
	Time        string             `json:"time"`
	SourceText  string             `json:"source_text"`
	Attachment  string             `json:"attachment"`
	AccountID   *uint              `json:"account_id"`
	Split       *entrySplitInput   `json:"split"`
}

type entrySplitInput struct {
	GroupID      *uint                        `json:"group_id"`
	GroupName    string                       `json:"group_name"`
	Notes        string                       `json:"notes"`
	Participants []entrySplitParticipantInput `json:"participants"`
}

type entrySplitParticipantInput struct {
	FriendID    *uint            `json:"friend_id"`
	Friend      splitFriendInput `json:"friend"`
	ShareAmount models.Money     `json:"share_amount"`
	Direction   string           `json:"direction"`
}

type updateEntryInput struct {
	Title       *string             `json:"title"`
	Type        *string             `json:"type"`
	Amount      *models.Money       `json:"amount"`
	Currency    *string             `json:"currency"`
	Source      *string             `json:"source"`
	Mode        *string             `json:"mode"`
	CardNetwork *string             `json:"card_network"`
	Category    *string             `json:"category"`
	Merchant    *string             `json:"merchant"`
	PurposeType *string             `json:"purpose_type"`
	Tag         *string             `json:"tag"`
	Tags        *models.StringArray `json:"tags"`
	Notes       *string             `json:"notes"`
	Date        *string             `json:"date"`
	Time        *string             `json:"time"`
	SourceText  *string             `json:"source_text"`
	Attachment  *string             `json:"attachment"`
	AccountID   optionalAccountID   `json:"account_id"`
	Split       optionalEntrySplit  `json:"split"`
}

type optionalAccountID struct {
	Set   bool
	Value *uint
}

type optionalEntrySplit struct {
	Set   bool
	Value *entrySplitInput
}

func (field *optionalAccountID) UnmarshalJSON(data []byte) error {
	field.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		field.Value = nil
		return nil
	}

	var value uint
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	field.Value = &value
	return nil
}

func (field *optionalEntrySplit) UnmarshalJSON(data []byte) error {
	field.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		field.Value = nil
		return nil
	}

	var value entrySplitInput
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	field.Value = &value
	return nil
}

func validateEntryValues(amount models.Money, title, entryType, currency, source, mode, category, date string) map[string]string {
	fields := map[string]string{}
	if !amount.IsPositive() {
		fields["amount"] = "must be greater than zero"
	}
	switch strings.ToLower(entryType) {
	case "expense", "income":
	default:
		fields["type"] = "must be expense or income"
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		fields["date"] = "must use YYYY-MM-DD"
	}
	if normalized := strings.ToUpper(strings.TrimSpace(currency)); normalized != "" && normalized != "INR" {
		fields["currency"] = "must be INR"
	}
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "manual", "text", "voice":
	default:
		fields["source"] = "must be manual, text, or voice"
	}
	if strings.TrimSpace(title) == "" {
		fields["title"] = "is required"
	}
	// Mode may be omitted from an entry request because account_id already
	// carries the payment source. The handler derives it after verifying account
	// ownership. A supplied value must still be canonical (or a known alias).
	if strings.TrimSpace(mode) != "" {
		if _, ok := canonicalMode(mode); !ok {
			fields["mode"] = modeMessage()
		}
	}
	if strings.TrimSpace(category) == "" {
		fields["category"] = "is required"
	} else if _, ok := categoryForSave(category, entryType); !ok {
		// Custom categories from the picker are allowed through; only the length
		// is bounded. Legacy names like "Food" are accepted and canonicalized by
		// toModel, which keeps older app builds saving successfully.
		fields["category"] = categoryLengthMessage()
	}
	return fields
}

func (input entryInput) validate() map[string]string {
	fields := validateEntryValues(
		input.Amount, input.Title, input.Type, input.Currency,
		input.Source, input.Mode, input.Category, input.Date,
	)
	if input.AccountID != nil && *input.AccountID == 0 {
		fields["account_id"] = "must be a positive integer"
	}
	if input.AccountID == nil && strings.TrimSpace(input.Mode) == "" {
		fields["mode"] = "is required when account_id is omitted"
	}
	for field, message := range input.Split.validate() {
		fields[field] = message
	}
	if input.Split != nil {
		if !strings.EqualFold(input.Type, "expense") {
			fields["split"] = "can be added only to expenses"
		}
		totalShares := models.Money(0)
		for _, participant := range input.Split.Participants {
			totalShares += participant.ShareAmount
		}
		if totalShares > input.Amount {
			fields["split.participants"] = "shares must not exceed transaction amount"
		}
	}
	return fields
}

func (input *entrySplitInput) validate() map[string]string {
	fields := map[string]string{}
	if input == nil {
		return fields
	}
	if len(input.Participants) == 0 {
		fields["split.participants"] = "must include at least one friend share"
	}
	if input.GroupID != nil && *input.GroupID == 0 {
		fields["split.group_id"] = "must be a positive integer"
	}
	seen := map[string]bool{}
	for index, participant := range input.Participants {
		prefix := "split.participants[" + strconv.Itoa(index) + "]"
		if participant.FriendID == nil && strings.TrimSpace(participant.Friend.Name) == "" {
			fields[prefix+".friend"] = "must include friend_id or friend.name"
		}
		if participant.FriendID != nil && *participant.FriendID == 0 {
			fields[prefix+".friend_id"] = "must be a positive integer"
		}
		if !participant.ShareAmount.IsPositive() {
			fields[prefix+".share_amount"] = "must be positive"
		}
		direction := normalizeSplitDirection(participant.Direction)
		if strings.TrimSpace(participant.Direction) == "" {
			direction = splitDirectionFriendOwesUser
		}
		if direction == "" {
			fields[prefix+".direction"] = "must be friend_owes_user or user_owes_friend"
		}
		key := strings.ToLower(strings.TrimSpace(participant.Friend.Name))
		if participant.FriendID != nil {
			key = strconv.Itoa(int(*participant.FriendID))
		}
		key += ":" + direction
		if seen[key] {
			fields[prefix+".friend"] = "duplicate friend and direction"
		}
		seen[key] = true
	}
	return fields
}

func (input entryInput) toModel(userID uint) models.Entry {
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))
	if currency == "" {
		currency = "INR"
	}
	source := strings.ToLower(strings.TrimSpace(input.Source))
	if source == "" {
		source = "manual"
	}
	// Rewrites recognised aliases to canonical form and passes a user's own
	// custom category through untouched.
	category := input.Category
	if resolved, ok := categoryForSave(category, input.Type); ok {
		category = resolved
	}
	return models.Entry{
		Title: input.Title, Type: strings.ToLower(input.Type), Amount: input.Amount,
		Currency: currency, Source: source, Mode: input.Mode, CardNetwork: input.CardNetwork,
		Category: category, Merchant: input.Merchant, PurposeType: input.PurposeType,
		Tag: input.Tag, Tags: input.Tags, Notes: input.Notes, Date: input.Date,
		Time: input.Time, SourceText: input.SourceText, Attachment: input.Attachment,
		AccountID: input.AccountID, UserID: userID,
	}
}
