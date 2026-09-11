package http

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// cardEMIPlanInput is what the bank quotes the user, plus what they were
// buying. Everything else — the schedule, the interest total, the per-month
// principal split — is derived by emi.go.
type cardEMIPlanInput struct {
	Title            string       `json:"title"`
	Merchant         string       `json:"merchant"`
	Category         string       `json:"category"`
	Principal        models.Money `json:"principal"`
	AnnualRatePct    float64      `json:"annual_rate_pct"`
	TenureMonths     int          `json:"tenure_months"`
	PurchasedOn      string       `json:"purchased_on"`
	FirstInstallment string       `json:"first_installment"`
	Notes            string       `json:"notes"`

	// SourceEntryID converts a purchase the user already logged. That entry is
	// removed, because a card bills instalments rather than the purchase —
	// see convertSourceEntry.
	SourceEntryID *uint `json:"source_entry_id"`
}

func (input cardEMIPlanInput) validate() map[string]string {
	fields := map[string]string{}

	if strings.TrimSpace(input.Title) == "" {
		fields["title"] = "is required"
	}
	if !input.Principal.IsPositive() {
		fields["principal"] = "must be positive"
	}
	if input.TenureMonths < 1 || input.TenureMonths > maxEMITenureMonths {
		fields["tenure_months"] = "must be between 1 and 360"
	}
	if input.AnnualRatePct < 0 || input.AnnualRatePct > maxEMIAnnualRatePercent {
		fields["annual_rate_pct"] = "must be between 0 and 100"
	}
	if _, err := parseStrictAPIDate(input.PurchasedOn); err != nil {
		fields["purchased_on"] = "must be a YYYY-MM-DD date"
	}
	if strings.TrimSpace(input.FirstInstallment) != "" {
		if _, err := parseStrictAPIDate(input.FirstInstallment); err != nil {
			fields["first_installment"] = "must be a YYYY-MM-DD date"
		}
	}

	return fields
}

// createCardEMIPlan converts a purchase to instalments.
func (s *Server) createCardEMIPlan(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, err := loadUserCard(userID, accountID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}

	var input cardEMIPlanInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_emi_plan", "fields": fields})
		return
	}
	if input.SourceEntryID != nil {
		var source models.Entry
		if err := database.DB.
			Where("id = ? AND user_id = ? AND account_id = ?", *input.SourceEntryID, userID, accountID).
			First(&source).Error; err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_emi_plan", "fields": gin.H{
				"source_entry_id": "must be an entry on this credit card",
			}})
			return
		}
		if source.Amount != input.Principal {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_emi_plan", "fields": gin.H{
				"principal": "must match the source entry amount",
			}})
			return
		}
	}

	purchasedOn, _ := parseStrictAPIDate(input.PurchasedOn)
	firstInstallment := strings.TrimSpace(input.FirstInstallment)
	if firstInstallment == "" {
		// Issuers bill the first instalment on the statement after the
		// purchase, so a month later is the honest default.
		firstInstallment = clampDayToMonth(
			purchasedOn.Year(), purchasedOn.Month()+1, purchasedOn.Day(),
		).Format(apiDateLayout)
	}

	// The schedule is emi.go's, not a second implementation of the same maths.
	calculation := buildEMICalculation(emiCalculationInput{
		PrincipalAmount:           input.Principal,
		AnnualInterestRatePercent: input.AnnualRatePct,
		TenureMonths:              input.TenureMonths,
		Currency:                  "INR",
	})

	plan := models.CardEMIPlan{
		UserID:           userID,
		AccountID:        accountID,
		Title:            strings.TrimSpace(input.Title),
		Merchant:         strings.TrimSpace(input.Merchant),
		Category:         emiEntryCategory(input.Category),
		Principal:        input.Principal,
		AnnualRatePct:    input.AnnualRatePct,
		TenureMonths:     input.TenureMonths,
		MonthlyAmount:    calculation.MonthlyEMI,
		TotalInterest:    calculation.TotalInterest,
		Currency:         "INR",
		PurchasedOn:      input.PurchasedOn,
		FirstInstallment: firstInstallment,
		Status:           emiPlanActive,
		SourceEntryID:    input.SourceEntryID,
		Notes:            input.Notes,
	}

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&plan).Error; err != nil {
			return err
		}
		installments := buildInstallments(plan, calculation.Schedule)
		if len(installments) == 0 {
			return gorm.ErrInvalidData
		}
		if err := tx.Create(&installments).Error; err != nil {
			return err
		}
		return convertSourceEntry(tx, userID, input.SourceEntryID)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_emi_plan"})
		return
	}

	// Bill anything already due — a plan added late should not sit there
	// pretending its first months have not happened.
	if _, err := syncCardEMIInstallments(userID, time.Now()); err != nil {
		log.Printf("card emi: could not bill due instalments for user %d: %v", userID, err)
	}

	respondWithEMIPlan(c, userID, plan.ID, http.StatusCreated)
}

