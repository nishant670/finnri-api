package http

import (
	"net/http"
	"testing"

	"finnri/internal/models"
)

func TestTransactionSummaryReportUsesFiltersAndOwnedRows(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "report-owner-device"}, http.StatusOK,
	)
	otherAuth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "report-other-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	otherAccounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", otherAuth.Token, nil, http.StatusOK,
	)

	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Cafe lunch", "type": "expense", "amount": 250, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Cafe",
		"date": "2026-07-12",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Groceries", "type": "expense", "amount": 150, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Market",
		"date": "2026-07-20",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Salary", "type": "income", "amount": 1000, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Misc", "merchant": "Employer",
		"date": "2026-07-31",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Metro", "type": "expense", "amount": 90, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Transport", "merchant": "Metro",
		"date": "2026-08-01",
	})
	createExportEntry(t, router, otherAuth.Token, otherAccounts[0].ID, map[string]any{
		"title": "Other food", "type": "expense", "amount": 9999, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Other",
		"date": "2026-07-12",
	})

	report := performJSONRequest[TransactionReportResponse](
		t, router, http.MethodGet,
		"/v1/reports/transactions/summary?start_date=2026-07-01&end_date=2026-07-31&category=Food+%26+Drinks",
		authResponse.Token, nil, http.StatusOK,
	)

	if report.Summary.TotalExpense != 400 || report.Summary.TotalIncome != 0 ||
		report.Summary.NetCashflow != -400 || report.Summary.TransactionCount != 2 {
		t.Fatalf("unexpected report summary: %#v", report.Summary)
	}
	if len(report.ByCategory) != 1 || report.ByCategory[0].Label != "Food & Drinks" ||
		report.ByCategory[0].Amount != 400 || report.ByCategory[0].TransactionCount != 2 ||
		report.ByCategory[0].Percentage != 100 {
		t.Fatalf("unexpected category breakdown: %#v", report.ByCategory)
	}
	if len(report.ByMerchant) != 2 || report.ByMerchant[0].Label != "Cafe" || report.ByMerchant[0].Amount != 250 {
		t.Fatalf("unexpected merchant breakdown: %#v", report.ByMerchant)
	}
	if len(report.ByAccount) != 1 || report.ByAccount[0].AccountID == nil ||
		*report.ByAccount[0].AccountID != accounts[0].ID || report.ByAccount[0].Amount != 400 {
		t.Fatalf("unexpected account breakdown: %#v", report.ByAccount)
	}
	if len(report.ByMonth) != 1 || report.ByMonth[0].Month != "2026-07" ||
		report.ByMonth[0].Expense != 400 || report.ByMonth[0].Income != 0 {
		t.Fatalf("unexpected month breakdown: %#v", report.ByMonth)
	}
	for _, category := range report.ByCategory {
		if category.Amount == 9999 {
			t.Fatalf("report leaked another user's entries: %#v", report.ByCategory)
		}
	}
}

func TestTransactionSummaryReportSupportsEmptyAndRejectsInvalidFilters(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "report-empty-device"}, http.StatusOK,
	)

	report := performJSONRequest[TransactionReportResponse](
		t, router, http.MethodGet, "/v1/reports/transactions/summary?type=income", authResponse.Token, nil, http.StatusOK,
	)
	if report.Summary.TransactionCount != 0 || report.Summary.TotalExpense != 0 || len(report.ByCategory) != 0 {
		t.Fatalf("expected empty report, got %#v", report)
	}

	_ = performJSONRequest[map[string]any](
		t, router, http.MethodGet, "/v1/reports/transactions/summary?start_date=bad-date",
		authResponse.Token, nil, http.StatusUnprocessableEntity,
	)
}
