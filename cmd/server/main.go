package main

import (
	"log"
	"os"
	"strings"
	"time"

	"finnri/internal/config"
	"finnri/internal/database"
	httpserver "finnri/internal/http"
	"finnri/internal/monitoring"

	"github.com/joho/godotenv"
)

func main() {
	loadEnv()
	if err := monitoring.Init(); err != nil {
		log.Printf("Sentry initialization failed; continuing without crash reporting: %v", err)
	}
	defer monitoring.Flush(2 * time.Second)
	database.Connect()

	cfg := config.Load()
	if err := httpserver.BootstrapAdminUsers(cfg); err != nil {
		log.Printf("admin bootstrap failed, console has no bootstrapped owner: %v", err)
	}
	httpserver.StartMaintenanceJobs(cfg)
	httpserver.StartSubscriptionAutomation(cfg)
	httpserver.StartRefundAutomation(cfg)
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
