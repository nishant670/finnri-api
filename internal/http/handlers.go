package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	timepkg "time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"
	"gorm.io/gorm"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/mailer"
	"finnri/internal/models"
	"finnri/internal/payments"
)

type Server struct {
	cfg               *config.Config
	validator         *gojsonschema.Schema
	parser            ai.Parser
	mailer            mailer.Sender
	payments          *payments.Client
	providerCircuit   *providerCircuitBreaker
	providerCircuitMu sync.Mutex
}

// emailSender returns the configured mail driver. A Server built directly —
// which every unit test does — has no mailer field, so it falls back to one
// derived from cfg, and finally to the no-op sender that fails loudly. The one
// thing it must never do is return nil and panic inside a request.
func (s *Server) emailSender() mailer.Sender {
	if s.mailer != nil {
		return s.mailer
	}
	if s.cfg != nil {
		return mailer.FromConfig(s.cfg)
	}
	return mailer.NotConfigured{}
}

func NewServer(cfg *config.Config) *gin.Engine {
	r := gin.New()
	// Without this Gin trusts every proxy, so a client can pick its own
	// X-Forwarded-For and get a fresh rate-limit bucket on each request.
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Fatalf("invalid TRUSTED_PROXIES: %v", err)
	}
	r.Use(gin.Recovery())
	// Before anything that keys on the caller: the rate limiters, the guest
	// trial grant, and the admin audit log all read what this settles.
	r.Use(resolveClientIP(cfg))
	r.Use(cors(cfg))
	r.Use(logging())

	if cfg.AuthBearer != "" {
		r.Use(func(c *gin.Context) {
			if skipsStaticBearer(c.Request.URL.Path) {
				c.Next()
				return
			}
			if c.GetHeader("Authorization") != "Bearer "+cfg.AuthBearer {
				log.Printf("[ERROR] Auth Fail: %s (authorization header present: %t)", c.Request.URL.Path, c.GetHeader("Authorization") != "")
				c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		})
	}

	schemaPath, err := filepath.Abs(config.ResolveBackendPath(filepath.Join("schemas", "expense_entry.schema.json")))
	if err != nil {
		panic(err)
	}
	loader := gojsonschema.NewReferenceLoader((&url.URL{Scheme: "file", Path: schemaPath}).String())
	schema, err := gojsonschema.NewSchema(loader)
	if err != nil {
		panic(err)
	}

	openai := ai.NewOpenAIClient(cfg)

	emailSender := mailer.FromConfig(cfg)
	log.Printf("[startup] email provider: %s", emailSender.Name())

	razorpay := payments.NewClient(payments.RazorpayConfig{
		KeyID:         cfg.RazorpayKeyID,
		KeySecret:     cfg.RazorpayKeySecret,
		WebhookSecret: cfg.RazorpayWebhookSecret,
		BaseURL:       cfg.RazorpayBaseURL,
	})
	log.Printf("[startup] payment provider configured: %v", razorpay.Configured())

	s := &Server{cfg: cfg, validator: schema, parser: openai, mailer: emailSender, payments: razorpay}
	// Auth
	authLimited := r.Group("/v1/auth")
	authLimited.Use(jsonRequestLimits(cfg), rateLimit(cfg, "auth"))
	{
		authLimited.POST("/guest", s.authGuest)
		authLimited.POST("/identify", s.authIdentify)
		authLimited.POST("/otp/send", s.authOtpSend)
		authLimited.POST("/otp/verify", s.authOtpVerify)
		authLimited.POST("/google", s.authGoogle)
		authLimited.POST("/register", s.authRegister)
		authLimited.POST("/login", s.authLogin)
		authLimited.POST("/admin/login", s.adminLogin)
		authLimited.POST("/admin/logout", s.adminLogout)
		authLimited.POST("/pin/reset", s.authPinReset)
	}

	billingPublic := r.Group("/v1/billing")
	billingPublic.Use(jsonRequestLimits(cfg))
	{
		billingPublic.GET("/plans", rateLimit(cfg, "billing"), s.listBillingPlans)
		// Read by the hosted pay page, which opens in a browser tab with no
		// Finnri session. Returns nothing secret and nothing about the buyer.
		billingPublic.GET("/checkout/:order_id", rateLimit(cfg, "billing"), s.getBillingCheckoutOrder)
		billingPublic.POST("/webhook", webhookRateLimit(cfg), s.handleBillingWebhook)
	}

	// A recipient must be able to understand an invite before creating or
	// signing into an account. This preview deliberately exposes only the group
	// name, inviter display name, member count and expiry.
	r.GET("/v1/split/invites/:token/preview", jsonRequestLimits(cfg), rateLimit(cfg, "split-invite-preview"), s.previewSplitGroupInvite)

	admin := r.Group("/v1/admin")
	admin.Use(jsonRequestLimits(cfg), adminRateLimit(cfg), s.requireAdminSession(), s.adminAuditMiddleware())
	{
		admin.GET("/me", s.getAdminMe)
		admin.GET("/overview", s.getAdminOverview)
		admin.GET("/users", s.listAdminUsers)
		admin.GET("/users/:id", s.getAdminUser)
		admin.GET("/users/:id/ai-usage", s.listAdminUserAIUsage)
		admin.GET("/users/:id/credits", s.getAdminUserCredits)
		admin.GET("/users/:id/activity", s.getAdminUserActivity)
		admin.POST("/users/:id/credits/adjustments", requireAdminRole(models.AdminRoleSupport), s.createUserCreditAdjustment)
		admin.GET("/ai/metrics", s.getAIMetrics)
		admin.GET("/ai/metrics/timeseries", s.getAIMetricsTimeseries)
		admin.GET("/ai/usage", s.listAdminAIUsage)
		admin.GET("/ai/limit-events", s.listAdminAILimitEvents)
		admin.GET("/ai/model-pricing", s.listAIModelPricing)
		admin.PUT("/ai/model-pricing", requireAdminRole(models.AdminRoleOwner), s.upsertAIModelPricing)
		admin.POST("/credits/adjustments", requireAdminRole(models.AdminRoleSupport), s.createCreditAdjustment)
		admin.GET("/credits/summary", s.getAdminCreditsSummary)
		admin.GET("/credits/ledger", s.listAdminCreditLedger)
		admin.GET("/subscriptions", s.listAdminSubscriptions)
		admin.GET("/revenue", s.getAdminRevenue)
		admin.GET("/plans", s.listAdminPlans)
		admin.PUT("/plans/:code", requireAdminRole(models.AdminRoleOwner), s.updateAdminPlan)
		admin.GET("/billing/lifetime-quotes", s.listLifetimeQuoteRequests)
		admin.PATCH("/billing/lifetime-quotes/:id", requireAdminRole(models.AdminRoleSupport), s.updateLifetimeQuoteRequest)
		admin.GET("/ai/abuse-blocks", s.listAIAbuseBlocks)
		admin.POST("/ai/abuse-blocks", requireAdminRole(models.AdminRoleSupport), s.createAIAbuseBlock)
		admin.PATCH("/ai/abuse-blocks/:id", requireAdminRole(models.AdminRoleSupport), s.updateAIAbuseBlock)
		admin.GET("/feedback", s.listAdminFeedback)
		admin.GET("/feedback/stats", s.getAdminFeedbackStats)
		admin.PATCH("/feedback/:id", requireAdminRole(models.AdminRoleSupport), s.updateAdminFeedback)
		admin.GET("/analytics/signups", s.getAdminSignups)
		admin.GET("/analytics/activation", s.getAdminActivation)
		admin.GET("/analytics/retention", s.getAdminRetention)
		admin.GET("/analytics/engagement", s.getAdminEngagement)
		admin.GET("/analytics/feature-adoption", s.getAdminFeatureAdoption)
		admin.GET("/audit-log", s.listAdminAuditLog)
		admin.GET("/health", s.getAdminHealth)
		admin.GET("/export/:resource", s.exportAdminCSV)
		admin.GET("/admin-users", requireAdminRole(models.AdminRoleOwner), s.listAdminIdentities)
		admin.POST("/admin-users", requireAdminRole(models.AdminRoleOwner), s.createAdminIdentity)
		admin.PATCH("/admin-users/:id", requireAdminRole(models.AdminRoleOwner), s.updateAdminIdentity)
	}

	// Protected Routes (User Token)
	authorized := r.Group("/v1")
	authorized.Use(AuthMiddleware())
	{
		authorized.POST("/parse", uploadRequestLimits(cfg), rateLimit(cfg, "ai"), s.handleParse)
		authorized.POST("/entries", s.saveEntry)
		authorized.GET("/entries", s.listEntries)
		authorized.GET("/entries/export", s.exportEntriesCSV)
		authorized.GET("/merchants/suggestions", s.listMerchantSuggestions)
		authorized.GET("/categories", s.listCategories)
		authorized.GET("/reports/transactions/summary", s.getTransactionSummaryReport)
		authorized.GET("/entries/:id", s.getEntry)
		authorized.PUT("/entries/:id", s.updateEntry)
		authorized.DELETE("/entries/:id", s.deleteEntry)
		authorized.GET("/quick-prompts", s.listQuickPrompts)
		authorized.POST("/quick-prompts", s.saveQuickPrompt)
		authorized.PUT("/quick-prompts/:id", s.updateQuickPrompt)
		authorized.DELETE("/quick-prompts/:id", s.deleteQuickPrompt)
		authorized.PUT("/user", s.updateProfile)
		authorized.DELETE("/user", s.deleteUser)
		authorized.POST("/auth/logout", s.authLogout)
		authorized.POST("/auth/sessions/revoke-all", s.authRevokeAllSessions)
		authorized.POST("/upload", uploadRequestLimits(cfg), s.handleUpload)
		authorized.POST("/feedback", s.createFeedback)

		// Billing and AI credit visibility
		authorized.GET("/billing/status", s.getBillingStatus)
		authorized.POST("/billing/checkout", s.createBillingCheckout)
		authorized.POST("/billing/lifetime-quote/request", s.requestLifetimeQuote)
		authorized.GET("/ai/usage", s.listAIUsage)
		authorized.GET("/ai/credits", s.getAICredits)

		// Accounts
		authorized.POST("/accounts", s.saveAccount)
		authorized.GET("/accounts", s.listAccounts)
		authorized.GET("/account-providers", listAccountProviders)
		authorized.PUT("/accounts/:id", s.updateAccount)
		authorized.POST("/accounts/:id/paid-off", s.markCardPaidOff)
		authorized.DELETE("/accounts/:id", s.deleteAccount)

		// Credit card statements
		authorized.GET("/accounts/:id/statements", s.listCardStatements)
		authorized.POST("/accounts/:id/statements", s.saveCardStatement)
		authorized.POST("/accounts/:id/statements/alert", jsonRequestLimits(cfg), rateLimit(cfg, "ai"), s.importCardStatementAlert)
		authorized.GET("/statements/upcoming", s.listUpcomingStatements)
		authorized.GET("/statements/:id", s.getCardStatement)
		authorized.DELETE("/statements/:id", s.deleteCardStatement)
		authorized.POST("/statements/:id/payments", s.recordCardStatementPayment)
		authorized.DELETE("/statements/:id/payments/:paymentId", s.deleteCardStatementPayment)
		authorized.POST("/statements/:id/diff", s.diffCardStatement)
		authorized.POST("/statements/:id/import", s.importCardStatementLines)
		authorized.POST("/statements/:id/upload", uploadRequestLimits(cfg), s.uploadCardStatementPDF)
		authorized.POST("/statements/:id/screenshots", uploadRequestLimits(cfg), rateLimit(cfg, "ai"), s.uploadCardStatementScreenshots)

		// Card EMI plans
		authorized.GET("/accounts/:id/emi-plans", s.listCardEMIPlans)
		authorized.POST("/accounts/:id/emi-plans", s.createCardEMIPlan)
		authorized.GET("/emi-plans/:id", s.getCardEMIPlan)
		authorized.POST("/emi-plans/:id/foreclose", s.forecloseCardEMIPlan)
		authorized.DELETE("/emi-plans/:id", s.deleteCardEMIPlan)

		// Insights
		authorized.GET("/monthly-review", s.getMonthlyReview)
		authorized.POST("/monthly-review/send", s.sendMonthlyReviewNow)
		authorized.GET("/dashboard", s.getDashboard)
		authorized.GET("/insights", s.requireEntitlement(billing.FeatureAdvancedInsights), s.getInsights)
		authorized.POST("/recurring-candidates/decision", s.saveRecurringCandidateDecision)
		authorized.POST("/recurring-candidates/track", s.trackRecurringCandidates)

		// Notifications
		authorized.GET("/notifications", s.listNotifications)
		authorized.GET("/notifications/unread-count", s.getUnreadNotificationCount)
		authorized.PATCH("/notifications/read-all", s.markAllNotificationsRead)
		authorized.PATCH("/notifications/:id/read", s.markNotificationRead)
		authorized.DELETE("/notifications/:id", s.deleteNotification)

		// Budgets
		budgets := authorized.Group("/budgets", s.requireEntitlement(billing.FeatureBudgets))
		{
			budgets.POST("", s.createBudget)
			budgets.GET("", s.listBudgets)
			budgets.PUT("/:id", s.updateBudget)
			budgets.DELETE("/:id", s.deleteBudget)
		}

		// Subscriptions
		authorized.POST("/subscriptions", s.createSubscription)
		authorized.GET("/subscriptions", s.listSubscriptions)
		authorized.POST("/subscriptions/reminders", s.requireEntitlement(billing.FeatureSubscriptionReminders), s.createSubscriptionReminders)
		authorized.POST("/subscriptions/sync", s.syncSubscriptionAutomationNow)
		authorized.PUT("/subscriptions/:id", s.updateSubscription)
		authorized.DELETE("/subscriptions/:id", s.deleteSubscription)
		authorized.POST("/subscriptions/:id/mark-paid", s.markSubscriptionPaid)
		authorized.GET("/subscription-occurrences", s.listSubscriptionOccurrences)
		authorized.POST("/subscription-occurrences/:id/confirm", s.confirmSubscriptionOccurrence)
		authorized.POST("/subscription-occurrences/:id/revert", s.revertSubscriptionOccurrence)
		authorized.POST("/push-devices", s.registerPushDevice)
		authorized.DELETE("/push-devices", s.unregisterPushDevice)

		// Being invited is not using the feature. These three sit outside the
		// split entitlement on purpose: somebody has to be able to see who
		// invited them and say yes, whatever plan they are on, or an invitation
		// becomes a bill before it is even an answer. Keep them here even if the
		// split ledger is later put behind a paid plan.
		invites := authorized.Group("/split")
		{
			invites.GET("/pending-invites", s.listPendingSplitGroupInvites)
			invites.GET("/invites/:token", s.getSplitGroupInvite)
			invites.POST("/invites/:token/accept", s.acceptSplitGroupInvite)
		}

		// Split ledger
		split := authorized.Group("/split", s.requireEntitlement(billing.FeatureSplitLedger))
		{
			split.POST("/friends", s.createSplitFriend)
			split.GET("/friends", s.listSplitFriends)
			split.PUT("/friends/:id", s.updateSplitFriend)
			split.POST("/friends/:id/merge-into/:target", s.mergeSplitFriend)
			split.DELETE("/friends/:id", s.archiveSplitFriend)
			split.POST("/groups", s.createSplitGroup)
			split.GET("/groups", s.listSplitGroups)
			split.PUT("/groups/:id", s.updateSplitGroup)
			split.PUT("/groups/:id/default-split", s.updateSplitGroupDefaultSplit)
			split.DELETE("/groups/:id", s.archiveSplitGroup)
			split.POST("/groups/:id/invite-link", s.createSplitGroupInvite)
			split.GET("/groups/:id/invites", s.listSplitGroupDirectInvites)
			split.POST("/groups/:id/invites", s.createSplitGroupDirectInvite)
			split.DELETE("/groups/:id/invites/:invite_id", s.revokeSplitGroupDirectInvite)
			split.POST("/groups/:id/leave", s.leaveSplitGroup)
			split.POST("/bills", s.createSplitBill)
			split.GET("/bills", s.listSplitBills)
			split.GET("/bills/by-entry/:entry_id", s.getSplitBillByEntry)
			split.PUT("/bills/:id", s.updateSplitBill)
			split.DELETE("/bills/:id", s.deleteSplitBill)
			split.POST("/settlements", s.createSplitSettlement)
			split.GET("/settlements", s.listSplitSettlements)
			split.GET("/activity", s.listSplitActivity)
			split.GET("/balances", s.listSplitBalances)
		}

		// Financial tools
		authorized.POST("/tools/emi/calculate", s.calculateEMI)
	}

	// Receipts are user-supplied bytes served from our own origin. The upload
	// handler already restricts them to images and PDFs, but pin the type down
	// so a browser cannot be talked into re-interpreting one as markup.
	uploads := r.Group("/"+uploadDir, func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; object-src 'none'; sandbox")
		c.Next()
	})
	uploads.Static("/", "./"+uploadDir)
	// Railway gates deploys on this route, so it has to fail when Postgres is
	// gone. A static 200 reports the service healthy while every data route
	// returns 500, which hides an outage instead of surfacing it.
	r.GET("/health", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		err := errors.New("database not initialised")
		if database.DB != nil {
			var sqlDB *sql.DB
			if sqlDB, err = database.DB.DB(); err == nil {
				err = sqlDB.PingContext(ctx)
			}
		}
		if err != nil {
			log.Printf("[ERROR] health check database ping failed: %v", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "database_unreachable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func skipsStaticBearer(path string) bool {
	return path == "/health" ||
		strings.HasPrefix(path, "/v1/auth/") ||
		strings.HasPrefix(path, "/v1/entries") ||
		strings.HasPrefix(path, "/v1/quick-prompts") ||
		strings.HasPrefix(path, "/v1/user") ||
		strings.HasPrefix(path, "/v1/insights") ||
		strings.HasPrefix(path, "/v1/dashboard") ||
		// The dashboard hands the app recurring candidates, so the endpoints
		// that answer for them belong to the same session-authenticated
		// surface. Without this the static bearer gates them and every
		// dismiss, snooze and track from the app answers 401 — which is what
		// happened to the decision endpoint from the day it shipped.
		strings.HasPrefix(path, "/v1/recurring-candidates") ||
		strings.HasPrefix(path, "/v1/merchants") ||
		strings.HasPrefix(path, "/v1/categories") ||
		strings.HasPrefix(path, "/v1/accounts") ||
		strings.HasPrefix(path, "/v1/account-providers") ||
		// Statements nested under a card are covered by the line above, but
		// the top-level ones are not: "/v1/statements/:id" and
		// "/v1/statements/upcoming" do not have "/v1/accounts" as a prefix.
		strings.HasPrefix(path, "/v1/statements") ||
		// Same for EMI plans addressed by their own id.
		strings.HasPrefix(path, "/v1/emi-plans") ||
		strings.HasPrefix(path, "/v1/notifications") ||
		strings.HasPrefix(path, "/v1/feedback") ||
		strings.HasPrefix(path, "/v1/budgets") ||
		strings.HasPrefix(path, "/v1/subscriptions") ||
		// Not covered by the line above: "/v1/subscription-occurrences" does
		// not have "/v1/subscriptions" as a prefix. Home's autopay review card
		// — Confirm and Correct/revert — and the pending-occurrence fetch all
		// answered 401 wherever AUTH_BEARER is set, which is every deployed
		// environment and the local .env.
		strings.HasPrefix(path, "/v1/subscription-occurrences") ||
		// Push registration. With this gated, no device token was ever stored,
		// so every push the server has tried to send had nobody to send it to
		// — including the monthly review this list was widened for.
		strings.HasPrefix(path, "/v1/push-devices") ||
		strings.HasPrefix(path, "/v1/upload") ||
		strings.HasPrefix(path, "/v1/reports") ||
		strings.HasPrefix(path, "/v1/monthly-review") ||
		strings.HasPrefix(path, "/v1/split") ||
		strings.HasPrefix(path, "/v1/tools") ||
		strings.HasPrefix(path, "/v1/billing") ||
		strings.HasPrefix(path, "/v1/ai") ||
		strings.HasPrefix(path, "/v1/parse")
}

// staticBearerGuardedPrefixes is the deliberate half of the list above.
//
// The prefix list has now drifted twice — M1 found the recurring-candidate
// endpoints had answered 401 since the day they shipped, and M4 found three
// more the same way. It drifts because it is written as "what to let through",
// so a new route is gated by *omission*: nobody adding an endpoint thinks to
// come here, and the failure is invisible until someone runs the app against a
// backend with AUTH_BEARER set.
//
// This inverts it. Everything under /v1 must skip the static bearer unless it
// is named here, and TestEverySessionRouteSkipsTheStaticBearer walks the
// registered routes and fails when something new is not. A route that genuinely
// should be gated is now a deliberate line in this list rather than an
// oversight in the other one.
//
// It contains only /v1/admin, which carries its own admin bearer — meaning the
// static bearer currently gates nothing that is not separately authenticated.
// That is worth a decision on its own and is deliberately not made here.
var staticBearerGuardedPrefixes = []string{"/v1/admin"}

func (s *Server) handleParse(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timepkg.Duration(s.cfg.ReqTimeoutSec)*timepkg.Second)
	defer cancel()

	if s.rejectIfAIParseDisabled(c) || s.rejectIfProviderCircuitOpen(c) {
		return
	}

	subject, subjectOK := parseCreditSubject(c)
	if !subjectOK {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return
	}
	if s.rejectIfAIAbuseBlocked(c, subject) || s.rejectIfAIFailureCooldown(c, subject) {
		return
	}

	if err := c.Request.ParseMultipartForm(s.cfg.MaxUploadMB * 1024 * 1024); err != nil && requestBodyTooLarge(err) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
		return
	}

	tz := c.PostForm("tz")
	if tz == "" {
		tz = s.cfg.TZDefault
	}

	var transcript string
	var audioBytes []byte
	var audioSize int64
	actionCode := ai.ActionTransactionParseText
	var usageEvent *models.AIUsageEvent
	var creditService *billing.CreditService
	file, header, err := c.Request.FormFile("audio")
	if err == nil {
		defer file.Close()
		if header.Size > s.cfg.MaxUploadMB*1024*1024 {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large"})
			return
		}
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, file); err != nil {
			if requestBodyTooLarge(err) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
				return
			}
			c.JSON(400, gin.H{"error": "failed to read file"})
			return
		}
		audioBytes = buf.Bytes()
		audioSize = int64(len(audioBytes))
		if audioSize == 0 {
			c.JSON(400, gin.H{"error": "empty_audio"})
			return
		}
		var tooLong bool
		actionCode, tooLong = classifyVoiceParseAction(audioSize)
		if tooLong {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "audio_too_long_for_ai"})
			return
		}
		creditService = billing.NewCreditService(database.DB)
		if s.rejectIfUnpaidVoiceTooLong(c, creditService, subject, actionCode, audioSize) {
			return
		}
		event, allowance, reserveErr := creditService.ReserveCredits(subject, actionCode, parseIdempotencyHeader(c))
		if reserveErr != nil {
			writeCreditReservationError(c, allowance, reserveErr)
			return
		}
		usageEvent = &event

		if t, err := s.parser.Transcribe(ctx, header.Filename, audioBytes); err == nil {
			transcript = t
		} else {
			s.recordAIProviderFailure()
			log.Printf("stt error: %v", err)
			_, _ = creditService.FinalizeUsage(event.ID, billing.ProviderUsage{
				Status:            billing.UsageStatusFailedAfterProvider,
				ErrorCode:         "transcription_failed",
				AudioBytes:        &audioSize,
				SecondaryModel:    s.cfg.OpenAIWhisper,
				SecondaryProvider: "openai",
			})
			c.JSON(http.StatusBadGateway, gin.H{"error": "transcription_failed"})
			return
		}
	}

	if transcript == "" {
		transcript = c.PostForm("hint_text")
	}
	if strings.TrimSpace(transcript) == "" {
		if usageEvent != nil {
			_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
				Status:    billing.UsageStatusFailedAfterProvider,
				ErrorCode: "empty_transcript",
			})
		}
		c.JSON(400, gin.H{"error": "no audio or hint_text provided"})
		return
	}
	if s.cfg.MaxTranscriptChars > 0 && utf8.RuneCountInString(transcript) > s.cfg.MaxTranscriptChars {
		if usageEvent != nil {
			_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
				Status:    billing.UsageStatusFailedAfterProvider,
				ErrorCode: "transcript_too_long",
			})
		}
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":          "transcript_too_long",
			"max_characters": s.cfg.MaxTranscriptChars,
		})
		return
	}
	if usageEvent == nil {
		creditService = billing.NewCreditService(database.DB)
		event, allowance, reserveErr := creditService.ReserveCredits(subject, actionCode, parseIdempotencyHeader(c))
		if reserveErr != nil {
			writeCreditReservationError(c, allowance, reserveErr)
			return
		}
		usageEvent = &event
	}

	parsed, parseUsage, err := s.parser.ParseText(ctx, transcript, tz)
	// Every finalisation below this line follows a call the provider actually
	// answered, so each one carries what that answer cost. The `could_not_parse`
	// branch is the exception: the call itself failed and reported nothing.
	promptTokens, completionTokens, totalTokens := tokenPointers(parseUsage)
	if err != nil {
		s.recordAIProviderFailure()
		inputChars := utf8.RuneCountInString(transcript)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:     billing.UsageStatusFailedAfterProvider,
			ErrorCode:  "could_not_parse",
			InputChars: &inputChars,
			AudioBytes: optionalInt64(audioSize),
		})
		c.JSON(422, gin.H{"error": "could_not_parse", "transcript": transcript})
		return
	}

	var parsedObj map[string]any
	if err := json.Unmarshal(parsed, &parsedObj); err != nil {
		inputChars := utf8.RuneCountInString(transcript)
		responseBytes := len(parsed)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "invalid_parse_response",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputChars:       &inputChars,
			ResponseBytes:    &responseBytes,
			AudioBytes:       optionalInt64(audioSize),
		})
		c.JSON(500, gin.H{"error": "invalid_parse_response"})
		return
	}

	// The two directions split here, before any draft normalisation runs. A
	// question is not a transaction with empty fields — normalising it as one
	// would file it under Misc, flag five fields for confirmation and hand the
	// app a draft it must not save.
	//
	// `parsedQuestionQuery` is the model's answer plus a deterministic backstop
	// for the fragments it routes to capture — "today spend" and its kind. See
	// parse_intent.go.
	if rawQuery, isQuestion := parsedQuestionQuery(parsedObj, transcript); isQuestion {
		s.answerParsedQuestion(c, answeredQuestionRequest{
			userID:        c.MustGet("userID").(uint),
			transcript:    transcript,
			tz:            tz,
			rawQuery:      rawQuery,
			creditService: creditService,
			usageEventID:  usageEvent.ID,
			subject:       subject,
			actionCode:    actionCode,
			inputChars:    utf8.RuneCountInString(transcript),
			responseBytes: len(parsed),
			audioSize:     audioSize,
			reported:      parseUsage,
		})
		return
	}

	normalizeParsedDraft(parsedObj, transcript)
	if !parsedDraftHasTransactionSignal(parsedObj) {
		inputChars := utf8.RuneCountInString(transcript)
		responseBytes := len(parsed)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "non_transactional_prompt",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputChars:       &inputChars,
			ResponseBytes:    &responseBytes,
			AudioBytes:       optionalInt64(audioSize),
		})
		c.JSON(422, gin.H{
			"error":      "non_transactional_prompt",
			"message":    "Please describe an expense, income, bill, subscription, split, or payment to add.",
			"transcript": transcript,
		})
		return
	}
	parsed, err = json.Marshal(parsedObj)
	if err != nil {
		inputChars := utf8.RuneCountInString(transcript)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "serialization_failed",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputChars:       &inputChars,
			AudioBytes:       optionalInt64(audioSize),
		})
		c.JSON(500, gin.H{"error": "serialization_failed"})
		return
	}

	res, err := s.validator.Validate(gojsonschema.NewBytesLoader(parsed))
	if err != nil {
		inputChars := utf8.RuneCountInString(transcript)
		responseBytes := len(parsed)
		_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "validation_failed",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputChars:       &inputChars,
			ResponseBytes:    &responseBytes,
			AudioBytes:       optionalInt64(audioSize),
		})
		c.JSON(500, gin.H{"error": "validation_failed"})
		return
	}
	if !res.Valid() {
		d := []string{}
		for _, e := range res.Errors() {
			d = append(d, e.String())
		}
		// The credit is already spent, so a draft that trips the schema is
		// worth one more attempt before it becomes nothing. Almost every
		// failure that survives the normalizer is in an optional block — a
		// split hint, a subscription hint — attached to a transaction that is
		// itself perfectly readable. Dropping the block costs the hint and
		// keeps the capture; refusing the draft costs both.
		repaired, dropped, recovered := s.repairInvalidParsedDraft(parsedObj)
		if !recovered {
			log.Printf("parse schema_invalid: %v", d)
			inputChars := utf8.RuneCountInString(transcript)
			responseBytes := len(parsed)
			_, _ = creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
				Status:           billing.UsageStatusFailedAfterProvider,
				ErrorCode:        "schema_invalid",
				Provider:         "openai",
				Model:            s.cfg.OpenAILlmModel,
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      totalTokens,
				InputChars:       &inputChars,
				ResponseBytes:    &responseBytes,
				AudioBytes:       optionalInt64(audioSize),
			})
			c.JSON(422, gin.H{
				"error":      "schema_invalid",
				"message":    "I heard the words but could not turn them into a transaction.",
				"details":    d,
				"transcript": transcript,
			})
			return
		}
		log.Printf("parse schema_invalid recovered by dropping %v: %v", dropped, d)
		parsed = repaired
	}

	credits, err := s.finalizeParseSuccess(
		creditService,
		usageEvent.ID,
		subject,
		actionCode,
		utf8.RuneCountInString(transcript),
		len(parsed),
		audioSize,
		parseUsage,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": "credit_finalization_failed"})
		return
	}
	for key, value := range credits {
		parsedObj[key] = value
	}
	c.JSON(200, parsedObj)
}

