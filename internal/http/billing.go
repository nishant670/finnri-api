package http

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
	"finnri/internal/payments"
)

const lifetimeQuoteRequiredPaidMonths = 3

type billingPlanResponse struct {
	Code                    string   `json:"code"`
	Name                    string   `json:"name"`
	BillingInterval         string   `json:"billing_interval"`
	PriceMinor              *int64   `json:"price_minor"`
	ListPriceMinor          *int64   `json:"list_price_minor"`
	Currency                string   `json:"currency"`
	IncludedCredits         int      `json:"included_credits"`
	DailyCreditLimit        int      `json:"daily_credit_limit"`
	RequiresLogin           bool     `json:"requires_login"`
	RequiresPriorPaidMonths int      `json:"requires_prior_paid_months"`
	CheckoutEnabled         bool     `json:"checkout_enabled"`
	FeatureGates            []string `json:"feature_gates"`
}

type creditSummaryResponse struct {
	TotalCreditsRemaining int                   `json:"total_credits_remaining"`
	DailyLimit            int                   `json:"daily_limit"`
	DailyCreditsUsed      int                   `json:"daily_credits_used"`
	DailyCreditsRemaining int                   `json:"daily_credits_remaining"`
	ResetAt               time.Time             `json:"reset_at"`
	TrialExpiresAt        *time.Time            `json:"trial_expires_at,omitempty"`
	Grants                []creditGrantResponse `json:"grants,omitempty"`
}

