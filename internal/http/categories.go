package http

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Canonical transaction categories. This is the single source of truth for what
// the parser may emit and what the API will persist.
//
// It drifted once: the mobile app added "Food & Drinks" and "Transport" to its
// picker while schemas/expense_entry.schema.json and internal/ai/prompt.txt
// still only knew the original six. The parser could not express either value,
// so every AI-captured meal was filed as "Food" and every auto rickshaw as
// "Travel", while manual entries used the newer names — the same spend split
// across two keys and every rollup quietly stopped being true.
//
// TestCanonicalCategoriesMatchParseSchema and TestCanonicalCategoriesMatchPrompt
// fail if these three ever diverge again.
//
// Clients read this list from GET /v1/categories rather than copying it, which
// is what keeps a fourth and fifth copy from appearing in the app and the web
// front end. TestCategoriesEndpointServesCanonicalList guards that route.
var canonicalCategories = []string{
	"Food & Drinks",
	"Transport",
	"Travel",
	"Shopping",
	"Bills",
	"Entertainment",
	"Family/Gifts",
	"Misc",
}

var canonicalIncomeCategories = []string{
	"Salary",
	"Freelance",
	"Interest",
	"Dividend",
	"Refund",
	"Other",
}

// defaultCategory is the confirm-first fallback: when the category is unknown we
// file it here and flag it for confirmation rather than guessing.
const defaultCategory = "Misc"
const defaultIncomeCategory = "Other"

// categoryAliases maps loose or legacy input onto the canonical list.
//
// Transport and Travel are deliberately distinct. Transport is getting around
// day to day — autos, cabs, the metro, fuel. Travel is going somewhere — flights,
// hotels, a trip. Collapsing them loses a distinction people actually budget
// against, so the aliases below route each term to the right one.
var categoryAliases = map[string]string{
	// Food & Drinks — "food" is the legacy name and still arrives from older builds.
	"food":            "Food & Drinks",
	"food & drink":    "Food & Drinks",
	"food and drinks": "Food & Drinks",
	"food and drink":  "Food & Drinks",
	"dining":          "Food & Drinks",
	"restaurant":      "Food & Drinks",
	"restaurants":     "Food & Drinks",
	"groceries":       "Food & Drinks",
	"grocery":         "Food & Drinks",
	"cafe":            "Food & Drinks",
	"coffee":          "Food & Drinks",
	"snacks":          "Food & Drinks",

	// Transport — daily movement.
	"transportation": "Transport",
	"commute":        "Transport",
	"cab":            "Transport",
	"taxi":           "Transport",
	"uber":           "Transport",
	"ola":            "Transport",
	"rapido":         "Transport",
	"auto":           "Transport",
	"autorickshaw":   "Transport",
	"auto rickshaw":  "Transport",
	"rickshaw":       "Transport",
	"metro":          "Transport",
	"bus":            "Transport",
	"train":          "Transport",
	"fuel":           "Transport",
	"petrol":         "Transport",
	"diesel":         "Transport",
	"parking":        "Transport",
	"toll":           "Transport",

	// Travel — trips away from home.
	"trip":          "Travel",
	"trips":         "Travel",
	"vacation":      "Travel",
	"holiday":       "Travel",
	"flight":        "Travel",
	"flights":       "Travel",
	"airfare":       "Travel",
	"hotel":         "Travel",
	"hotels":        "Travel",
	"lodging":       "Travel",
	"accommodation": "Travel",
	"airbnb":        "Travel",
	"air bnb":       "Travel",
	"stay":          "Travel",
	"resort":        "Travel",

	// Shopping.
	"clothes":     "Shopping",
	"clothing":    "Shopping",
	"apparel":     "Shopping",
	"electronics": "Shopping",

	// Entertainment.
	"movies":    "Entertainment",
	"movie":     "Entertainment",
	"streaming": "Entertainment",
	"ott":       "Entertainment",
	"games":     "Entertainment",
	"gaming":    "Entertainment",
	"music":     "Entertainment",

	// Bills.
	"bill":          "Bills",
	"utilities":     "Bills",
	"utility":       "Bills",
	"rent":          "Bills",
	"recharge":      "Bills",
	"electricity":   "Bills",
	"internet":      "Bills",
	"subscription":  "Bills",
	"subscriptions": "Bills",
	// The old subscription-only vocabulary. These were a separate list that
	// leaked into entry categories through autopay; they are recurring service
	// bills, so they land in Bills.
	"productivity": "Bills",
	"cloud":        "Bills",
	"membership":   "Bills",
	"learning":     "Bills",

	// Family/Gifts.
	"family": "Family/Gifts",
	"gifts":  "Family/Gifts",
	"gift":   "Family/Gifts",

	// Misc, plus strays that reached the database before categories were validated
	// on save. "Finance" was an investment entry; the Investment tag carries that
	// meaning now, so the category falls back rather than inventing a bucket.
	"miscellaneous": "Misc",
	"other":         "Misc",
	"others":        "Misc",
	"finance":       "Misc",
	"split":         "Misc",
}

