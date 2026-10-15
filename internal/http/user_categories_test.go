package http

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func seedCategoryEntry(t *testing.T, userID uint, entryType, category string) {
	t.Helper()
	entry := models.Entry{
		UserID: userID, Title: category, Type: entryType, Category: category, Date: "2026-10-01",
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatalf("failed to seed entry: %v", err)
	}
}

func removeCategoryPath(entryType, name string) string {
	return "/v1/categories/custom?" + url.Values{"type": {entryType}, "name": {name}}.Encode()
}

func TestCustomCategoriesComeFromTheUsersOwnEntries(t *testing.T) {
	// Every build so far kept a typed category on its one entry. Those are the
	// user's categories already, and the picker should offer them without the
	// user typing them again.
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	other, _ := createBillingTestUserSession(t)

	seedCategoryEntry(t, user.ID, "income", "Dividend")
	seedCategoryEntry(t, user.ID, "income", "dividend") // same category, other spelling
	seedCategoryEntry(t, user.ID, "Expense", "Pet Care")
	seedCategoryEntry(t, user.ID, "expense", "Food & Drinks") // built in
	seedCategoryEntry(t, user.ID, "expense", "Food")          // legacy spelling of a built-in
	seedCategoryEntry(t, user.ID, "income", "Salary")         // built in
	seedCategoryEntry(t, user.ID, "income", "Income")         // placeholder from older builds
	seedCategoryEntry(t, other.ID, "expense", "Their Own")

	got := performJSONRequest[CustomCategoriesResponse](t, router, http.MethodGet, "/v1/categories/custom", token, nil, http.StatusOK)

	if strings.Join(got.Expense, "|") != "Pet Care" {
		t.Fatalf("expense categories = %v, want only Pet Care", got.Expense)
	}
	if len(got.Income) != 1 || !strings.EqualFold(got.Income[0], "Dividend") {
		t.Fatalf("income categories = %v, want one Dividend", got.Income)
	}
}

func TestCustomCategoryAddedFromThePickerIsKept(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	created := performJSONRequest[CustomCategoryResponse](t, router, http.MethodPost, "/v1/categories/custom", token,
		map[string]any{"name": "  Rental   income ", "type": "Income"}, http.StatusCreated)
	if created.Name != "Rental income" || created.Type != "income" || created.Canonical {
		t.Fatalf("unexpected add response: %#v", created)
	}

	// Adding it again, in any case, is the same category.
	again := performJSONRequest[CustomCategoryResponse](t, router, http.MethodPost, "/v1/categories/custom", token,
		map[string]any{"name": "rental INCOME", "type": "income"}, http.StatusOK)
	if again.Name != "Rental income" {
		t.Fatalf("a repeat add should answer with the existing name, got %#v", again)
	}

	got := performJSONRequest[CustomCategoriesResponse](t, router, http.MethodGet, "/v1/categories/custom", token, nil, http.StatusOK)
	if strings.Join(got.Income, "|") != "Rental income" || len(got.Expense) != 0 {
		t.Fatalf("listed categories = %#v", got)
	}
}

func TestCustomCategoryThatIsABuiltInIsNotStored(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)

	got := performJSONRequest[CustomCategoryResponse](t, router, http.MethodPost, "/v1/categories/custom", token,
		map[string]any{"name": "groceries", "type": "expense"}, http.StatusOK)
	if !got.Canonical || got.Name != "Food & Drinks" {
		t.Fatalf("a built-in's alias should resolve to it, got %#v", got)
	}
	var count int64
	database.DB.Model(&models.UserCategory{}).Where("user_id = ?", user.ID).Count(&count)
	if count != 0 {
		t.Fatalf("nothing should be stored for a built-in, found %d rows", count)
	}
}

func TestRemovedCustomCategoryStaysOffTheListUntilAddedAgain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	seedCategoryEntry(t, user.ID, "expense", "Dividned") // a typo the user wants gone

	performJSONRequest[map[string]any](t, router, http.MethodDelete, removeCategoryPath("expense", "dividned"), token, nil, http.StatusOK)

	got := performJSONRequest[CustomCategoriesResponse](t, router, http.MethodGet, "/v1/categories/custom", token, nil, http.StatusOK)
	if len(got.Expense) != 0 {
		t.Fatalf("a removed category came back from its entry: %v", got.Expense)
	}
	var entry models.Entry
	database.DB.Where("user_id = ?", user.ID).First(&entry)
	if entry.Category != "Dividned" {
		t.Fatalf("removing a category must not touch the entries using it, got %q", entry.Category)
	}

	performJSONRequest[CustomCategoryResponse](t, router, http.MethodPost, "/v1/categories/custom", token,
		map[string]any{"name": "Dividned", "type": "expense"}, http.StatusCreated)
	got = performJSONRequest[CustomCategoriesResponse](t, router, http.MethodGet, "/v1/categories/custom", token, nil, http.StatusOK)
	if strings.Join(got.Expense, "|") != "Dividned" {
		t.Fatalf("adding a removed category should bring it back, got %v", got.Expense)
	}
}

func TestCustomCategoryInputIsChecked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	for _, body := range []map[string]any{
		{"name": "Dividend", "type": "transfer"},
		{"name": "   ", "type": "income"},
		{"name": strings.Repeat("x", maxCustomCategoryLength+1), "type": "income"},
	} {
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/categories/custom", token, body, http.StatusUnprocessableEntity)
	}
	performJSONRequest[map[string]any](t, router, http.MethodDelete, removeCategoryPath("", "Dividend"), token, nil, http.StatusUnprocessableEntity)
}

func TestDeleteUserDataRemovesUserCategories(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	if err := saveUserCategory(database.DB, user.ID, "income", "Dividend", false); err != nil {
		t.Fatal(err)
	}

	if _, err := deleteUserData(database.DB, user); err != nil {
		t.Fatalf("delete user data: %v", err)
	}
	var count int64
	database.DB.Model(&models.UserCategory{}).Where("user_id = ?", user.ID).Count(&count)
	if count != 0 {
		t.Fatalf("user categories survived account deletion: %d", count)
	}
}
