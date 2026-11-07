package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/database"
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