// parseDraftRepairs lists the optional blocks to try removing, narrowest first,
// when a normalised draft still fails the schema.
var parseDraftRepairs = [][]string{
	{"split_candidate_details"},
	{"subscription_candidate"},
	{"split_candidate_details", "subscription_candidate"},
}

// repairInvalidParsedDraft retries validation without the optional candidate
// blocks and, on the first combination that passes, commits it onto the draft.
//
// It reports which blocks it dropped so the caller can log what was lost. The
// user is told through `clarifications` — the same channel the review sheet
// already uses to ask about anything the parse was unsure of — rather than
// through silence.
func (s *Server) repairInvalidParsedDraft(entry map[string]any) ([]byte, []string, bool) {
	for _, fields := range parseDraftRepairs {
		attempt := make(map[string]any, len(entry))
		for key, value := range entry {
			attempt[key] = value
		}
		dropped := []string{}
		for _, field := range fields {
			if attempt[field] == nil {
				continue
			}
			attempt[field] = nil
			dropped = append(dropped, field)
			switch field {
			case "split_candidate_details":
				attempt["split_candidate"] = nil
				attempt["clarifications"] = appendUniqueAnyString(
					attempt["clarifications"],
					"I could not read the split details. Turn on Split and pick the group or friends.",
				)
			case "subscription_candidate":
				attempt["recurring_candidate"] = nil
				attempt["clarifications"] = appendUniqueAnyString(
					attempt["clarifications"],
					"I could not read the recurring details. Set them up here if this repeats.",
				)
			}
		}
		if len(dropped) == 0 {
			continue
		}
		repaired, err := json.Marshal(attempt)
		if err != nil {
			continue
		}
		result, err := s.validator.Validate(gojsonschema.NewBytesLoader(repaired))
		if err != nil || !result.Valid() {
			continue
		}
		for key, value := range attempt {
			entry[key] = value
		}
		return repaired, dropped, true
	}
	return nil, nil, false
}

