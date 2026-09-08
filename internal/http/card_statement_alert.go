package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finance-parser-go/internal/ai"
	"finance-parser-go/internal/billing"
	"finance-parser-go/internal/database"
	"finance-parser-go/internal/models"
)

const maxStatementAlertChars = 4000

type statementAlertInput struct {
	Text       string `json:"text"`
	Channel    string `json:"channel"`
	ReceivedAt string `json:"received_at"`
}

type parsedStatementAlert struct {
	StatementDate string       `json:"statement_date"`
	DueDate       string       `json:"due_date"`
	TotalDue      models.Money `json:"total_due"`
	MinimumDue    models.Money `json:"minimum_due"`
}

// importCardStatementAlert accepts alert text from any consented SMS/email
// channel without coupling the ledger to that channel. The unique statement
// key makes retries safe, and an existing manual statement always wins.
func (s *Server) importCardStatementAlert(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	account, err := loadUserCard(userID, accountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}
	var input statementAlertInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	input.Text = strings.TrimSpace(input.Text)
	input.Channel = strings.ToLower(strings.TrimSpace(input.Channel))
	if input.Text == "" || utf8.RuneCountInString(input.Text) > maxStatementAlertChars {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_alert", "fields": gin.H{"text": "is required and must be at most 4000 characters"}})
		return
	}
	if input.Channel != "sms" && input.Channel != "email" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_alert", "fields": gin.H{"channel": "must be sms or email"}})
		return
	}
	receivedAt := input.ReceivedAt
	if receivedAt == "" {
		receivedAt = truncateDate(time.Now()).Format(apiDateLayout)
	} else if _, err := parseStrictAPIDate(receivedAt); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement_alert", "fields": gin.H{"received_at": "must be a YYYY-MM-DD date"}})
		return
	}

	if s.rejectIfAIParseDisabled(c) || s.rejectIfProviderCircuitOpen(c) {
		return
	}
	parser, ok := s.parser.(ai.StatementAlertParser)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "statement_alert_parser_unavailable"})
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
	action, _ := ai.DefaultActionRegistry().RequireImplemented(ai.ActionFutureAIStatementImport)
	creditService := billing.NewCreditService(database.DB)
	usage, allowance, reserveErr := creditService.ReserveCredits(subject, action.Code, parseIdempotencyHeader(c))
	if reserveErr != nil {
		writeCreditReservationError(c, allowance, reserveErr)
		return
	}

	timeout := s.cfg.ReqTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	raw, alertUsage, parseErr := parser.ParseStatementAlert(ctx, input.Text, receivedAt)
	if parseErr != nil {
		s.recordAIProviderFailure()
		log.Printf("statement alert parse failed: %v", parseErr)
		_, _ = creditService.FinalizeUsage(usage.ID, billing.ProviderUsage{Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "statement_alert_parse_failed"})
		c.JSON(http.StatusBadGateway, gin.H{"error": "statement_alert_parse_failed"})
		return
	}
	parsed, parseErr := decodeStatementAlert(raw, input, receivedAt)
	if parseErr != nil {
		responseBytes := len(raw)
		_, _ = creditService.FinalizeUsage(usage.ID, withStatementTokens(billing.ProviderUsage{Status: billing.UsageStatusFailedAfterProvider, ErrorCode: "invalid_statement_alert_response", ResponseBytes: &responseBytes}, alertUsage, s.cfg.OpenAILlmModel))
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "statement_alert_not_recognized"})
		return
	}
	responseBytes := len(raw)
	credits, err := s.finalizeParseSuccess(creditService, usage.ID, subject, action.Code, utf8.RuneCountInString(input.Text), responseBytes, 0, alertUsage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_finalization_failed"})
		return
	}

	statementDate, _ := parseStrictAPIDate(parsed.StatementDate)
	if parsed.DueDate == "" {
		parsed.DueDate = dueDateFor(statementDate, effectiveDueDay(account, statementDate)).Format(apiDateLayout)
	}
	cycleStart, cycleEnd := statementCycle(statementDate, account.StatementDay)
	var statement models.CardStatement
	dbErr := database.DB.Where("user_id = ? AND account_id = ? AND statement_date = ?", userID, accountID, parsed.StatementDate).First(&statement).Error
	created := dbErr == gorm.ErrRecordNotFound
	if dbErr != nil && dbErr != gorm.ErrRecordNotFound {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_lookup_statement"})
		return
	}
	manualPreserved := !created && statement.Source == "manual" && statement.Status != statementStatusDraft
	if !manualPreserved {
		statement.UserID = userID
		statement.AccountID = accountID
		statement.StatementDate = parsed.StatementDate
		statement.CycleStart = cycleStart.Format(apiDateLayout)
		statement.CycleEnd = cycleEnd.Format(apiDateLayout)
		statement.DueDate = parsed.DueDate
		statement.TotalDue = parsed.TotalDue
		statement.MinimumDue = parsed.MinimumDue
		statement.Currency = "INR"
		statement.Source = input.Channel
		if statement.Status == "" || statement.Status == statementStatusDraft {
			statement.Status = statementStatusUnpaid
		}
		statement.Status = deriveStatementStatus(statement.Status, statement.TotalDue, statement.PaidAmount)
		if err := database.DB.Save(&statement).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
			return
		}
	}

	if charged, ok := credits["credits_charged"].(int); ok {
		c.Header("X-AI-Credits-Charged", fmt.Sprint(charged))
	}
	if manualPreserved {
		c.Header("X-Finnri-Manual-Preserved", "true")
	}
	respondWithStatement(c, &statement, created)
}