type creditGrantResponse struct {
	ID               uint       `json:"id"`
	Source           string     `json:"source"`
	CreditsGranted   int        `json:"credits_granted"`
	CreditsRemaining int        `json:"credits_remaining"`
	ValidFrom        time.Time  `json:"valid_from"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

type lifetimeEligibilityResponse struct {
	Eligible            bool `json:"eligible"`
	PaidMonthsCompleted int  `json:"paid_months_completed"`
	RequiredPaidMonths  int  `json:"required_paid_months"`
}

type billingStatusResponse struct {
	Plan                *billingPlanResponse        `json:"plan,omitempty"`
	SubscriptionStatus  string                      `json:"subscription_status"`
	CurrentPeriodStart  *time.Time                  `json:"current_period_start,omitempty"`
	CurrentPeriodEnd    *time.Time                  `json:"current_period_end,omitempty"`
	Credits             creditSummaryResponse       `json:"credits"`
	LifetimeEligibility lifetimeEligibilityResponse `json:"lifetime_eligibility"`
}

type aiUsageEventResponse struct {
	ID               uint       `json:"id"`
	RequestID        string     `json:"request_id"`
	ActionCode       string     `json:"action_code"`
	InputKind        string     `json:"input_kind"`
	Status           string     `json:"status"`
	EstimatedCredits int        `json:"estimated_credits"`
	ReservedCredits  int        `json:"reserved_credits"`
	FinalCredits     int        `json:"final_credits"`
	Model            string     `json:"model,omitempty"`
	SecondaryModel   string     `json:"secondary_model,omitempty"`
	PromptTokens     *int       `json:"prompt_tokens,omitempty"`
	CompletionTokens *int       `json:"completion_tokens,omitempty"`
	TotalTokens      *int       `json:"total_tokens,omitempty"`
	AudioDurationMs  *int       `json:"audio_duration_ms,omitempty"`
	AudioBytes       *int64     `json:"audio_bytes,omitempty"`
	InputChars       *int       `json:"input_chars,omitempty"`
	ResponseBytes    *int       `json:"response_bytes,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

type aiUsageListResponse struct {
	Events   []aiUsageEventResponse `json:"events"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
	Total    int64                  `json:"total"`
}

func (s *Server) listBillingPlans(c *gin.Context) {
	plans, err := s.publicBillingPlans()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plans"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plans": plans})
}

func (s *Server) getBillingStatus(c *gin.Context) {
	subject, ok := parseCreditSubject(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return
	}
	summary, err := buildCreditSummary(subject, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_credit_summary"})
		return
	}

	response := billingStatusResponse{
		SubscriptionStatus: "free",
		Credits:            summary,
		LifetimeEligibility: lifetimeEligibilityResponse{
			RequiredPaidMonths: lifetimeQuoteRequiredPaidMonths,
		},
	}
	user := currentUser(c)
	if user != nil && !user.IsGuest {
		subscription, found, err := currentUserSubscription(user.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_subscription"})
			return
		}
		if found {
			response.SubscriptionStatus = subscription.Status
			response.CurrentPeriodStart = &subscription.CurrentPeriodStart
			response.CurrentPeriodEnd = &subscription.CurrentPeriodEnd
			plan := s.planResponseFromModel(subscription.Plan)
			response.Plan = &plan
		}
		paidMonths, err := paidMonthsCompleted(user.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_lifetime_eligibility"})
			return
		}
		response.LifetimeEligibility.PaidMonthsCompleted = paidMonths
		response.LifetimeEligibility.Eligible = paidMonths >= lifetimeQuoteRequiredPaidMonths
	}
	c.JSON(http.StatusOK, response)
}

func (s *Server) requestLifetimeQuote(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if user.IsGuest {
		c.JSON(http.StatusForbidden, gin.H{"error": "login_required_for_lifetime_quote"})
		return
	}
	paidMonths, err := paidMonthsCompleted(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_lifetime_eligibility"})
		return
	}
	if paidMonths < lifetimeQuoteRequiredPaidMonths {
		c.JSON(http.StatusForbidden, gin.H{
			"error":                 "lifetime_quote_not_eligible",
			"paid_months_completed": paidMonths,
			"required_paid_months":  lifetimeQuoteRequiredPaidMonths,
		})
		return
	}

	quote, err := createLifetimeQuoteRequest(user.ID, paidMonths)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_lifetime_quote_request"})
		return
	}
	c.JSON(http.StatusCreated, quote)
}

func (s *Server) listAIUsage(c *gin.Context) {
	subject, ok := parseCreditSubject(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return
	}
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))

	var total int64
	query := scopeSubject(database.DB.Model(&models.AIUsageEvent{}), subject)
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_ai_usage"})
		return
	}

	var events []models.AIUsageEvent
	query = scopeSubject(database.DB.Model(&models.AIUsageEvent{}), subject)
	if err := query.Order("started_at DESC").Order("id DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&events).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_ai_usage"})
		return
	}

	responseEvents := make([]aiUsageEventResponse, 0, len(events))
	for _, event := range events {
		responseEvents = append(responseEvents, aiUsageEventResponse{
			ID:               event.ID,
			RequestID:        event.RequestID,
			ActionCode:       event.ActionCode,
			InputKind:        event.InputKind,
			Status:           event.Status,
			EstimatedCredits: event.EstimatedCredits,
			ReservedCredits:  event.ReservedCredits,
			FinalCredits:     event.FinalCredits,
			Model:            event.Model,
			SecondaryModel:   event.SecondaryModel,
			PromptTokens:     event.PromptTokens,
			CompletionTokens: event.CompletionTokens,
			TotalTokens:      event.TotalTokens,
			AudioDurationMs:  event.AudioDurationMs,
			AudioBytes:       event.AudioBytes,
			InputChars:       event.InputChars,
			ResponseBytes:    event.ResponseBytes,
			ErrorCode:        event.ErrorCode,
			StartedAt:        event.StartedAt,
			FinishedAt:       event.FinishedAt,
		})
	}
	c.JSON(http.StatusOK, aiUsageListResponse{Events: responseEvents, Page: page, PageSize: pageSize, Total: total})
}

func (s *Server) getAICredits(c *gin.Context) {
	subject, ok := parseCreditSubject(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return
	}
	summary, err := buildCreditSummary(subject, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_credit_summary"})
		return
	}
	c.JSON(http.StatusOK, summary)
}

func (s *Server) publicBillingPlans() ([]billingPlanResponse, error) {
	var plans []models.Plan
	err := database.DB.Where("is_public = ?", true).
		Order("CASE billing_interval WHEN 'weekly' THEN 1 WHEN 'monthly' THEN 2 WHEN 'quarterly' THEN 3 WHEN 'yearly' THEN 4 WHEN 'lifetime_quote' THEN 5 ELSE 6 END").
		Order("price_minor ASC").
		Find(&plans).Error
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return s.defaultBillingPlans(), nil
	}
	response := make([]billingPlanResponse, 0, len(plans))
	for _, plan := range plans {
		response = append(response, s.planResponseFromModel(plan))
	}
	return response, nil
}

func (s *Server) defaultBillingPlans() []billingPlanResponse {
	return []billingPlanResponse{
		{
			Code:             "weekly_pass",
			Name:             "Weekly Pass",
			BillingInterval:  "weekly",
			PriceMinor:       int64Pointer(7900),
			ListPriceMinor:   int64Pointer(19900),
			Currency:         "INR",
			IncludedCredits:  800,
			DailyCreditLimit: 200,
			RequiresLogin:    true,
			CheckoutEnabled:  s.checkoutEnabledFor("weekly"),
			FeatureGates:     paidFeatureGates(),
		},
		{
			Code:             "monthly",
			Name:             "Monthly",
			BillingInterval:  "monthly",
			PriceMinor:       int64Pointer(14900),
			ListPriceMinor:   int64Pointer(49900),
			Currency:         "INR",
			IncludedCredits:  3600,
			DailyCreditLimit: 250,
			RequiresLogin:    true,
			CheckoutEnabled:  s.checkoutEnabledFor("monthly"),
			FeatureGates:     paidFeatureGates(),
		},
		{
			Code:             "quarterly",
			Name:             "Quarterly",
			BillingInterval:  "quarterly",
			PriceMinor:       int64Pointer(32900),
			ListPriceMinor:   int64Pointer(129900),
			Currency:         "INR",
			IncludedCredits:  11000,
			DailyCreditLimit: 300,
			RequiresLogin:    true,
			CheckoutEnabled:  s.checkoutEnabledFor("quarterly"),
			FeatureGates:     paidFeatureGates(),
		},
		{
			Code:             "yearly",
			Name:             "Yearly",
			BillingInterval:  "yearly",
			PriceMinor:       int64Pointer(79900),
			ListPriceMinor:   int64Pointer(399900),
			Currency:         "INR",
			IncludedCredits:  48000,
			DailyCreditLimit: 350,
			RequiresLogin:    true,
			CheckoutEnabled:  s.checkoutEnabledFor("yearly"),
			FeatureGates:     paidFeatureGates(),
		},
		{
			Code:                    "lifetime_quote",
			Name:                    "Lifetime Quote",
			BillingInterval:         "lifetime_quote",
			PriceMinor:              int64Pointer(499900),
			ListPriceMinor:          int64Pointer(1999900),
			Currency:                "INR",
			IncludedCredits:         5000,
			DailyCreditLimit:        500,
			RequiresLogin:           true,
			RequiresPriorPaidMonths: lifetimeQuoteRequiredPaidMonths,
			CheckoutEnabled:         s.checkoutEnabledFor("lifetime_quote"),
			FeatureGates:            paidFeatureGates(),
		},
	}
}

func (s *Server) planResponseFromModel(plan models.Plan) billingPlanResponse {
	price := plan.PriceMinor
	listPrice := plan.ListPriceMinor
	return billingPlanResponse{
		Code:                    plan.Code,
		Name:                    plan.Name,
		BillingInterval:         plan.BillingInterval,
		PriceMinor:              &price,
		ListPriceMinor:          &listPrice,
		Currency:                plan.Currency,
		IncludedCredits:         plan.IncludedCredits,
		DailyCreditLimit:        plan.DailyCreditLimit,
		RequiresLogin:           plan.RequiresLogin,
		RequiresPriorPaidMonths: plan.RequiresPriorPaidMonths,
		CheckoutEnabled:         s.checkoutEnabledFor(plan.BillingInterval),
		FeatureGates:            planFeatureGates(plan.Code),
	}
}

func int64Pointer(value int64) *int64 { return &value }

// razorpayConfig reads the provider secrets. A Server built directly, which
// every unit test does, has no cfg and therefore no configured provider — so
// checkout stays advertised as off rather than panicking.
func (s *Server) razorpayConfig() payments.RazorpayConfig {
	if s.cfg == nil {
		return payments.RazorpayConfig{}
	}
	return payments.RazorpayConfig{
		KeyID:         s.cfg.RazorpayKeyID,
		KeySecret:     s.cfg.RazorpayKeySecret,
		WebhookSecret: s.cfg.RazorpayWebhookSecret,
		BaseURL:       s.cfg.RazorpayBaseURL,
	}
}

// paymentProviderConfigured was a literal `return false` for the whole life of
// the billing code, which is why checkout_enabled was false on every plan and
// no rupee could be collected. It now reads real configuration.
func (s *Server) paymentProviderConfigured() bool {
	return s.razorpayClient().Configured()
}

// razorpayClient returns the provider client, building one from cfg when the
// Server was constructed without going through NewServer.
func (s *Server) razorpayClient() *payments.Client {
	if s.payments != nil {
		return s.payments
	}
	return payments.NewClient(s.razorpayConfig())
}

// checkoutEnabledFor answers whether the buy button may be drawn for a plan.
//
// Lifetime is excluded by design and not only because the provider is off: the
// launch plan requires it be non-purchasable in store builds, and it is sold
// by quote after three paid months rather than off a price tag.
func (s *Server) checkoutEnabledFor(billingInterval string) bool {
	return s.paymentProviderConfigured() && billingInterval != "lifetime_quote"
}

func (s *Server) findPublicBillingPlan(code string) (billingPlanResponse, bool, error) {
	var plan models.Plan
	err := database.DB.Where("code = ? AND is_public = ?", code, true).First(&plan).Error
	if err == nil {
		return s.planResponseFromModel(plan), true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return billingPlanResponse{}, false, err
	}
	for _, fallback := range s.defaultBillingPlans() {
		if fallback.Code == code {
			return fallback, true, nil
		}
	}
	return billingPlanResponse{}, false, nil
}

func planFeatureGates(code string) []string {
	if code == "lifetime_quote" {
		return paidFeatureGates()
	}
	return paidFeatureGates()
}

func paidFeatureGates() []string {
	return billing.PaidFeatureCodes()
}

func buildCreditSummary(subject billing.CreditSubject, includeGrants bool) (creditSummaryResponse, error) {
	now := time.Now().UTC()
	var total int
	query := database.DB.Model(&models.CreditGrant{}).
		Where("valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) AND credits_remaining > 0", now, now)
	query = scopeSubject(query, subject)
	if err := query.Select("COALESCE(SUM(credits_remaining), 0)").Scan(&total).Error; err != nil {
		return creditSummaryResponse{}, err
	}

	var dailyUsage models.DailyCreditUsage
	usedToday := 0
	dailyQuery := scopeSubject(database.DB.Where("usage_date = ?", now.Format("2006-01-02")), subject)
	if result := dailyQuery.Limit(1).Find(&dailyUsage); result.Error != nil {
		return creditSummaryResponse{}, result.Error
	} else if result.RowsAffected > 0 {
		usedToday = dailyUsage.CreditsUsed
	}

	dailyLimit := billing.LoggedInFreeDailyLimit
	if subject.UserID == nil {
		dailyLimit = billing.GuestDailyLimit
	} else if subscription, found, err := currentUserSubscription(*subject.UserID); err != nil {
		return creditSummaryResponse{}, err
	} else if found && subscription.Plan.DailyCreditLimit > 0 {
		dailyLimit = subscription.Plan.DailyCreditLimit
	}
	dailyRemaining := dailyLimit - usedToday
	if dailyRemaining < 0 {
		dailyRemaining = 0
	}
	if dailyRemaining > total {
		dailyRemaining = total
	}

	var trialExpiresAt *time.Time
	var trialGrant models.CreditGrant
	trialQuery := scopeSubject(database.DB.Where("source = ?", billing.GrantSourceFreeTrial), subject)
	if result := trialQuery.Order("expires_at DESC").Limit(1).Find(&trialGrant); result.Error != nil {
		return creditSummaryResponse{}, result.Error
	} else if result.RowsAffected > 0 {
		trialExpiresAt = trialGrant.ExpiresAt
	}

	summary := creditSummaryResponse{
		TotalCreditsRemaining: total,
		DailyLimit:            dailyLimit,
		DailyCreditsUsed:      usedToday,
		DailyCreditsRemaining: dailyRemaining,
		ResetAt:               nextCreditResetAt(now),
		TrialExpiresAt:        trialExpiresAt,
	}
	if includeGrants {
		var grants []models.CreditGrant
		grantsQuery := database.DB.Where("valid_from <= ? AND (expires_at IS NULL OR expires_at > ?)", now, now)
		grantsQuery = scopeSubject(grantsQuery, subject)
		if err := grantsQuery.Order("expires_at ASC").Order("valid_from ASC").Find(&grants).Error; err != nil {
			return creditSummaryResponse{}, err
		}
		summary.Grants = make([]creditGrantResponse, 0, len(grants))
		for _, grant := range grants {
			summary.Grants = append(summary.Grants, creditGrantResponse{
				ID:               grant.ID,
				Source:           grant.Source,
				CreditsGranted:   grant.CreditsGranted,
				CreditsRemaining: grant.CreditsRemaining,
				ValidFrom:        grant.ValidFrom,
				ExpiresAt:        grant.ExpiresAt,
			})
		}
	}
	return summary, nil
}

func currentUserSubscription(userID uint) (models.UserSubscription, bool, error) {
	now := time.Now().UTC()
	var subscription models.UserSubscription
	result := database.DB.Preload("Plan").
		Where("user_id = ? AND status IN ? AND current_period_start <= ? AND current_period_end > ?", userID, []string{"trialing", "active"}, now, now).
		Order("current_period_end DESC").
		Limit(1).
		Find(&subscription)
	if result.Error != nil {
		return models.UserSubscription{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		return models.UserSubscription{}, false, nil
	}
	return subscription, true, nil
}

func paidMonthsCompleted(userID uint) (int, error) {
	now := time.Now().UTC()
	var subscriptions []models.UserSubscription
	if err := database.DB.Preload("Plan").
		Where("user_id = ? AND status IN ?", userID, []string{"active", "cancelled", "expired"}).
		Find(&subscriptions).Error; err != nil {
		return 0, err
	}

	days := 0.0
	for _, subscription := range subscriptions {
		if subscription.Plan.BillingInterval == "lifetime_quote" || subscription.CurrentPeriodEnd.Before(subscription.CurrentPeriodStart) {
			continue
		}
		end := subscription.CurrentPeriodEnd
		if end.After(now) {
			end = now
		}
		if !end.After(subscription.CurrentPeriodStart) {
			continue
		}
		days += end.Sub(subscription.CurrentPeriodStart).Hours() / 24
	}
	return int(math.Floor(days / 30)), nil
}

func createLifetimeQuoteRequest(userID uint, paidMonths int) (models.LifetimeQuoteRequest, error) {
	now := time.Now().UTC()
	windowStart := now.AddDate(0, 0, -90)
	var summary struct {
		UsageEventCount        int
		CreditsUsed            int
		EstimatedCostUSDMicros int64
	}
	if err := database.DB.Model(&models.AIUsageEvent{}).
		Where("user_id = ? AND started_at >= ? AND started_at < ?", userID, windowStart, now).
		Select("COUNT(*) AS usage_event_count, COALESCE(SUM(final_credits), 0) AS credits_used, COALESCE(SUM(estimated_cost_usd_micros), 0) AS estimated_cost_usd_micros").
		Scan(&summary).Error; err != nil {
		return models.LifetimeQuoteRequest{}, err
	}
	request := models.LifetimeQuoteRequest{
		UserID:                      userID,
		Status:                      "requested",
		PaidMonthsCompleted:         paidMonths,
		UsageWindowStart:            windowStart,
		UsageWindowEnd:              now,
		UsageEventCount:             summary.UsageEventCount,
		CreditsUsed:                 summary.CreditsUsed,
		AverageMonthlyCredits:       int(math.Ceil(float64(summary.CreditsUsed) / 3)),
		EstimatedCostUSDMicros:      summary.EstimatedCostUSDMicros,
		AverageMonthlyCostUSDMicros: int64(math.Ceil(float64(summary.EstimatedCostUSDMicros) / 3)),
	}
	if err := database.DB.Create(&request).Error; err != nil {
		return models.LifetimeQuoteRequest{}, err
	}
	return request, nil
}

func scopeSubject(query *gorm.DB, subject billing.CreditSubject) *gorm.DB {
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

func currentUser(c *gin.Context) *models.User {
	value, exists := c.Get("user")
	if !exists {
		return nil
	}
	user, ok := value.(*models.User)
	if !ok {
		return nil
	}
	return user
}

func parseBillingPagination(pageRaw, pageSizeRaw string) (int, int) {
	page := 1
	pageSize := 20
	if parsed, err := strconv.Atoi(strings.TrimSpace(pageRaw)); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(strings.TrimSpace(pageSizeRaw)); err == nil && parsed > 0 {
		pageSize = parsed
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
