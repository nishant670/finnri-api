package main

import (
	"log"
	"os"
	"strings"

	"finnri/internal/config"
	"finnri/internal/database"
	httpserver "finnri/internal/http"
	"finnri/internal/models"

	"github.com/joho/godotenv"
)

func main() {
	loadEnv()
	database.Connect()
	// Must precede AutoMigrate: it renames constraints the raw schema named
	// for itself to the names the GORM models expect. Without it AutoMigrate
	// tries to drop a constraint that never existed and kills the boot.
	if err := database.PrepareForAutoMigrate(); err != nil {
		log.Fatalf("database constraint reconciliation failed: %v", err)
	}
	if err := database.DB.AutoMigrate(
		&models.User{},
		&models.AuthSession{},
		&models.AuthVerification{},
		&models.AdminUser{},
		&models.AdminAuditLog{},
		&models.AdminDailyMetric{},
		&models.Account{},
		&models.Entry{},
		&models.QuickPrompt{},
		&models.Notification{},
		&models.Feedback{},
		&models.Budget{},
		&models.BudgetAlert{},
		&models.MonthlyReview{},
		&models.Subscription{},
		&models.SubscriptionReminder{},
		&models.SubscriptionOccurrence{},
		&models.CardStatement{},
		&models.CardStatementPayment{},
		&models.CardStatementReminder{},
		&models.CardEMIPlan{},
		&models.CardEMIInstallment{},
		&models.PushDevice{},
		&models.SplitFriend{},
		&models.SplitGroup{},
		&models.SplitGroupMember{},
		&models.SplitBill{},
		&models.SplitParticipant{},
		&models.SplitSettlement{},
		&models.SplitFriendMerge{},
		&models.Payment{},
		&models.PaymentWebhookEvent{},
	); err != nil {
		log.Fatalf("database schema migration failed: %v", err)
	}
	if err := database.EnsureRuntimeSchema(); err != nil {
		log.Fatalf("database runtime schema check failed: %v", err)
	}

	cfg := config.Load()
	if err := httpserver.BootstrapAdminUsers(cfg); err != nil {
		log.Printf("admin bootstrap failed, console has no bootstrapped owner: %v", err)
	}
	httpserver.StartMaintenanceJobs(cfg)
	httpserver.StartSubscriptionAutomation(cfg)
	httpserver.StartCardStatementAutomation(cfg)
	httpserver.StartMonthlyReviewJob(cfg)
	r := httpserver.NewServer(cfg)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}

func loadEnv() {
	loadEnvFile(".env")
	loadEnvFile(config.ResolveBackendPath(".env"))
}

func loadEnvFile(path string) {
	values, err := godotenv.Read(path)
	if err != nil {
		return
	}
	for key, value := range values {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			_ = os.Setenv(key, value)
		}
	}
}
