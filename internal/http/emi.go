package http

import (
	"math"
	"net/http"
	"strings"

	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

const (
	maxEMITenureMonths      = 360
	maxEMIAnnualRatePercent = 100
)

type emiCalculationInput struct {
	PrincipalAmount           models.Money `json:"principal_amount"`
	AnnualInterestRatePercent float64      `json:"annual_interest_rate_percent"`
	TenureMonths              int          `json:"tenure_months"`
	Currency                  string       `json:"currency"`
}

type emiCalculationResponse struct {
	PrincipalAmount           models.Money       `json:"principal_amount"`
	Currency                  string             `json:"currency"`
	AnnualInterestRatePercent float64            `json:"annual_interest_rate_percent"`
	TenureMonths              int                `json:"tenure_months"`
	MonthlyEMI                models.Money       `json:"monthly_emi"`
	TotalPayment              models.Money       `json:"total_payment"`
	TotalInterest             models.Money       `json:"total_interest"`
	Schedule                  []emiScheduleMonth `json:"schedule"`
}

type emiScheduleMonth struct {
	Month           int          `json:"month"`
	OpeningBalance  models.Money `json:"opening_balance"`
	PaymentAmount   models.Money `json:"payment_amount"`
	PrincipalAmount models.Money `json:"principal_amount"`
	InterestAmount  models.Money `json:"interest_amount"`
	ClosingBalance  models.Money `json:"closing_balance"`
}

func (s *Server) calculateEMI(c *gin.Context) {
	var input emiCalculationInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_emi_calculation", "fields": fields})
		return
	}

	c.JSON(http.StatusOK, buildEMICalculation(input))
}

func (input emiCalculationInput) validate() map[string]string {
	fields := map[string]string{}
	if !input.PrincipalAmount.IsPositive() {
		fields["principal_amount"] = "must be positive"
	}
	if math.IsNaN(input.AnnualInterestRatePercent) || math.IsInf(input.AnnualInterestRatePercent, 0) {
		fields["annual_interest_rate_percent"] = "must be a finite number"
	} else if input.AnnualInterestRatePercent < 0 {
		fields["annual_interest_rate_percent"] = "must be zero or positive"
	} else if input.AnnualInterestRatePercent > maxEMIAnnualRatePercent {
		fields["annual_interest_rate_percent"] = "must not exceed 100"
	}
	if input.TenureMonths < 1 {
		fields["tenure_months"] = "must be at least 1"
	} else if input.TenureMonths > maxEMITenureMonths {
		fields["tenure_months"] = "must not exceed 360"
	}
	if normalizedEMICurrency(input.Currency) != "INR" {
		fields["currency"] = "must be INR"
	}
	return fields
}

func buildEMICalculation(input emiCalculationInput) emiCalculationResponse {
	principal := input.PrincipalAmount
	monthlyRate := input.AnnualInterestRatePercent / 12 / 100
	monthlyEMI := calculateMonthlyEMI(principal, monthlyRate, input.TenureMonths)

	schedule := buildEMISchedule(principal, monthlyRate, input.TenureMonths, monthlyEMI)
	totalPayment := models.Money(0)
	totalInterest := models.Money(0)
	for _, month := range schedule {
		totalPayment += month.PaymentAmount
		totalInterest += month.InterestAmount
	}

	return emiCalculationResponse{
		PrincipalAmount:           principal,
		Currency:                  normalizedEMICurrency(input.Currency),
		AnnualInterestRatePercent: input.AnnualInterestRatePercent,
		TenureMonths:              input.TenureMonths,
		MonthlyEMI:                monthlyEMI,
		TotalPayment:              totalPayment,
		TotalInterest:             totalInterest,
		Schedule:                  schedule,
	}
}

func calculateMonthlyEMI(principal models.Money, monthlyRate float64, tenureMonths int) models.Money {
	if monthlyRate == 0 {
		return roundToMoney(float64(principal) / float64(tenureMonths))
	}
	factor := math.Pow(1+monthlyRate, float64(tenureMonths))
	return roundToMoney(float64(principal) * monthlyRate * factor / (factor - 1))
}

func buildEMISchedule(principal models.Money, monthlyRate float64, tenureMonths int, monthlyEMI models.Money) []emiScheduleMonth {
	schedule := make([]emiScheduleMonth, 0, tenureMonths)
	balance := principal
	for month := 1; month <= tenureMonths; month++ {
		opening := balance
		interest := roundToMoney(float64(opening) * monthlyRate)
		principalPayment := monthlyEMI - interest
		if principalPayment <= 0 || principalPayment > balance || month == tenureMonths {
			principalPayment = balance
		}
		payment := principalPayment + interest
		balance -= principalPayment
		if balance < 0 {
			balance = 0
		}
		schedule = append(schedule, emiScheduleMonth{
			Month:           month,
			OpeningBalance:  opening,
			PaymentAmount:   payment,
			PrincipalAmount: principalPayment,
			InterestAmount:  interest,
			ClosingBalance:  balance,
		})
		if balance == 0 {
			break
		}
	}
	return schedule
}

func normalizedEMICurrency(currency string) string {
	if strings.TrimSpace(currency) == "" {
		return "INR"
	}
	return strings.ToUpper(strings.TrimSpace(currency))
}

func roundToMoney(amount float64) models.Money {
	return models.Money(math.Round(amount))
}