// finalizeParseSuccess settles the credit reservation for a completed provider
// call and returns the credit fields every parse response carries.
//
// Both directions charge the same action. The reservation is made before the
// text is read, so at that point nobody — not the app, not this handler, not
// the model — knows yet whether the user is recording an expense or asking
// about one. Billing a question differently would mean reserving twice, or
// reserving after the provider call and letting an over-limit user through.
func (s *Server) finalizeParseSuccess(
	creditService *billing.CreditService,
	usageEventID uint,
	subject billing.CreditSubject,
	actionCode ai.ActionCode,
	inputChars int,
	responseBytes int,
	audioSize int64,
	reported ai.Usage,
) (map[string]any, error) {
	prompt, completion, total := tokenPointers(reported)
	providerUsage := billing.ProviderUsage{
		Status:           billing.UsageStatusSucceeded,
		Provider:         "openai",
		Model:            s.cfg.OpenAILlmModel,
		InputChars:       &inputChars,
		ResponseBytes:    &responseBytes,
		AudioBytes:       optionalInt64(audioSize),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
	if audioSize > 0 {
		providerUsage.SecondaryProvider = "openai"
		providerUsage.SecondaryModel = s.cfg.OpenAIWhisper
	}
	finalized, err := creditService.FinalizeUsage(usageEventID, providerUsage)
	if err != nil {
		return nil, err
	}
	s.recordAIProviderSuccess()
	s.logCostControlAlerts()
	status, _ := creditService.CheckAllowance(subject, actionCode)
	return map[string]any{
		"credits_charged":         finalized.FinalCredits,
		"credits_remaining_today": status.DailyRemaining,
		"credits_remaining_total": status.AvailableCredits,
		"plan_code":               status.PlanCode,
	}, nil
}

func parseCreditSubject(c *gin.Context) (billing.CreditSubject, bool) {
	value, exists := c.Get("user")
	if !exists {
		return billing.CreditSubject{}, false
	}
	user, ok := value.(*models.User)
	if !ok || user == nil || user.ID == 0 {
		return billing.CreditSubject{}, false
	}
	if user.IsGuest {
		if user.DeviceID == nil || !validGuestDeviceFingerprint(*user.DeviceID) {
			return billing.CreditSubject{}, false
		}
		return billing.SubjectForGuestDeviceID(*user.DeviceID), true
	}
	return billing.SubjectForUser(user.ID), true
}

func classifyVoiceParseAction(audioBytes int64) (ai.ActionCode, bool) {
	switch {
	case audioBytes <= 512*1024:
		return ai.ActionTransactionParseVoiceShort, false
	case audioBytes <= 1536*1024:
		return ai.ActionTransactionParseVoiceMedium, false
	default:
		return "", true
	}
}

func parseIdempotencyHeader(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader("Idempotency-Key"))
}

