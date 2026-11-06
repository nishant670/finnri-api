package http

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Which account pays for an expense the bill or sentence did not name one for.
//
// The app asks after a draft lands (and as a manual form is typed) whenever
// how it was paid is still the form's own default. Two sources answer, the
// user's own word first:
//
//  1. A rule: a phrase the user tied to an account ("fuel" → SBI BPCL card).
//  2. A habit: one account paid most of the user's recent expenses of the same
//     kind — at least habitMinimumMatches of them in the last habitWindowDays,
//     with habitMinimumShare or more from that one account.
//
// "The same kind" is, in order: the same merchant, the same spend family
// (petrol, diesel, BPCL and Bharat Petroleum are all fuel), the same tag, or
// the same title. The first of those the draft has that history can decide
// wins; a merchant habit is more specific than a family one.
const (
	habitWindowDays      = 90
	habitMinimumMatches  = 3
	habitMinimumShare    = 0.7
	habitScanLimit       = 1000
	paymentRulePhraseMax = 40
	paymentRuleLimit     = 200
)

// spendFamily is a kind of spending people tend to pay one way, usually for a
// card's rewards: a fuel card at the pump, a grocery card at the supermarket.
// Terms match whole words of the title, merchant or tag, after lowercasing and
// turning punctuation into spaces.
type spendFamily struct {
	key   string
	label string
	terms []string
	// excludes are phrases that contain a term but are not the family: "Ola
	// Electric" is a scooter, not a cab.
	excludes []string
}

// Listed most specific first: "Swiggy Instamart" is groceries before it is
// food delivery.
var spendFamilies = []spendFamily{
	{key: "fuel", label: "fuel", terms: []string{
		"fuel", "petrol", "diesel", "cng", "petrol pump", "fuel station", "filling station",
		"bpcl", "hpcl", "iocl", "indian oil", "bharat petroleum", "hindustan petroleum",
		"nayara", "jio bp",
	}},
	{key: "groceries", label: "groceries", terms: []string{
		"grocery", "groceries", "kirana", "supermarket", "blinkit", "zepto", "bigbasket",
		"big basket", "instamart", "dmart", "d mart", "jiomart", "jio mart",
	}},
	{key: "food_delivery", label: "food delivery", terms: []string{
		"swiggy", "zomato", "food delivery",
	}},
	{key: "cabs", label: "cab rides", terms: []string{
		"uber", "ola", "rapido", "blusmart", "blu smart", "namma yatri", "cab", "taxi",
	}, excludes: []string{"ola electric", "ola s1", "uber eats"}},
}

// Tags that say nothing about what was bought.
var uninformativeTags = map[string]bool{"": true, "general": true, "other": true, "misc": true}

// normalizeSpendText lowercases and keeps letters and digits, with every run of
// anything else as one space: "Soami BPCL, Sec-14" → "soami bpcl sec 14".
func normalizeSpendText(value string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// containsWords reports whether phrase appears in text as whole words. Both
// are already normalized.
func containsWords(text, phrase string) bool {
	if text == "" || phrase == "" {
		return false
	}
	return strings.Contains(" "+text+" ", " "+phrase+" ")
}

func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if containsWords(text, phrase) {
			return true
		}
	}
	return false
}

// familiesIn lists the spend families any of texts mentions, in family order.
func familiesIn(texts ...string) []spendFamily {
	var found []spendFamily
	for _, family := range spendFamilies {
		for _, term := range family.terms {
			matched := false
			for _, text := range texts {
				if containsWords(text, term) && !containsAny(text, family.excludes) {
					matched = true
					break
				}
			}
			if matched {
				found = append(found, family)
				break
			}
		}
	}
	return found
}

// familyOfPhrase is the family a rule phrase names, if any: a rule for "fuel"
// or "BPCL" covers petrol and Bharat Petroleum too.
func familyOfPhrase(phraseKey string) *spendFamily {
	for i, family := range spendFamilies {
		if phraseKey == family.key || phraseKey == normalizeSpendText(family.label) {
			return &spendFamilies[i]
		}
		for _, term := range family.terms {
			if phraseKey == term {
				return &spendFamilies[i]
			}
		}
	}
	return nil
}

// spendKind is one way two expenses can be "the same kind", with the words the
// app shows for it ("fuel", "Swiggy") and offers as a rule phrase.
type spendKind struct {
	key   string
	label string
}

