package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func (s *Server) requireEntitlement(feature billing.FeatureCode) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.ensureEntitlement(c, feature) {
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) ensureEntitlement(c *gin.Context, feature billing.FeatureCode) bool {
	subject, ok := parseCreditSubject(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return false
	}
	result, err := billing.NewEntitlementService(database.DB).Check(subject, feature)
	if errors.Is(err, billing.ErrInvalidCreditSubject) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "entitlement_check_failed"})
		return false
	}
	if result.Allowed {
		return true
	}
	status := http.StatusPaymentRequired
	if result.Reason == billing.EntitlementFeatureLocked {
		status = http.StatusForbidden
	}
	c.JSON(status, gin.H{
		"error":                result.Reason,
		"feature_code":         result.FeatureCode,
		"feature_label":        result.FeatureLabel,
		"required_plan":        result.RequiredPlan,
		"required_entitlement": result.FeatureCode,
		"upgrade_required":     result.UpgradeRequired,
	})
	return false
}

// FreeBudgetLimit is how many budgets a user without a pass may keep (P22).
// One is enough to see what a budget does; more is what a pass buys.
const FreeBudgetLimit = 1

// requireBudgetAllowance gates the budget routes. A pass opens them fully.
// Without one, reading, editing and deleting stay open — the handlers only
// ever touch the caller's own budgets — and creating is allowed while the
// user holds fewer than FreeBudgetLimit. Past that the answer is the usual
// 402 upgrade payload, with the limit attached so the app can say why.
func (s *Server) requireBudgetAllowance() gin.HandlerFunc {
	return func(c *gin.Context) {
		subject, ok := parseCreditSubject(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
			c.Abort()
			return
		}
		result, err := billing.NewEntitlementService(database.DB).Check(subject, billing.FeatureBudgets)
		if errors.Is(err, billing.ErrInvalidCreditSubject) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
			c.Abort()
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "entitlement_check_failed"})
			c.Abort()
			return
		}
		if result.Allowed || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		var count int64
		userID := c.MustGet("userID").(uint)
		if err := database.DB.Model(&models.Budget{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "entitlement_check_failed"})
			c.Abort()
			return
		}
		if count < FreeBudgetLimit {
			c.Next()
			return
		}
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":                result.Reason,
			"feature_code":         result.FeatureCode,
			"feature_label":        result.FeatureLabel,
			"required_plan":        result.RequiredPlan,
			"required_entitlement": result.FeatureCode,
			"upgrade_required":     result.UpgradeRequired,
			"free_limit":           FreeBudgetLimit,
		})
		c.Abort()
	}
}