func writeCreditReservationError(c *gin.Context, allowance billing.AllowanceResult, err error) {
	if errors.Is(err, billing.ErrInvalidCreditSubject) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credit_subject"})
		return
	}
	if !errors.Is(err, billing.ErrAllowanceDenied) {
		c.JSON(500, gin.H{"error": "credit_reservation_failed"})
		return
	}

	switch allowance.Reason {
	case billing.AllowanceDailyLimitReached:
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":       billing.AllowanceDailyLimitReached,
			"daily_limit": allowance.DailyLimit,
			"used_today":  allowance.UsedToday,
			"reset_at":    nextCreditResetAt(time.Now().UTC()),
		})
	case billing.AllowanceInsufficientCredits, billing.AllowanceFeatureLocked, billing.AllowanceGuestNotAllowed:
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":                 allowance.Reason,
			"required_credits":      allowance.RequiredCredits,
			"available_credits":     allowance.AvailableCredits,
			"daily_limit_remaining": allowance.DailyRemaining,
			"upgrade_required":      allowance.Reason != billing.AllowanceDailyLimitReached,
		})
	default:
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":                 allowance.Reason,
			"required_credits":      allowance.RequiredCredits,
			"available_credits":     allowance.AvailableCredits,
			"daily_limit_remaining": allowance.DailyRemaining,
			"upgrade_required":      true,
		})
	}
}

