package billing

import (
	"time"

	"gorm.io/gorm"

	"finnri/internal/models"
)

type AlertConfig struct {
	DailyCostUSDMicros         int64
	AbuseDailyCreditsThreshold int
	FreeCostPerUserUSDMicros   int64
}

type AlertResult struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type UsageMetricRow struct {
	Key                    string `json:"key"`
	Events                 int64  `json:"events"`
	Credits                int64  `json:"credits"`
	EstimatedCostUSDMicros int64  `json:"estimated_cost_usd_micros"`
}

type AIMetrics struct {
	WindowStart                   time.Time        `json:"window_start"`
	WindowEnd                     time.Time        `json:"window_end"`
	TotalEvents                   int64            `json:"total_events"`
	SuccessfulEvents              int64            `json:"successful_events"`
	FailedAfterProviderEvents     int64            `json:"failed_after_provider_events"`
	EstimatedCostUSDMicros        int64            `json:"estimated_cost_usd_micros"`
	ActualCostUSDMicros           int64            `json:"actual_cost_usd_micros"`
	TotalCreditsCharged           int64            `json:"total_credits_charged"`
	ParseSuccessRate              float64          `json:"parse_success_rate"`
	AverageCreditsCharged         float64          `json:"average_credits_charged"`
	ActiveUsers                   int64            `json:"active_users"`
	PaidUsers                     int64            `json:"paid_users"`
	FreeUsers                     int64            `json:"free_users"`
	CostPerActiveUserUSDMicros    int64            `json:"cost_per_active_user_usd_micros"`
	CostPerPaidUserUSDMicros      int64            `json:"cost_per_paid_user_usd_micros"`
	FreeCostPerUserUSDMicros      int64            `json:"free_cost_per_user_usd_micros"`
	UsersHitDailyCap              int64            `json:"users_hit_daily_cap"`
	UsersHitTotalCreditCap        int64            `json:"users_hit_total_credit_cap"`
	GuestDevicesHitDailyCap       int64            `json:"guest_devices_hit_daily_cap"`
	GuestDevicesHitTotalCreditCap int64            `json:"guest_devices_hit_total_credit_cap"`
	MaxSubjectDailyCredits        int64            `json:"max_subject_daily_credits"`
	ByModel                       []UsageMetricRow `json:"by_model"`
	ByAction                      []UsageMetricRow `json:"by_action"`
	ByPlan                        []UsageMetricRow `json:"by_plan"`
	Alerts                        []AlertResult    `json:"alerts"`
}

func BuildAIMetrics(db *gorm.DB, windowStart, windowEnd time.Time, alerts AlertConfig) (AIMetrics, error) {
	metrics := AIMetrics{WindowStart: windowStart.UTC(), WindowEnd: windowEnd.UTC()}
	base := db.Model(&models.AIUsageEvent{}).Where("started_at >= ? AND started_at < ?", metrics.WindowStart, metrics.WindowEnd)
	if err := base.Count(&metrics.TotalEvents).Error; err != nil {
		return metrics, err
	}
	if err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ? AND status = ?", metrics.WindowStart, metrics.WindowEnd, UsageStatusSucceeded).
		Count(&metrics.SuccessfulEvents).Error; err != nil {
		return metrics, err
	}
	if err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ? AND status = ?", metrics.WindowStart, metrics.WindowEnd, UsageStatusFailedAfterProvider).
		Count(&metrics.FailedAfterProviderEvents).Error; err != nil {
		return metrics, err
	}
	if err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ?", metrics.WindowStart, metrics.WindowEnd).
		Select("COALESCE(SUM(estimated_cost_usd_micros), 0)").
		Scan(&metrics.EstimatedCostUSDMicros).Error; err != nil {
		return metrics, err
	}
	if err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ? AND actual_cost_usd_micros IS NOT NULL", metrics.WindowStart, metrics.WindowEnd).
		Select("COALESCE(SUM(actual_cost_usd_micros), 0)").
		Scan(&metrics.ActualCostUSDMicros).Error; err != nil {
		return metrics, err
	}
	if err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ?", metrics.WindowStart, metrics.WindowEnd).
		Select("COALESCE(SUM(final_credits), 0)").
		Scan(&metrics.TotalCreditsCharged).Error; err != nil {
		return metrics, err
	}
	if metrics.TotalEvents > 0 {
		metrics.ParseSuccessRate = float64(metrics.SuccessfulEvents) / float64(metrics.TotalEvents)
		metrics.AverageCreditsCharged = float64(metrics.TotalCreditsCharged) / float64(metrics.TotalEvents)
	}

	if err := countDistinctUsers(db, &metrics); err != nil {
		return metrics, err
	}
	if err := countLimitHits(db, &metrics); err != nil {
		return metrics, err
	}
	if err := maxSubjectDailyCredits(db, &metrics); err != nil {
		return metrics, err
	}
	var err error
	metrics.ByModel, err = metricRows(db, metrics.WindowStart, metrics.WindowEnd, "COALESCE(NULLIF(model, ''), NULLIF(secondary_model, ''), 'unknown')")
	if err != nil {
		return metrics, err
	}
	metrics.ByAction, err = metricRows(db, metrics.WindowStart, metrics.WindowEnd, "action_code")
	if err != nil {
		return metrics, err
	}
	metrics.ByPlan, err = metricRowsByPlan(db, metrics.WindowStart, metrics.WindowEnd)
	if err != nil {
		return metrics, err
	}
	metrics.Alerts = evaluateAlerts(metrics, alerts)
	return metrics, nil
}

