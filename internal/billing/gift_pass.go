package billing

import (
	"fmt"

	"finnri/internal/models"

	"gorm.io/gorm"
)

const ReasonGiftPassRevoked = "gift_pass_revoked"

// RevokeGiftedPassCreditsTx takes back whatever is left of a gifted pass's
// credits when an admin revokes it. What was already spent stays spent, as on
// a refund: the ledger is append-only and a negative balance would break every
// allowance check. Every grant tied to the pass goes, including a not-yet-valid
// one on a queued period. Safe to call twice.
func RevokeGiftedPassCreditsTx(tx *gorm.DB, subscriptionID uint) (int, error) {
	var grants []models.CreditGrant
	if err := tx.Where("subscription_id = ? AND credits_remaining > 0", subscriptionID).Find(&grants).Error; err != nil {
		return 0, err
	}
	revoked := 0
	for _, grant := range grants {
		remaining := grant.CreditsRemaining
		result := tx.Model(&models.CreditGrant{}).Where("id = ? AND credits_remaining = ?", grant.ID, remaining).
			Update("credits_remaining", 0)
		if result.Error != nil {
			return revoked, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		grant.CreditsRemaining = 0
		if err := createGrantLedger(tx, grant, LedgerDirectionExpiry, remaining, ReasonGiftPassRevoked, fmt.Sprintf("gift_revoke:grant:%d", grant.ID)); err != nil {
			return revoked, err
		}
		revoked += remaining
	}
	return revoked, nil
}