// spendKindsOf lists an expense's kinds, most specific first: merchant, then
// family, then tag, then title.
func spendKindsOf(title, merchant, tag string) []spendKind {
	titleKey := normalizeSpendText(title)
	merchantKey := normalizeSpendText(merchant)
	tagKey := normalizeSpendText(tag)
	var kinds []spendKind
	if merchantKey != "" {
		kinds = append(kinds, spendKind{key: "m:" + merchantKey, label: strings.TrimSpace(merchant)})
	}
	for _, family := range familiesIn(titleKey, merchantKey, tagKey) {
		kinds = append(kinds, spendKind{key: "f:" + family.key, label: family.label})
	}
	if !uninformativeTags[tagKey] {
		kinds = append(kinds, spendKind{key: "g:" + tagKey, label: strings.TrimSpace(tag)})
	}
	if titleKey != "" {
		kinds = append(kinds, spendKind{key: "t:" + titleKey, label: strings.TrimSpace(title)})
	}
	return kinds
}

// ruleMatches reports whether a rule's phrase covers this expense: the phrase
// itself as whole words of its title, merchant or tag, or, when the phrase
// names a spend family, any word of that family.
func ruleMatches(phraseKey string, texts []string) bool {
	for _, text := range texts {
		if containsWords(text, phraseKey) {
			return true
		}
	}
	if family := familyOfPhrase(phraseKey); family != nil {
		for _, found := range familiesIn(texts...) {
			if found.key == family.key {
				return true
			}
		}
	}
	return false
}

type PaymentSuggestion struct {
	AccountID uint `json:"account_id"`
	// Source is "rule" (the user tied this phrase to the account) or "habit"
	// (the account paid most recent expenses like this one).
	Source string `json:"source"`
	// Label is what the expense was recognised as: the rule's phrase, or the
	// merchant, family ("fuel"), tag or title the habit was found on.
	Label string `json:"label"`
	// RuleID is set for a rule, so the app can offer to remove it.
	RuleID uint `json:"rule_id,omitempty"`
	// Matched and Total count the recent expenses of this kind: Matched were
	// paid from the account. Zero for a rule.
	Matched int `json:"matched"`
	Total   int `json:"total"`
}

type PaymentSuggestionResponse struct {
	Suggestion *PaymentSuggestion `json:"suggestion"`
	// Kind is the most specific way this expense can be recognised again, with
	// or without a suggestion: what "Always use this card for …?" names and
	// saves as the rule's phrase. Null when the expense has nothing to go on.
	Kind *PaymentSpendKind `json:"kind"`
}

type PaymentSpendKind struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func (s *Server) getPaymentSuggestion(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	entryType := strings.ToLower(strings.TrimSpace(c.DefaultQuery("type", "expense")))
	title := limitText(c.Query("title"), 200)
	merchant := limitText(c.Query("merchant"), 200)
	tag := limitText(c.Query("tag"), 60)

	response := PaymentSuggestionResponse{}
	kinds := spendKindsOf(title, merchant, tag)
	if len(kinds) > 0 {
		response.Kind = &PaymentSpendKind{Key: kinds[0].key, Label: ruleLabelFor(kinds)}
	}
	// Only spending is paid with something; income lands where it lands.
	if entryType != "expense" || len(kinds) == 0 {
		c.JSON(http.StatusOK, response)
		return
	}

	suggestion, err := suggestPaymentAccount(userID, title, merchant, tag, kinds, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_suggest_payment"})
		return
	}
	response.Suggestion = suggestion
	c.JSON(http.StatusOK, response)
}

// ruleLabelFor is the phrase "Always use … for X?" offers. A family beats the
// merchant here, unlike for habits: "fuel" covers every pump, while the
// merchant the AI read ("Soami BPCL") is one spelling of one of them.
func ruleLabelFor(kinds []spendKind) string {
	for _, kind := range kinds {
		if strings.HasPrefix(kind.key, "f:") {
			return kind.label
		}
	}
	return kinds[0].label
}

func limitText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