func nextCreditResetAt(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}

func optionalInt64(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

// tokenPointers turns what a provider reported about a call into the optional
// fields the usage event stores.
//
// They are pointers because on this event nil means "the provider did not tell
// us" and zero means "it told us, and the answer was none". Writing zeros for
// an unreported call would price it at nothing and quietly understate real
// spend, so a Usage that reports nothing yields three nils and the columns
// stay null.
func tokenPointers(reported ai.Usage) (prompt, completion, total *int) {
	if !reported.Reported() {
		return nil, nil, nil
	}
	p, c, t := reported.PromptTokens, reported.CompletionTokens, reported.TotalTokens
	return &p, &c, &t
}

func (s *Server) saveEntry(c *gin.Context) {
	val, exists := c.Get("userID")
	if !exists {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(uint)

	var input entryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	if input.Split != nil && !s.ensureEntitlement(c, billing.FeatureSplitLedger) {
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": fields})
		return
	}
	if input.AccountID != nil {
		var account models.Account
		if err := database.DB.Where("user_id = ? AND id = ?", userID, *input.AccountID).First(&account).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"account_id": "must belong to the current user"}})
			} else {
				c.JSON(500, gin.H{"error": "account_lookup_failed"})
			}
			return
		}
		resolvedMode, ok := resolveEntryMode(input.Mode, account.Type)
		if !ok {
			message := modeMessage()
			if strings.TrimSpace(input.Mode) == "" {
				message = "is required when account type is other"
			}
			c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"mode": message}})
			return
		}
		input.Mode = resolvedMode
	}
	if fields, err := validateEntrySplitReferences(userID, input.Split); err != nil {
		c.JSON(500, gin.H{"error": "split_lookup_failed"})
		return
	} else if len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": fields})
		return
	}

	idempotencyKey, idempotencyFields := parseIdempotencyKey(c.GetHeader("Idempotency-Key"))
	if len(idempotencyFields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": idempotencyFields})
		return
	}
	if idempotencyKey != "" {
		var existing models.Entry
		if err := database.DB.Preload("Account").
			Where("user_id = ? AND idempotency_key = ?", userID, idempotencyKey).
			First(&existing).Error; err == nil {
			c.Header("Idempotency-Replayed", "true")
			c.JSON(200, existing)
			return
		} else if err != gorm.ErrRecordNotFound {
			c.JSON(500, gin.H{"error": "idempotency_lookup_failed"})
			return
		}
	}

	entry := input.toModel(userID)
	if idempotencyKey != "" {
		entry.IdempotencyKey = &idempotencyKey
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		return createEntrySplitBill(tx, userID, entry, input.Split)
	}); err != nil {
		if idempotencyKey != "" {
			var existing models.Entry
			if lookupErr := database.DB.Preload("Account").
				Where("user_id = ? AND idempotency_key = ?", userID, idempotencyKey).
				First(&existing).Error; lookupErr == nil {
				c.Header("Idempotency-Replayed", "true")
				c.JSON(200, existing)
				return
			}
		}
		c.JSON(500, gin.H{"error": "failed_create_entry"})
		return
	}
	_ = database.DB.Preload("Account").First(&entry, entry.ID).Error
	// No notification here: the client already confirms its own save inline. Only
	// events the user could not otherwise know about belong in the inbox.
	_ = maybeCreateBudgetAlertsForEntry(entry)

	c.JSON(201, entry)
}

func (s *Server) listEntries(c *gin.Context) {
	val, exists := c.Get("userID")
	if !exists {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(uint)

	page, pageSize, fields := parseEntryPagination(c.Query("page"), c.Query("page_size"))
	if len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_filters", "fields": fields})
		return
	}

	query, filterFields := filteredEntriesQuery(userID, c)
	if len(filterFields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_filters", "fields": filterFields})
		return
	}

	sortOrder, sortFields := parseEntrySort(c.Query("sort"))
	if len(sortFields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_filters", "fields": sortFields})
		return
	}

	var entries []models.Entry
	listQuery := query.Session(&gorm.Session{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_count_entries"})
		return
	}

	if err := listQuery.Preload("Account").
		Order(sortOrder).
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&entries).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_entries"})
		return
	}

	categoryCounts, err := entryCategoryCounts(userID, c)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_count_entries"})
		return
	}

	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	c.JSON(200, gin.H{
		"entries": entries, "page": page, "page_size": pageSize,
		"total": total, "total_pages": totalPages,
		"category_counts": categoryCounts,
	})
}

// entryCategoryCounts answers "how many would I get if I picked this category",
// so it counts against every active filter except the category one.
//
// Canonical names are the keys. Legacy rows are folded onto their canonical
// name here rather than reported separately, because the filter chips are the
// canonical list and a "Food: 12" the UI cannot render is a count nobody sees.
func entryCategoryCounts(userID uint, c *gin.Context) (map[string]int64, error) {
	query, fields := filteredEntriesQueryOmitting(userID, c, true)
	if len(fields) > 0 {
		return map[string]int64{}, nil
	}

	var rows []struct {
		Category string
		Total    int64
	}
	if err := query.
		Select("entries.category as category, count(*) as total").
		Group("entries.category").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		name := row.Category
		if resolved, ok := canonicalCategory(name); ok {
			name = resolved
		}
		if strings.TrimSpace(name) == "" {
			name = defaultCategory
		}
		counts[name] += row.Total
	}
	return counts, nil
}