func countDistinctUsers(db *gorm.DB, metrics *AIMetrics) error {
	userQuery := "started_at >= ? AND started_at < ? AND user_id IS NOT NULL"
	if err := db.Model(&models.AIUsageEvent{}).Where(userQuery, metrics.WindowStart, metrics.WindowEnd).
		Select("COUNT(DISTINCT user_id)").Scan(&metrics.ActiveUsers).Error; err != nil {
		return err
	}
	if err := db.Model(&models.UserSubscription{}).
		Where("status = ? AND current_period_start < ? AND current_period_end > ?", "active", metrics.WindowEnd, metrics.WindowStart).
		Select("COUNT(DISTINCT user_id)").Scan(&metrics.PaidUsers).Error; err != nil {
		return err
	}
	metrics.FreeUsers = metrics.ActiveUsers - metrics.PaidUsers
	if metrics.FreeUsers < 0 {
		metrics.FreeUsers = 0
	}
	var userCost int64
	if err := db.Model(&models.AIUsageEvent{}).
		Where(userQuery, metrics.WindowStart, metrics.WindowEnd).
		Select("COALESCE(SUM(estimated_cost_usd_micros), 0)").Scan(&userCost).Error; err != nil {
		return err
	}
	if metrics.ActiveUsers > 0 {
		metrics.CostPerActiveUserUSDMicros = userCost / metrics.ActiveUsers
	}

	var paidUserIDs []uint
	if err := db.Model(&models.UserSubscription{}).
		Where("status = ? AND current_period_start < ? AND current_period_end > ?", "active", metrics.WindowEnd, metrics.WindowStart).
		Distinct("user_id").
		Pluck("user_id", &paidUserIDs).Error; err != nil {
		return err
	}
	var paidCost int64
	if len(paidUserIDs) > 0 {
		if err := db.Model(&models.AIUsageEvent{}).
			Where("started_at >= ? AND started_at < ? AND user_id IN ?", metrics.WindowStart, metrics.WindowEnd, paidUserIDs).
			Select("COALESCE(SUM(estimated_cost_usd_micros), 0)").Scan(&paidCost).Error; err != nil {
			return err
		}
	}
	if metrics.PaidUsers > 0 {
		metrics.CostPerPaidUserUSDMicros = paidCost / metrics.PaidUsers
	}

	freeCostQuery := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ? AND user_id IS NOT NULL", metrics.WindowStart, metrics.WindowEnd)
	if len(paidUserIDs) > 0 {
		freeCostQuery = freeCostQuery.Where("user_id NOT IN ?", paidUserIDs)
	}
	var freeCost int64
	if err := freeCostQuery.Select("COALESCE(SUM(estimated_cost_usd_micros), 0)").Scan(&freeCost).Error; err != nil {
		return err
	}
	if metrics.FreeUsers > 0 {
		metrics.FreeCostPerUserUSDMicros = freeCost / metrics.FreeUsers
	}
	return nil
}