func suggestPaymentAccount(
	userID uint,
	title, merchant, tag string,
	kinds []spendKind,
	now time.Time,
) (*PaymentSuggestion, error) {
	var accounts []models.Account
	if err := database.DB.Select("id").Where("user_id = ?", userID).Find(&accounts).Error; err != nil {
		return nil, err
	}
	owned := make(map[uint]bool, len(accounts))
	for _, account := range accounts {
		owned[account.ID] = true
	}
	if len(owned) == 0 {
		return nil, nil
	}

	// 1. The user's own rules. The longest phrase wins, so "swiggy instamart"
	// beats "swiggy"; ties go to the newest rule.
	var rules []models.AccountRule
	if err := database.DB.Where("user_id = ?", userID).
		Order("updated_at DESC, id DESC").Find(&rules).Error; err != nil {
		return nil, err
	}
	texts := []string{normalizeSpendText(title), normalizeSpendText(merchant), normalizeSpendText(tag)}
	var best *models.AccountRule
	for i := range rules {
		rule := &rules[i]
		if !owned[rule.AccountID] || !ruleMatches(rule.PhraseKey, texts) {
			continue
		}
		if best == nil || len(rule.PhraseKey) > len(best.PhraseKey) {
			best = rule
		}
	}
	if best != nil {
		return &PaymentSuggestion{
			AccountID: best.AccountID, Source: "rule", Label: best.Phrase, RuleID: best.ID,
		}, nil
	}

	// 2. Habits, from the last few months of the user's expenses.
	since := now.AddDate(0, 0, -habitWindowDays).Format("2006-01-02")
	var history []models.Entry
	if err := database.DB.
		Select("title, merchant, tag, account_id").
		Where("user_id = ? AND LOWER(type) = ? AND account_id IS NOT NULL AND date >= ?", userID, "expense", since).
		Order("date DESC, id DESC").
		Limit(habitScanLimit).
		Find(&history).Error; err != nil {
		return nil, err
	}
	byKind := map[string]map[uint]int{}
	for _, entry := range history {
		if entry.AccountID == nil || !owned[*entry.AccountID] {
			continue
		}
		for _, kind := range spendKindsOf(entry.Title, entry.Merchant, entry.Tag) {
			if byKind[kind.key] == nil {
				byKind[kind.key] = map[uint]int{}
			}
			byKind[kind.key][*entry.AccountID]++
		}
	}
	for _, kind := range kinds {
		counts := byKind[kind.key]
		total := 0
		for _, count := range counts {
			total += count
		}
		if total < habitMinimumMatches {
			continue
		}
		accountID, matched := topAccount(counts)
		if float64(matched)/float64(total) < habitMinimumShare {
			// Split between accounts: a less specific kind will not settle it
			// either, and guessing would undo the user's own mix.
			return nil, nil
		}
		return &PaymentSuggestion{
			AccountID: accountID, Source: "habit", Label: kind.label, Matched: matched, Total: total,
		}, nil
	}
	return nil, nil
}

// topAccount is the account with the most entries, ties to the lower id so the
// answer does not change between requests.
func topAccount(counts map[uint]int) (uint, int) {
	ids := make([]uint, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var bestID uint
	best := 0
	for _, id := range ids {
		if counts[id] > best {
			bestID, best = id, counts[id]
		}
	}
	return bestID, best
}

type PaymentRulesResponse struct {
	Rules []models.AccountRule `json:"rules"`
}

func (s *Server) listPaymentRules(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var rules []models.AccountRule
	if err := database.DB.Where("user_id = ?", userID).
		Order("phrase_key ASC").Find(&rules).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_payment_rules"})
		return
	}
	c.JSON(http.StatusOK, PaymentRulesResponse{Rules: rules})
}

type PaymentRuleInput struct {
	AccountID uint   `json:"account_id"`
	Phrase    string `json:"phrase"`
}

// savePaymentRule ties a phrase to an account. A phrase already tied to
// another account moves: one phrase picks one account.
func (s *Server) savePaymentRule(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input PaymentRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	phrase := strings.Join(strings.Fields(input.Phrase), " ")
	phraseKey := normalizeSpendText(phrase)
	fields := map[string]string{}
	if phraseKey == "" {
		fields["phrase"] = "is required"
	} else if len(phrase) > paymentRulePhraseMax || len(phraseKey) > paymentRulePhraseMax {
		fields["phrase"] = "must not exceed 40 characters"
	}
	if input.AccountID == 0 {
		fields["account_id"] = "is required"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_payment_rule", "fields": fields})
		return
	}

	var account models.Account
	if err := database.DB.Select("id").Where("id = ? AND user_id = ?", input.AccountID, userID).
		First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_payment_rule"})
		return
	}

	var rule models.AccountRule
	status := http.StatusOK
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("user_id = ? AND phrase_key = ?", userID, phraseKey).First(&rule).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var count int64
			if err := tx.Model(&models.AccountRule{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
				return err
			}
			if count >= paymentRuleLimit {
				return errPaymentRuleLimit
			}
			rule = models.AccountRule{UserID: userID, AccountID: account.ID, Phrase: phrase, PhraseKey: phraseKey}
			status = http.StatusCreated
			return tx.Create(&rule).Error
		}
		if err != nil {
			return err
		}
		rule.AccountID = account.ID
		rule.Phrase = phrase
		return tx.Save(&rule).Error
	})
	if errors.Is(err, errPaymentRuleLimit) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_payment_rule",
			"fields": map[string]string{"phrase": "you can keep up to 200 rules"},
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_payment_rule"})
		return
	}
	c.JSON(status, rule)
}

var errPaymentRuleLimit = errors.New("payment rule limit reached")

func (s *Server) deletePaymentRule(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	result := database.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&models.AccountRule{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_payment_rule"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment_rule_not_found"})
		return
	}
	c.Status(http.StatusNoContent)
}