func (s *Server) getEntry(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var entry models.Entry
	if err := ownedEntries(database.DB, userID).Preload("Account").Where("id = ?", id).First(&entry).Error; err != nil {
		c.JSON(404, gin.H{"error": "entry not found"})
		return
	}

	c.JSON(200, entry)
}

func (s *Server) updateEntry(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var entry models.Entry
	if err := ownedEntries(database.DB, userID).Where("id = ?", id).First(&entry).Error; err != nil {
		c.JSON(404, gin.H{"error": "entry not found"})
		return
	}

	var input updateEntryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}

	amount, title, entryType := entry.Amount, entry.Title, entry.Type
	currency, source, mode, category, date := entry.Currency, entry.Source, entry.Mode, entry.Category, entry.Date
	if input.Amount != nil {
		amount = *input.Amount
	}
	if input.Type != nil {
		entryType = *input.Type
	}
	if input.Title != nil {
		title = *input.Title
	}
	if input.Currency != nil {
		currency = *input.Currency
	}
	if input.Source != nil {
		source = *input.Source
	}
	if input.Mode != nil {
		mode = *input.Mode
	}
	if input.Category != nil {
		category = *input.Category
	}
	if input.Date != nil {
		date = *input.Date
	}
	if input.AccountID.Set && input.AccountID.Value != nil && *input.AccountID.Value == 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"account_id": "must be a positive integer"}})
		return
	}
	if input.AccountID.Set && input.AccountID.Value != nil {
		var account models.Account
		if err := database.DB.Where("user_id = ? AND id = ?", userID, *input.AccountID.Value).First(&account).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"account_id": "must belong to the current user"}})
			} else {
				c.JSON(500, gin.H{"error": "account_lookup_failed"})
			}
			return
		}
		if input.Mode != nil {
			resolvedMode, ok := resolveEntryMode(*input.Mode, account.Type)
			if !ok {
				c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"mode": modeMessage()}})
				return
			}
			mode = resolvedMode
		} else if resolvedMode, ok := paymentModeForAccountType(account.Type); ok {
			// Re-derive even when the account is unchanged. This repairs a legacy
			// bank/debit entry's false Credit Card mode on its next explicit edit.
			mode = resolvedMode
		} else if entry.AccountID == nil || *entry.AccountID != account.ID {
			c.JSON(422, gin.H{"error": "invalid_entry", "fields": gin.H{"mode": "is required when account type is other"}})
			return
		}
	}
	if fields := validateEntryValues(amount, title, entryType, currency, source, mode, category, date); len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_entry", "fields": fields})
		return
	}
	if input.Split.Set {
		if input.Split.Value != nil && !s.ensureEntitlement(c, billing.FeatureSplitLedger) {
			return
		}
		splitFields := map[string]string{}
		for field, message := range input.Split.Value.validate() {
			splitFields[field] = message
		}
		if input.Split.Value != nil {
			if !strings.EqualFold(entryType, "expense") {
				splitFields["split"] = "can be added only to expenses"
			}
			totalShares := models.Money(0)
			for _, participant := range input.Split.Value.Participants {
				totalShares += participant.ShareAmount
			}
			if totalShares > amount {
				splitFields["split.participants"] = "shares must not exceed transaction amount"
			}
		}
		if len(splitFields) > 0 {
			c.JSON(422, gin.H{"error": "invalid_entry", "fields": splitFields})
			return
		}
		if fields, err := validateEntrySplitReferences(userID, input.Split.Value); err != nil {
			c.JSON(500, gin.H{"error": "split_lookup_failed"})
			return
		} else if len(fields) > 0 {
			c.JSON(422, gin.H{"error": "invalid_entry", "fields": fields})
			return
		}
	}
	if input.Title != nil {
		entry.Title = *input.Title
	}
	if input.Amount != nil {
		entry.Amount = *input.Amount
	}
	if input.Type != nil {
		entry.Type = strings.ToLower(*input.Type)
	}
	if input.Currency != nil {
		entry.Currency = strings.ToUpper(strings.TrimSpace(*input.Currency))
	}
	if input.Source != nil {
		entry.Source = strings.ToLower(strings.TrimSpace(*input.Source))
	}
	entry.Mode = mode
	if input.CardNetwork != nil {
		entry.CardNetwork = *input.CardNetwork
	}
	if input.Category != nil {
		entry.Category = *input.Category
		if resolved, ok := categoryForSave(entry.Category, entry.Type); ok {
			entry.Category = resolved
		}
	}
	if input.Notes != nil {
		entry.Notes = *input.Notes
	}
	if input.Merchant != nil {
		entry.Merchant = *input.Merchant
	}
	if input.PurposeType != nil {
		entry.PurposeType = *input.PurposeType
	}
	if input.Date != nil {
		entry.Date = *input.Date
	}
	if input.Time != nil {
		entry.Time = *input.Time
	}
	if input.Tag != nil {
		entry.Tag = *input.Tag
	}
	if input.Tags != nil {
		entry.Tags = *input.Tags
	}
	if input.SourceText != nil {
		entry.SourceText = *input.SourceText
	}
	// Hold the previous receipt so it can be removed once the swap is committed.
	replacedAttachment := ""
	if input.Attachment != nil {
		if entry.Attachment != "" && entry.Attachment != *input.Attachment {
			replacedAttachment = entry.Attachment
		}
		entry.Attachment = *input.Attachment
	}
	if input.AccountID.Set {
		entry.AccountID = input.AccountID.Value
	}

	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&entry).Error; err != nil {
			return err
		}
		if input.Split.Set {
			return replaceEntrySplitBill(tx, userID, entry, input.Split.Value)
		}
		return nil
	}); err != nil {
		c.JSON(500, gin.H{"error": "failed_update_entry"})
		return
	}
	if replacedAttachment != "" {
		deleteLocalUploadFiles([]string{replacedAttachment})
	}
	_ = database.DB.Preload("Account").First(&entry, entry.ID).Error
	// See createEntry: the user performed this edit and saw it succeed.
	_ = maybeCreateBudgetAlertsForEntry(entry)

	c.JSON(200, entry)
}

func userOwnsAccount(userID, accountID uint) (bool, error) {
	var count int64
	err := database.DB.Model(&models.Account{}).
		Where("id = ? AND user_id = ?", accountID, userID).
		Count(&count).Error
	return count == 1, err
}

func ownedEntries(db *gorm.DB, userID uint) *gorm.DB {
	return db.Where("user_id = ?", userID)
}

func (s *Server) deleteEntry(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var entry models.Entry
	if err := ownedEntries(database.DB, userID).Where("id = ?", id).First(&entry).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(404, gin.H{"error": "entry not found"})
			return
		}
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := deleteEntrySplitBills(tx, userID, uint(id)); err != nil {
			return err
		}
		result := ownedEntries(tx, userID).Where("id = ?", id).Delete(&models.Entry{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	}); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(404, gin.H{"error": "entry not found"})
			return
		}
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// The row is gone, so nothing else can reference the receipt now.
	deleteLocalUploadFiles([]string{entry.Attachment})
	// See createEntry: the user performed this delete and saw it succeed.

	c.JSON(200, gin.H{"message": "entry deleted"})
}

func (s *Server) listQuickPrompts(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var prompts []models.QuickPrompt
	if err := database.DB.Where("user_id = ?", userID).Find(&prompts).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	// Seed default prompts if none exist
	if len(prompts) == 0 {
		defaults := []models.QuickPrompt{
			{UserID: userID, Title: "Morning Coffee", Amount: 150, Mode: "Cash", Category: "Food & Drinks", Icon: "coffee-outline"},
			{UserID: userID, Title: "Metro Recharge", Amount: 500, Mode: "UPI", Category: "Travel", Icon: "train"},
			{UserID: userID, Title: "Car Fuel", Amount: 3000, Mode: "Credit Card", Category: "Transport", Icon: "gas-station-outline"},
		}
		for _, p := range defaults {
			database.DB.Create(&p)
		}
		// Fetch again to get IDs and updated list
		database.DB.Where("user_id = ?", userID).Find(&prompts)
	}

	c.JSON(200, prompts)
}