func countLimitHits(db *gorm.DB, metrics *AIMetrics) error {
	if err := db.Model(&models.AIUsageLimitEvent{}).
		Where("created_at >= ? AND created_at < ? AND reason = ? AND user_id IS NOT NULL", metrics.WindowStart, metrics.WindowEnd, AllowanceDailyLimitReached).
		Select("COUNT(DISTINCT user_id)").Scan(&metrics.UsersHitDailyCap).Error; err != nil {
		return err
	}
	if err := db.Model(&models.AIUsageLimitEvent{}).
		Where("created_at >= ? AND created_at < ? AND reason = ? AND user_id IS NOT NULL", metrics.WindowStart, metrics.WindowEnd, AllowanceInsufficientCredits).
		Select("COUNT(DISTINCT user_id)").Scan(&metrics.UsersHitTotalCreditCap).Error; err != nil {
		return err
	}
	if err := db.Model(&models.AIUsageLimitEvent{}).
		Where("created_at >= ? AND created_at < ? AND reason = ? AND guest_device_id_hash <> ''", metrics.WindowStart, metrics.WindowEnd, AllowanceDailyLimitReached).
		Select("COUNT(DISTINCT guest_device_id_hash)").Scan(&metrics.GuestDevicesHitDailyCap).Error; err != nil {
		return err
	}
	return db.Model(&models.AIUsageLimitEvent{}).
		Where("created_at >= ? AND created_at < ? AND reason = ? AND guest_device_id_hash <> ''", metrics.WindowStart, metrics.WindowEnd, AllowanceInsufficientCredits).
		Select("COUNT(DISTINCT guest_device_id_hash)").Scan(&metrics.GuestDevicesHitTotalCreditCap).Error
}

func metricRows(db *gorm.DB, start, end time.Time, groupExpr string) ([]UsageMetricRow, error) {
	var rows []UsageMetricRow
	err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ?", start, end).
		Select(groupExpr + " AS key, COUNT(*) AS events, COALESCE(SUM(final_credits), 0) AS credits, COALESCE(SUM(estimated_cost_usd_micros), 0) AS estimated_cost_usd_micros").
		Group("key").
		Order("estimated_cost_usd_micros DESC").
		Scan(&rows).Error
	return rows, err
}

func maxSubjectDailyCredits(db *gorm.DB, metrics *AIMetrics) error {
	var rows []struct {
		Subject string
		Credits int64
	}
	err := db.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND started_at < ?", metrics.WindowStart, metrics.WindowEnd).
		Select("COALESCE(CAST(user_id AS TEXT), guest_device_id_hash) AS subject, COALESCE(SUM(final_credits), 0) AS credits").
		Group("subject").
		Order("credits DESC").
		Limit(1).
		Scan(&rows).Error
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		metrics.MaxSubjectDailyCredits = rows[0].Credits
	}
	return nil
}

func metricRowsByPlan(db *gorm.DB, start, end time.Time) ([]UsageMetricRow, error) {
	var rows []UsageMetricRow
	err := db.Table("ai_usage_events").
		Joins("LEFT JOIN user_subscriptions ON user_subscriptions.user_id = ai_usage_events.user_id AND user_subscriptions.current_period_start <= ai_usage_events.started_at AND user_subscriptions.current_period_end > ai_usage_events.started_at AND user_subscriptions.status = ?", "active").
		Joins("LEFT JOIN plans ON plans.id = user_subscriptions.plan_id").
		Where("ai_usage_events.started_at >= ? AND ai_usage_events.started_at < ?", start, end).
		Select("COALESCE(plans.code, CASE WHEN ai_usage_events.user_id IS NULL THEN 'guest' ELSE 'free' END) AS key, COUNT(*) AS events, COALESCE(SUM(ai_usage_events.final_credits), 0) AS credits, COALESCE(SUM(ai_usage_events.estimated_cost_usd_micros), 0) AS estimated_cost_usd_micros").
		Group("key").
		Order("estimated_cost_usd_micros DESC").
		Scan(&rows).Error
	return rows, err
}

func evaluateAlerts(metrics AIMetrics, cfg AlertConfig) []AlertResult {
	alerts := []AlertResult{}
	if cfg.DailyCostUSDMicros > 0 && metrics.EstimatedCostUSDMicros >= cfg.DailyCostUSDMicros {
		alerts = append(alerts, AlertResult{Code: "daily_ai_cost_threshold", Severity: "warning", Message: "Daily estimated AI cost crossed the configured threshold."})
	}
	if cfg.AbuseDailyCreditsThreshold > 0 && metrics.MaxSubjectDailyCredits >= int64(cfg.AbuseDailyCreditsThreshold) {
		alerts = append(alerts, AlertResult{Code: "ai_abuse_credit_threshold", Severity: "warning", Message: "AI credit usage crossed the configured abuse-review threshold."})
	}
	if cfg.FreeCostPerUserUSDMicros > 0 && metrics.FreeCostPerUserUSDMicros >= cfg.FreeCostPerUserUSDMicros {
		alerts = append(alerts, AlertResult{Code: "free_tier_cost_per_user_threshold", Severity: "warning", Message: "Free-tier cost per active user crossed the configured target."})
	}
	return alerts
}
