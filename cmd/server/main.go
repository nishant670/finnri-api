package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	// Before anything serves. A volume the server cannot write to used to be
	// discovered by whichever user next attached a receipt; it now says so in
	// the deploy log.
	httpserver.EnsureUploadStorage()
	if err := httpserver.BootstrapAdminUsers(cfg); err != nil {
		log.Printf("admin bootstrap failed, console has no bootstrapped owner: %v", err)
	}
	httpserver.StartMaintenanceJobs(cfg)
	httpserver.StartSubscriptionAutomation(cfg)
	httpserver.StartRefundAutomation(cfg)
	httpserver.StartCardStatementAutomation(cfg)
	httpserver.StartMonthlyReviewJob(cfg)
	r := httpserver.NewServer(cfg)
	// No WriteTimeout: AI reads and uploads are bounded by their own
	// per-request deadlines, and a blanket one would cut the slow ones short.
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// A deploy sends SIGTERM to the old container. `r.Run` died with it, so
	// whatever was in flight — an AI read that had already reserved credits, a
	// receipt half written — was cut off mid-request. Now the listener closes
	// first and requests already accepted get shutdownGrace to finish.
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	failed := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		log.Fatal(err)
	case <-stop.Done():
	}
	log.Printf("shutdown signal received; finishing in-flight requests")
	ctx, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown did not finish cleanly within %s: %v", shutdownGrace, err)
	}
}

// shutdownGrace is how long accepted requests get to finish after SIGTERM. It
// sits under the request timeout's default of 30 seconds and well inside the
// window a platform allows before it kills the container.
const shutdownGrace = 20 * time.Second

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