func (s *Server) saveQuickPrompt(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var prompt models.QuickPrompt
	if err := c.BindJSON(&prompt); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	prompt.UserID = userID
	if err := database.DB.Create(&prompt).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(201, prompt)
}

func (s *Server) updateQuickPrompt(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var prompt models.QuickPrompt
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&prompt).Error; err != nil {
		c.JSON(404, gin.H{"error": "prompt not found"})
		return
	}

	if err := c.BindJSON(&prompt); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	prompt.ID = uint(id)
	prompt.UserID = userID
	if err := database.DB.Save(&prompt).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, prompt)
}

func (s *Server) deleteQuickPrompt(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&models.QuickPrompt{}).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"message": "prompt deleted"})
}

func (s *Server) updateProfile(c *gin.Context) {
	val, exists := c.Get("userID")
	if !exists {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(uint)

	var payload struct {
		Username   string `json:"username"`
		Email      string `json:"email"`
		Phone      string `json:"phone"`
		ClaimToken string `json:"claim_token"`
	}

	if err := c.BindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	if strings.TrimSpace(payload.Username) == "" {
		c.JSON(400, gin.H{"error": "Username cannot be empty"})
		return
	}

	// 1. Check Username Uniqueness
	var existingUser models.User
	if err := database.DB.Where("username = ? AND id != ?", payload.Username, userID).First(&existingUser).Error; err == nil {
		c.JSON(409, gin.H{"error": "Username is already taken"})
		return
	}

	var user models.User
	if err := database.DB.First(&user, userID).Error; err != nil {
		c.JSON(404, gin.H{"error": "User not found"})
		return
	}

	emailChanged := payload.Email != "" && (user.Email == nil || *user.Email != payload.Email)
	phoneChanged := payload.Phone != "" && (user.Phone == nil || *user.Phone != payload.Phone)
	if emailChanged && phoneChanged {
		c.JSON(403, gin.H{"error": "Only one verified contact field can be updated at a time."})
		return
	}

	if emailChanged || phoneChanged {
		claim, err := consumeClaimToken(payload.ClaimToken)
		if err != nil {
			c.JSON(403, gin.H{"error": "Contact verification required. Please verify OTP."})
			return
		}
		if emailChanged {
			_, normalizedEmail, err := normalizeIdentifier(payload.Email)
			if err != nil || claim.IdentifierType != "email" || claim.Identifier != normalizedEmail {
				c.JSON(403, gin.H{"error": "Email verification required. Please verify OTP."})
				return
			}
			user.Email = &normalizedEmail
		}
		if phoneChanged {
			_, normalizedPhone, err := normalizeIdentifier(payload.Phone)
			if err != nil || claim.IdentifierType != "phone" || claim.Identifier != normalizedPhone {
				c.JSON(403, gin.H{"error": "Phone verification required. Please verify OTP."})
				return
			}
			user.Phone = &normalizedPhone
		}
	} else if payload.Email == "" {
		// Optional: Allow clearing email? Or just ignore if empty?
		// For now assuming empty string in payload means 'no change' or 'clear' managed by FE logic.
		// If explicit clear is needed, logic might differ. Assuming update sends current value if unchanged.
	}

	user.Username = payload.Username

	if err := database.DB.Save(&user).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to update profile."})
		return
	}
	if err := reconcileSplitIdentities(database.DB, user.ID); err != nil {
		c.JSON(500, gin.H{"error": "Failed to reconcile split identities."})
		return
	}

	// Re-serialize user to ensure clean JSON response
	c.JSON(200, gin.H{"user": user})
}

func (s *Server) deleteUser(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	user := models.User{ID: userID}
	if value, exists := c.Get("user"); exists {
		if currentUser, ok := value.(*models.User); ok && currentUser != nil {
			user = *currentUser
		}
	}
	if user.ID == 0 {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	if user.Email == nil && user.Phone == nil {
		if err := database.DB.First(&user, userID).Error; err != nil {
			c.JSON(404, gin.H{"error": "user_not_found"})
			return
		}
	}

	attachments, err := deleteUserData(database.DB, user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_delete_user"})
		return
	}
	deleteLocalUploadFiles(attachments)
	c.JSON(200, gin.H{"message": "account deleted"})
}

func deleteUserData(db *gorm.DB, user models.User) ([]string, error) {
	if user.ID == 0 {
		return nil, errors.New("user_id_required")
	}

	var attachments []string
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Entry{}).
			Where("user_id = ? AND attachment <> ?", user.ID, "").
			Pluck("attachment", &attachments).Error; err != nil {
			return err
		}

		guestDeviceHash := ""
		if user.DeviceID != nil {
			guestDeviceHash = billing.HashUsageKey(*user.DeviceID)
		}
		if guestDeviceHash != "" {
			if err := tx.Where("guest_device_id_hash = ?", guestDeviceHash).
				Delete(&models.GuestUsageKey{}).Error; err != nil {
				return fmt.Errorf("delete guest usage keys: %w", err)
			}
		}

		billingDeletes := []struct {
			model          any
			name           string
			hasGuestDevice bool
		}{
			{model: &models.CreditLedger{}, name: "credit ledger", hasGuestDevice: true},
			{model: &models.AIUsageEvent{}, name: "ai usage events", hasGuestDevice: true},
			{model: &models.AIUsageLimitEvent{}, name: "ai usage limit events", hasGuestDevice: true},
			{model: &models.DailyCreditUsage{}, name: "daily credit usage", hasGuestDevice: true},
			{model: &models.AIAbuseBlock{}, name: "ai abuse blocks", hasGuestDevice: true},
			{model: &models.LifetimeQuoteRequest{}, name: "lifetime quote requests"},
			{model: &models.CreditGrant{}, name: "credit grants", hasGuestDevice: true},
			{model: &models.UserSubscription{}, name: "user subscriptions"},
		}
		for _, item := range billingDeletes {
			query := tx.Where("user_id = ?", user.ID)
			if guestDeviceHash != "" && item.hasGuestDevice {
				query = query.Or("guest_device_id_hash = ?", guestDeviceHash)
			}
			if err := query.Delete(item.model).Error; err != nil {
				return fmt.Errorf("delete %s: %w", item.name, err)
			}
		}

		deletes := []struct {
			model any
			name  string
		}{
			{&models.SplitSettlement{}, "split settlements"},
			{&models.SplitParticipant{}, "split participants"},
			{&models.SplitBill{}, "split bills"},
			{&models.SplitGroupUserMember{}, "split group user members"},
			{&models.SplitGroupMember{}, "split group members"},
			{&models.SplitGroupDirectInvite{}, "split group direct invites"},
			{&models.SplitGroupInvite{}, "split group invites"},
			{&models.SplitGroup{}, "split groups"},
			{&models.SplitFriend{}, "split friends"},
			{&models.BudgetAlert{}, "budget alerts"},
			{&models.Budget{}, "budgets"},
			{&models.SubscriptionReminder{}, "subscription reminders"},
			{&models.SubscriptionOccurrence{}, "subscription occurrences"},
			{&models.Subscription{}, "subscriptions"},
			{&models.RecurringCandidateDecision{}, "recurring candidate decisions"},
			{&models.PushDevice{}, "push devices"},
			{&models.Notification{}, "notifications"},
			{&models.QuickPrompt{}, "quick prompts"},
			{&models.Entry{}, "entries"},
			{&models.Account{}, "accounts"},
			{&models.AuthSession{}, "auth sessions"},
		}
		for _, item := range deletes {
			if err := tx.Where("user_id = ?", user.ID).Delete(item.model).Error; err != nil {
				return fmt.Errorf("delete %s: %w", item.name, err)
			}
		}

		for _, identifier := range verificationIdentifiersForUser(user) {
			if err := tx.Where("identifier_type = ? AND identifier = ?", identifier.identifierType, identifier.identifier).
				Delete(&models.AuthVerification{}).Error; err != nil {
				return fmt.Errorf("delete auth verifications: %w", err)
			}
		}

		if err := tx.Where("id = ?", user.ID).Delete(&models.User{}).Error; err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		return nil
	})
	return attachments, err
}

