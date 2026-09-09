package http

import (
	"fmt"
	"log"
	"sync"
	"time"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
	"gorm.io/gorm/clause"
)

var maintenanceState struct {
	sync.RWMutex
	LastRunAt *time.Time
	LastError string
}

type maintenanceResult struct {
	ExpiredCreditGrants     int
	AnonymousGuestRetention billing.AnonymousGuestRetentionResult
}

func StartMaintenanceJobs(cfg *config.Config) {
	if cfg == nil || !cfg.MaintenanceJobsEnabled {
		return
	}
	interval := time.Duration(cfg.MaintenanceIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	go func() {
		runMaintenanceAndLog(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			runMaintenanceAndLog(cfg)
		}
	}()
}

func runMaintenanceAndLog(cfg *config.Config) {
	result, err := runMaintenanceOnce(cfg)
	now := time.Now().UTC()
	maintenanceState.Lock()
	maintenanceState.LastRunAt = &now
	if err != nil {
		maintenanceState.LastError = err.Error()
		maintenanceState.Unlock()
		log.Printf("maintenance jobs failed: %v", err)
		return
	}
	maintenanceState.LastError = ""
	maintenanceState.Unlock()
	log.Printf(
		"maintenance jobs completed: expired_credit_grants=%d anonymous_guest_ai_usage=%d anonymous_guest_limit_events=%d anonymous_guest_daily_usage=%d anonymous_guest_keys=%d anonymous_guest_credit_ledger=%d anonymous_guest_credit_grants=%d",
		result.ExpiredCreditGrants,
		result.AnonymousGuestRetention.AIUsageEvents,
		result.AnonymousGuestRetention.AIUsageLimitEvents,
		result.AnonymousGuestRetention.DailyCreditUsage,
		result.AnonymousGuestRetention.GuestUsageKeys,
		result.AnonymousGuestRetention.CreditLedger,
		result.AnonymousGuestRetention.CreditGrants,
	)
}

func runMaintenanceOnce(cfg *config.Config) (maintenanceResult, error) {
	if cfg == nil {
		return maintenanceResult{}, fmt.Errorf("config is required")
	}
	retentionDays := cfg.AnonymousGuestRetentionDays
	if retentionDays <= 0 {
		retentionDays = int(billing.AnonymousGuestRetention / (24 * time.Hour))
	}

	service := billing.NewCreditService(database.DB)
	expired, err := service.ExpireCredits()
	if err != nil {
		return maintenanceResult{}, fmt.Errorf("expire credits: %w", err)
	}
	retention, err := service.PurgeAnonymousGuestUsage(time.Duration(retentionDays) * 24 * time.Hour)
	if err != nil {
		return maintenanceResult{}, fmt.Errorf("purge anonymous guest usage: %w", err)
	}
	if err := rollupAdminDailyMetrics(time.Now().UTC().AddDate(0, 0, -1)); err != nil {
		return maintenanceResult{}, fmt.Errorf("roll up admin metrics: %w", err)
	}
	return maintenanceResult{ExpiredCreditGrants: expired, AnonymousGuestRetention: retention}, nil
}

func rollupAdminDailyMetrics(day time.Time) error {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	active := len(activeUserIDs(start, end))
	var events []models.AIUsageEvent
	if err := database.DB.Where("started_at >= ? AND started_at < ?", start, end).Find(&events).Error; err != nil {
		return err
	}
	row := models.AdminDailyMetric{MetricDate: start.Format("2006-01-02"), ActiveUsers: active}
	for _, event := range events {
		row.AIEvents++
		row.AICredits += event.FinalCredits
		if event.Status == "succeeded" {
			row.SuccessfulAIEvents++
		}
		cost := event.EstimatedCostUSDMicros
		if event.ActualCostUSDMicros != nil {
			cost = *event.ActualCostUSDMicros
		}
		row.AICostUSDMicros += cost
	}
	return database.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "metric_date"}}, DoUpdates: clause.AssignmentColumns([]string{"active_users", "ai_events", "ai_credits", "ai_cost_usd_micros", "successful_ai_events", "updated_at"})}).Create(&row).Error
}