func decodeStatementAlert(raw []byte, input statementAlertInput, receivedAt string) (parsedStatementAlert, error) {
	trimmed := strings.TrimSpace(string(raw))
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	var parsed parsedStatementAlert
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &parsed); err != nil {
		return parsed, err
	}
	if parsed.StatementDate == "" {
		parsed.StatementDate = receivedAt
	}
	statement := cardStatementInput{
		StatementDate: parsed.StatementDate, DueDate: parsed.DueDate,
		TotalDue: parsed.TotalDue, MinimumDue: parsed.MinimumDue,
		Currency: "INR", Source: input.Channel,
	}
	if fields := statement.validate(); len(fields) > 0 || parsed.TotalDue <= 0 {
		return parsed, fmt.Errorf("invalid parsed statement: %v", fields)
	}
	if !statementAlertContainsAmount(input.Text, parsed.TotalDue) ||
		(parsed.MinimumDue > 0 && !statementAlertContainsAmount(input.Text, parsed.MinimumDue)) {
		return parsed, fmt.Errorf("parsed amount not present in source")
	}
	return parsed, nil
}

func statementAlertContainsAmount(text string, amount models.Money) bool {
	normalized := strings.NewReplacer(",", "", "₹", "", "Rs.", "", "Rs", "", "INR", "").Replace(text)
	decimal := amount.String()
	if strings.Contains(normalized, decimal) {
		return true
	}
	return strings.HasSuffix(decimal, ".00") && strings.Contains(normalized, strings.TrimSuffix(decimal, ".00"))
}

// withStatementTokens records what a statement parse cost when the provider
// answered but the answer could not be used. The credit is spent either way,
// and a call that produced an unreadable statement costs exactly what one that
// produced a readable one does.
func withStatementTokens(usage billing.ProviderUsage, reported ai.Usage, model string) billing.ProviderUsage {
	prompt, completion, total := tokenPointers(reported)
	if prompt == nil {
		return usage
	}
	usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens = prompt, completion, total
	if usage.Provider == "" {
		usage.Provider = "openai"
	}
	if usage.Model == "" {
		usage.Model = model
	}
	return usage
}