type verificationIdentifier struct {
	identifierType string
	identifier     string
}

func verificationIdentifiersForUser(user models.User) []verificationIdentifier {
	identifiers := make([]verificationIdentifier, 0, 2)
	if user.Email != nil {
		email := strings.TrimSpace(*user.Email)
		if email != "" {
			identifiers = append(identifiers, verificationIdentifier{identifierType: "email", identifier: strings.ToLower(email)})
		}
	}
	if user.Phone != nil {
		phone := strings.TrimSpace(*user.Phone)
		if phone != "" {
			identifiers = append(identifiers, verificationIdentifier{identifierType: "phone", identifier: phone})
		}
	}
	return identifiers
}

func localUploadPathFromAttachment(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Path != "" {
		value = parsed.Path
	}

	cleaned := filepath.Clean(strings.TrimPrefix(value, "/"))
	relative, err := filepath.Rel("uploads", cleaned)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return "", false
	}
	return filepath.Join("uploads", relative), true
}

func deleteLocalUploadFiles(attachments []string) {
	seen := make(map[string]struct{}, len(attachments))
	for _, attachment := range attachments {
		path, ok := localUploadPathFromAttachment(attachment)
		if !ok {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("failed_delete_upload file=%s err=%v", filepath.Base(path), err)
		}
	}
}

func cors(cfg *config.Config) gin.HandlerFunc {
	allowedOrigins := parseAllowedOrigins(cfg.AllowOrigins)
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin != "" {
			if _, ok := allowedOrigins[origin]; ok {
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Vary", "Origin")
				c.Writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			} else if c.Request.Method == http.MethodOptions {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cors_origin_not_allowed"})
				return
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func parseAllowedOrigins(raw string) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, origin := range strings.Split(raw, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" || origin == "*" {
			continue
		}
		allowed[origin] = struct{}{}
	}
	return allowed
}

func logging() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := timepkg.Now()
		c.Next()
		log.Printf("%s %s %d %s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), timepkg.Since(start))
	}
}

func loadLocationOrIndia(requested, fallback string) *timepkg.Location {
	if strings.TrimSpace(requested) == "" {
		requested = fallback
	}
	if strings.TrimSpace(requested) == "" {
		requested = "Asia/Kolkata"
	}
	loc, err := timepkg.LoadLocation(requested)
	if err == nil {
		return loc
	}
	loc, err = timepkg.LoadLocation("Asia/Kolkata")
	if err == nil {
		return loc
	}
	return timepkg.FixedZone("IST", 5*3600+1800)
}

// publicOrigin resolves the scheme and host the client reached us on. A
// TLS-terminating proxy leaves Request.TLS nil, so relying on it alone would
// hand back http:// URLs that Android blocks as cleartext — and those URLs are
// persisted on the entry, so a wrong one stays wrong.
func publicOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := firstForwardedValue(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}

	host := r.Host
	if forwarded := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}

	return scheme + "://" + host
}

// firstForwardedValue takes the left-most entry of a comma-separated forwarding
// header, which is the value the original client was served.
func firstForwardedValue(header string) string {
	value, _, _ := strings.Cut(header, ",")
	return strings.TrimSpace(value)
}
func (s *Server) saveAccount(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input accountInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_account", "fields": fields})
		return
	}
	account := models.Account{UserID: userID}
	input.apply(&account)
	var accountCount int64
	if err := database.DB.Model(&models.Account{}).Where("user_id = ?", userID).Count(&accountCount).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_count_accounts"})
		return
	}
	if accountCount == 0 {
		account.IsDefault = true
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if account.IsDefault {
			if err := tx.Model(&models.Account{}).Where("user_id = ?", userID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Create(&account).Error
	}); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	hydrateAccountProvider(&account)
	if account.AutoCreated {
		_ = createNotification(
			userID,
			"account.needs_setup",
			"Finish setting up "+account.Name,
			"Add the missing account details so balances, bills, and matching stay accurate.",
			fmt.Sprintf("/accounts/%d", account.ID),
		)
	}
	c.JSON(201, account)
}

func (s *Server) listAccounts(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var accounts []models.Account
	if err := database.DB.Where("user_id = ?", userID).Find(&accounts).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	for index := range accounts {
		hydrateAccountProvider(&accounts[index])
	}

	// Each account arrives with what its transactions prove, not just what the
	// user typed into it once. See account_summary.go for the rules.
	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
	monthStart, monthEnd := monthToDateWindow(timepkg.Now().In(location))
	totals, err := loadAccountLedgerTotals(userID, monthStart, monthEnd)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_load_account_activity"})
		return
	}
	// Credit cards prefer their latest bill over the ledger's own arithmetic.
	statements, err := loadCardStatementContexts(userID, creditCardIDs(accounts))
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_load_card_statements"})
		return
	}
	today := truncateDate(timepkg.Now().In(location)).Format(apiDateLayout)
	c.JSON(200, summariseAccounts(accounts, totals, statements, today))
}

func (s *Server) updateAccount(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	var account models.Account
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&account).Error; err != nil {
		c.JSON(404, gin.H{"error": "account not found"})
		return
	}
	wasDefault := account.IsDefault
	var input accountInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(422, gin.H{"error": "invalid_account", "fields": fields})
		return
	}
	input.apply(&account)
	// Reaching the edit handler means the user intentionally reviewed the
	// account, even when they kept some optional fields empty.
	account.AutoCreated = false
	if wasDefault {
		account.IsDefault = true
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if account.IsDefault {
			if err := tx.Model(&models.Account{}).
				Where("user_id = ? AND id <> ?", userID, id).
				Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Save(&account).Error
	}); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	hydrateAccountProvider(&account)
	c.JSON(200, account)
}

// markCardPaidOff resets only the ledger fallback used before a card has a
// priced statement. Once a statement exists, payment rows remain authoritative.
func (s *Server) markCardPaidOff(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	var account models.Account
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&account).Error; err != nil {
		c.JSON(404, gin.H{"error": "account_not_found"})
		return
	}
	if normalizeAccountType(account.Type) != "credit_card" {
		c.JSON(422, gin.H{"error": "not_credit_card"})
		return
	}
	var pricedStatements int64
	if err := database.DB.Model(&models.CardStatement{}).
		Where("user_id = ? AND account_id = ? AND status <> ?", userID, account.ID, statementStatusDraft).
		Count(&pricedStatements).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_check_card_statements"})
		return
	}
	if pricedStatements > 0 {
		c.JSON(409, gin.H{
			"error":   "statement_managed_card",
			"message": "Record the payment on the card statement instead.",
		})
		return
	}
	now := timepkg.Now().UTC()
	var lastEntryID uint
	if err := database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND account_id = ?", userID, account.ID).
		Select("COALESCE(MAX(id), 0)").Scan(&lastEntryID).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_mark_card_paid_off"})
		return
	}
	if err := database.DB.Model(&account).Updates(map[string]any{
		"card_ledger_reset_at": now, "card_ledger_reset_entry_id": lastEntryID,
	}).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_mark_card_paid_off"})
		return
	}
	account.CardLedgerResetAt = &now
	account.CardLedgerResetEntryID = &lastEntryID
	c.JSON(200, account)
}

func (s *Server) deleteAccount(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	var account models.Account
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&account).Error; err != nil {
		c.JSON(404, gin.H{"error": "account not found"})
		return
	}
	var entryCount int64
	if err := database.DB.Model(&models.Entry{}).Where("account_id = ? AND user_id = ?", id, userID).Count(&entryCount).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_check_account_usage"})
		return
	}
	if entryCount > 0 {
		c.JSON(409, gin.H{"error": "account_in_use", "message": "Move or delete linked transactions before deleting this account."})
		return
	}
	var accountCount int64
	if err := database.DB.Model(&models.Account{}).Where("user_id = ?", userID).Count(&accountCount).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_count_accounts"})
		return
	}
	if accountCount <= 1 {
		c.JSON(409, gin.H{"error": "last_account", "message": "Create another account before deleting your only account."})
		return
	}
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&account).Error; err != nil {
			return err
		}
		if account.IsDefault {
			var replacement models.Account
			if err := tx.Where("user_id = ?", userID).Order("created_at asc").First(&replacement).Error; err != nil {
				return err
			}
			return tx.Model(&replacement).Update("is_default", true).Error
		}
		return nil
	}); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "account deleted"})
}
