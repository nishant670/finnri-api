package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// Card art tones, orientations and networks. schema.go builds its CHECK
// constraints from these lists, so the handlers and the database cannot
// disagree.
const (
	CardToneDeep  = "deep"  // the mood's dark background mixed with its accent
	CardToneSolid = "solid" // the accent itself
	CardToneTint  = "tint"  // the card colour mixed with the accent
	CardTonePale  = "pale"  // near-white with a touch of accent

	CardOrientationLandscape = "landscape"
	CardOrientationPortrait  = "portrait"
)

var (
	CardTones        = []string{CardToneDeep, CardToneSolid, CardToneTint, CardTonePale}
	CardOrientations = []string{CardOrientationLandscape, CardOrientationPortrait}
	// CardNetworks is shown on the card as plain text ("Visa •• 4867"). Never
	// a network logo: see docs/CREDIT_CARD_SYSTEM_PLAN_2026-11-13.md §4.1.
	CardNetworks = []string{"visa", "mastercard", "rupay", "amex", "diners"}
)

// CardProduct is one real credit card a bank sells (HDFC Millennia, Axis
// Flipkart), so Add card can offer the exact card instead of only the bank.
//
// The art is a sketch: Drawing names a pen drawing the app ships (the idea of
// the real card, drawn fresh, never traced and never a logo), coloured at
// runtime from the app's mood in one of four tones. Drawing names are the
// app's: an unknown one draws the plain card.
//
// Fees are optional and only filled from the issuer's own published terms,
// with the pages they came from in SourceURLs and the day they were checked in
// LastVerifiedAt. Reward rules are phase 2 and live elsewhere.
type CardProduct struct {
	ID       string `gorm:"primaryKey;type:varchar(64)" json:"id"`
	IssuerID string `gorm:"type:varchar(64);not null;index" json:"issuer_id"`
	Name     string `gorm:"type:varchar(80);not null" json:"name"`

	Tone        string `gorm:"type:varchar(8);not null" json:"tone"`
	Drawing     string `gorm:"type:varchar(32);not null" json:"drawing"`
	Orientation string `gorm:"type:varchar(10);not null;default:landscape" json:"orientation"`
	// What the real card shows and how our drawing answers it, for the art
	// review ("a user says that's my card, a designer says that's a different
	// drawing"). ArtReviewed records that the review was done.
	ArtIdea     string `gorm:"type:varchar(120);not null;default:''" json:"art_idea"`
	ArtDrawnAs  string `gorm:"type:varchar(120);not null;default:''" json:"art_drawn_as"`
	ArtReviewed bool   `gorm:"not null;default:false" json:"art_reviewed"`

	JoiningFee     *Money     `gorm:"type:numeric(19,2)" json:"joining_fee"`
	AnnualFee      *Money     `gorm:"type:numeric(19,2)" json:"annual_fee"`
	FeeWaiverSpend *Money     `gorm:"type:numeric(19,2)" json:"fee_waiver_spend"`
	SourceURLs     StringList `gorm:"column:source_urls;type:text;not null;default:'[]'" json:"source_urls"`
	LastVerifiedAt *time.Time `json:"last_verified_at"`

	// SortOrder puts a bank's most-held cards first in the picker.
	SortOrder int `gorm:"not null;default:0" json:"sort_order"`
	// Hidden keeps a product out of the picker without breaking accounts that
	// already point at it. (A bool defaulting to false, so GORM never swaps an
	// explicit false for a column default.)
	Hidden    bool      `gorm:"not null;default:false" json:"hidden"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CardProductSummary is what an account carries about its product: enough to
// draw the card and name it, without the provenance.
type CardProductSummary struct {
	ID          string `json:"id"`
	IssuerID    string `json:"issuer_id"`
	Name        string `json:"name"`
	Tone        string `json:"tone"`
	Drawing     string `json:"drawing"`
	Orientation string `json:"orientation"`
}

func (p CardProduct) Summary() CardProductSummary {
	return CardProductSummary{ID: p.ID, IssuerID: p.IssuerID, Name: p.Name, Tone: p.Tone, Drawing: p.Drawing, Orientation: p.Orientation}
}

// StringList stores a short list of strings as a JSON array in a text column,
// which reads the same in Postgres and in the SQLite test database.
type StringList []string

func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	data, err := json.Marshal([]string(l))
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (l *StringList) Scan(value any) error {
	var raw []byte
	switch typed := value.(type) {
	case nil:
		*l = StringList{}
		return nil
	case []byte:
		raw = typed
	case string:
		raw = []byte(typed)
	default:
		return fmt.Errorf("unsupported string list value %T", value)
	}
	if len(raw) == 0 {
		*l = StringList{}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	*l = list
	return nil
}

func (l StringList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(l))
}