var incomeCategoryAliases = map[string]string{
	"salary income":     "Salary",
	"wages":             "Salary",
	"paycheck":          "Salary",
	"freelancing":       "Freelance",
	"consulting":        "Freelance",
	"side hustle":       "Freelance",
	"interest income":   "Interest",
	"bank interest":     "Interest",
	"dividends":         "Dividend",
	"dividend income":   "Dividend",
	"cashback":          "Refund",
	"reimbursement":     "Refund",
	"returned purchase": "Refund",
}

// canonicalCategory resolves a raw value to its canonical form. The second
// return is false when the value is not recognised at all, which callers treat
// as "ask the user" rather than silently filing it somewhere.
func canonicalCategory(value string) (string, bool) {
	if resolved, ok := canonicalCategoryForType(value, "expense"); ok {
		return resolved, true
	}
	return canonicalCategoryForType(value, "income")
}

func canonicalCategoryForType(value, entryType string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", false
	}
	categories := canonicalCategories
	aliases := categoryAliases
	if strings.EqualFold(strings.TrimSpace(entryType), "income") {
		categories = canonicalIncomeCategories
		aliases = incomeCategoryAliases
	}
	for _, category := range categories {
		if strings.ToLower(category) == normalized {
			return category, true
		}
	}
	if alias, ok := aliases[normalized]; ok {
		return alias, true
	}
	return "", false
}

// maxCustomCategoryLength bounds user-authored category names.
const maxCustomCategoryLength = 40

// categoryForSave decides what actually gets persisted.
//
// Recognised names — canonical or legacy alias — are rewritten to their
// canonical form, so "Food" from an older build lands as "Food & Drinks" and
// the taxonomy stays whole.
//
// Anything else is kept verbatim, because the category picker lets people add
// their own ("Pet Care", "Tuition"). A user deliberately naming a bucket is not
// the same failure as the parser inventing one — the parser is now held to the
// canonical enum by the schema and the normalizer, which is where "Finance" and
// "Split" originally came from.
func categoryForSave(value string, entryTypes ...string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	var resolved string
	var ok bool
	if len(entryTypes) > 0 {
		resolved, ok = canonicalCategoryForType(trimmed, entryTypes[0])
	} else {
		// Historical callers without a type are expense-only. Keeping that
		// default prevents an income alias such as "side hustle" from rewriting
		// a user-authored expense category with the same words.
		resolved, ok = canonicalCategoryForType(trimmed, "expense")
	}
	if ok {
		return resolved, true
	}
	if len([]rune(trimmed)) > maxCustomCategoryLength {
		return "", false
	}
	return trimmed, true
}

// categoryLengthMessage explains the only limit on a custom category name.
func categoryLengthMessage() string {
	return "must be " + strconv.Itoa(maxCustomCategoryLength) + " characters or fewer"
}

// CategoriesResponse is what every client picker is built from.
//
// The list exists in this file, in schemas/expense_entry.schema.json, and in
// internal/ai/prompt.txt — three copies inside this repository, held together by
// TestCanonicalCategoriesMatchParseSchema and TestCanonicalCategoriesMatchPrompt.
// Clients used to keep a fourth: the mobile app hardcodes CATEGORIES in
// lib/categories.ts and the web app hardcoded a different eight in its
// AddTransactionModal, which is how "Electronics", "Health" and "Other" reached
// the picker while Travel, Family/Gifts and Misc fell out of it. Two of those
// were silently rewritten by categoryAliases on save, so the picker was telling
// the user something the database disagreed with, and the third was stored
// verbatim as a custom category nobody had chosen to create.
//
// A client that reads this endpoint cannot drift, because it holds no list.
type CategoriesResponse struct {
	Categories       []string `json:"categories"`
	Default          string   `json:"default"`
	IncomeCategories []string `json:"income_categories"`
	IncomeDefault    string   `json:"income_default"`
}

// listCategories serves the canonical picker list.
//
// It is deliberately free of user data: the response is identical for everyone,
// so a client may cache it. Custom categories a user has created are not
// included — those come back on the entries themselves, and a picker keeps them
// selectable by appending the value it is currently editing.
func (s *Server) listCategories(c *gin.Context) {
	categories := make([]string, len(canonicalCategories))
	copy(categories, canonicalCategories)

	c.JSON(http.StatusOK, CategoriesResponse{
		Categories:       categories,
		Default:          defaultCategory,
		IncomeCategories: append([]string(nil), canonicalIncomeCategories...),
		IncomeDefault:    defaultIncomeCategory,
	})
}
