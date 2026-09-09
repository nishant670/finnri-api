package http

import (
	"net/http"
	"strings"
	"testing"

	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

func TestCalculateEMIReturnsAmortizationSchedule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "emi-tools-device",
		}, http.StatusOK,
	)
	if !strings.HasPrefix(authResponse.Token, "fnr_") {
		t.Fatalf("expected opaque guest session token, got %q", authResponse.Token)
	}

	result := performJSONRequest[emiCalculationResponse](
		t, router, http.MethodPost, "/v1/tools/emi/calculate", authResponse.Token,
		map[string]any{
			"principal_amount":             "100000.00",
			"annual_interest_rate_percent": 12,
			"tenure_months":                12,
		}, http.StatusOK,
	)

	if result.Currency != "INR" || result.TenureMonths != 12 {
		t.Fatalf("unexpected EMI metadata: %#v", result)
	}
	if result.MonthlyEMI.String() != "8884.88" {
		t.Fatalf("monthly EMI = %s, want 8884.88", result.MonthlyEMI.String())
	}
	if result.TotalPayment.String() != "106618.53" || result.TotalInterest.String() != "6618.53" {
		t.Fatalf("unexpected totals: payment=%s interest=%s", result.TotalPayment.String(), result.TotalInterest.String())
	}
	if len(result.Schedule) != 12 {
		t.Fatalf("schedule months = %d, want 12", len(result.Schedule))
	}
	first := result.Schedule[0]
	if first.InterestAmount.String() != "1000.00" || first.PrincipalAmount.String() != "7884.88" {
		t.Fatalf("unexpected first month: %#v", first)
	}
	last := result.Schedule[len(result.Schedule)-1]
	if last.ClosingBalance.String() != "0.00" {
		t.Fatalf("last closing balance = %s, want 0.00", last.ClosingBalance.String())
	}
}

func mustParseMoney(t *testing.T, value string) models.Money {
	t.Helper()

	amount, err := models.ParseMoney(value)
	if err != nil {
		t.Fatalf("failed to parse money %q: %v", value, err)
	}
	return amount
}

func TestCalculateEMIHandlesZeroInterest(t *testing.T) {
	principal := mustParseMoney(t, "12000.00")
	result := buildEMICalculation(emiCalculationInput{
		PrincipalAmount:           principal,
		AnnualInterestRatePercent: 0,
		TenureMonths:              12,
		Currency:                  "inr",
	})

	if result.MonthlyEMI.String() != "1000.00" {
		t.Fatalf("monthly EMI = %s, want 1000.00", result.MonthlyEMI.String())
	}
	if result.TotalInterest.String() != "0.00" || result.TotalPayment.String() != "12000.00" {
		t.Fatalf("unexpected zero-interest totals: %#v", result)
	}
	if len(result.Schedule) != 12 || result.Schedule[11].ClosingBalance.String() != "0.00" {
		t.Fatalf("unexpected zero-interest schedule: %#v", result.Schedule)
	}
}

func TestEMICalculationInputRejectsInvalidFields(t *testing.T) {
	input := emiCalculationInput{
		AnnualInterestRatePercent: 125,
		TenureMonths:              361,
		Currency:                  "USD",
	}

	fields := input.validate()
	for _, field := range []string{"principal_amount", "annual_interest_rate_percent", "tenure_months", "currency"} {
		if fields[field] == "" {
			t.Fatalf("expected %s validation error, got %v", field, fields)
		}
	}
}

func TestStaticBearerSkipsEMIToolsRoute(t *testing.T) {
	if !skipsStaticBearer("/v1/tools/emi/calculate") {
		t.Fatal("expected EMI tools route to skip legacy static bearer auth")
	}
}