// convertSourceEntry removes the original purchase.
//
// This looks destructive and is deliberate. A ₹60,000 purchase converted to
// EMI is never billed as ₹60,000 — the statement bills ₹5,000 a month, and the
// plan now generates exactly those entries. Leaving the original would
// double-count the spend, wreck the cycle's reconciliation against the bill,
// and overstate the month it was bought in by the full purchase price.
//
// Nothing is lost that the plan does not carry: its title, merchant and
// category came from this entry.
func convertSourceEntry(tx *gorm.DB, userID uint, entryID *uint) error {
	if entryID == nil {
		return nil
	}
	result := tx.Where("id = ? AND user_id = ?", *entryID, userID).
		Delete(&models.Entry{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// listCardEMIPlans returns a card's plans, newest first, each with progress.
func (s *Server) listCardEMIPlans(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, err := loadUserCard(userID, accountID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}

	var plans []models.CardEMIPlan
	if err := database.DB.
		Preload("Installments", func(db *gorm.DB) *gorm.DB { return db.Order("seq ASC") }).
		Where("user_id = ? AND account_id = ?", userID, accountID).
		Order("status ASC, purchased_on DESC").
		Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_emi_plans"})
		return
	}

	responses := make([]cardEMIPlanResponse, 0, len(plans))
	for _, plan := range plans {
		responses = append(responses, cardEMIPlanResponse{
			CardEMIPlan: plan,
			Progress:    summariseEMIPlan(plan.Installments),
		})
	}
	c.JSON(http.StatusOK, responses)
}

func (s *Server) getCardEMIPlan(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	planID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	respondWithEMIPlan(c, userID, planID, http.StatusOK)
}

// forecloseCardEMIPlan pays a plan off early, releasing all remaining
// principal at once.
func (s *Server) forecloseCardEMIPlan(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	planID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var plan models.CardEMIPlan
	if err := database.DB.
		Where("id = ? AND user_id = ?", planID, userID).
		First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "emi plan not found"})
		return
	}
	if plan.Status != emiPlanActive {
		c.JSON(http.StatusConflict, gin.H{"error": "emi_plan_not_active"})
		return
	}

	if err := forecloseEMIPlan(&plan); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_foreclose_emi_plan"})
		return
	}
	respondWithEMIPlan(c, userID, plan.ID, http.StatusOK)
}

// deleteCardEMIPlan removes a plan added by mistake, along with the
// instalment entries it has already written. Entries the user made themselves
// are untouched — only ones this plan generated.
func (s *Server) deleteCardEMIPlan(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	planID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	plan, err := loadEMIPlanWithInstallments(userID, planID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "emi plan not found"})
		return
	}

	entryIDs := make([]uint, 0, len(plan.Installments))
	for _, installment := range plan.Installments {
		if installment.EntryID != nil {
			entryIDs = append(entryIDs, *installment.EntryID)
		}
	}

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		if len(entryIDs) > 0 {
			if err := tx.Where("id IN ? AND user_id = ?", entryIDs, userID).
				Delete(&models.Entry{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("plan_id = ?", plan.ID).
			Delete(&models.CardEMIInstallment{}).Error; err != nil {
			return err
		}
		return tx.Delete(&plan).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_emi_plan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

func respondWithEMIPlan(c *gin.Context, userID, planID uint, status int) {
	plan, err := loadEMIPlanWithInstallments(userID, planID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "emi plan not found"})
		return
	}
	c.JSON(status, cardEMIPlanResponse{
		CardEMIPlan: plan,
		Progress:    summariseEMIPlan(plan.Installments),
	})
}
