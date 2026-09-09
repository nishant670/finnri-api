package http

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

type accountProvider = models.AccountProviderDetails

var accountProviders = []accountProvider{
	{ID: "hdfc", DisplayName: "HDFC Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"HDFC", "HDFC card"}},
	{ID: "icici", DisplayName: "ICICI Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"ICICI", "ICICI card"}},
	{ID: "sbi", DisplayName: "State Bank of India", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"SBI", "SBI card", "SBI State Bank of India"}},
	{ID: "axis", DisplayName: "Axis Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Axis", "Axis card"}},
	{ID: "kotak", DisplayName: "Kotak Mahindra Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Kotak", "Kotak Bank"}},
	{ID: "amex", DisplayName: "American Express", TypeSupport: []string{"credit_card"}, AssetKey: "credit-card", Aliases: []string{"Amex", "American Express card"}},
	{ID: "hsbc", DisplayName: "HSBC", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"HSBC Bank", "HSBC card"}},
	{ID: "standard-chartered", DisplayName: "Standard Chartered", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"SCB", "Standard Chartered Bank"}},
	{ID: "citibank", DisplayName: "Citibank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Citi", "Citi Bank", "Citi card"}},
	{ID: "federal", DisplayName: "Federal Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Federal", "Federal Bank card", "Federal card"}},
	{ID: "idfc-first", DisplayName: "IDFC FIRST Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"IDFC", "IDFC First", "IDFC First Bank", "IDFC card"}},
	{ID: "yes-bank", DisplayName: "YES BANK", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Yes Bank", "YES card"}},
	{ID: "indusind", DisplayName: "IndusInd Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"IndusInd", "IndusInd card"}},
	{ID: "rbl", DisplayName: "RBL Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"RBL", "RBL card"}},
	{ID: "au-small-finance", DisplayName: "AU Small Finance Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"AU Bank", "AU Small Finance", "AU card"}},
	{ID: "bank-of-baroda", DisplayName: "Bank of Baroda", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"BoB", "BOB Bank", "Bank of Baroda card"}},
	{ID: "pnb", DisplayName: "Punjab National Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"PNB", "Punjab National", "PNB card"}},
	{ID: "canara", DisplayName: "Canara Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Canara", "Canara card"}},
	{ID: "union-bank", DisplayName: "Union Bank of India", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Union Bank", "UBI", "Union Bank card"}},
	{ID: "bandhan", DisplayName: "Bandhan Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Bandhan", "Bandhan card"}},
	{ID: "idbi", DisplayName: "IDBI Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"IDBI", "IDBI card"}},
	{ID: "karnataka", DisplayName: "Karnataka Bank", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Karnataka", "KBL", "Karnataka card"}},
	{ID: "dbs", DisplayName: "DBS Bank", TypeSupport: []string{"bank", "debit_card"}, AssetKey: "bank", Aliases: []string{"DBS", "Digibank", "DBS Digibank", "Digibank by DBS"}},
	{ID: "bank-of-india", DisplayName: "Bank of India", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"BOI", "Bank of India card"}},
	{ID: "central-bank", DisplayName: "Central Bank of India", TypeSupport: []string{"bank", "credit_card", "debit_card"}, AssetKey: "bank", Aliases: []string{"Central Bank", "CBI Bank", "Central Bank card"}},
	{ID: "scapia", DisplayName: "Scapia", TypeSupport: []string{"credit_card"}, AssetKey: "credit-card", Aliases: []string{"Scapia card", "Scapia credit card", "Scapia Federal", "Scapia Federal Bank credit card"}},
	{ID: "slice", DisplayName: "Slice", TypeSupport: []string{"credit_card"}, AssetKey: "credit-card", Aliases: []string{"Slice card", "Slice credit card"}},
	{ID: "onecard", DisplayName: "OneCard", TypeSupport: []string{"credit_card"}, AssetKey: "credit-card", Aliases: []string{"One Card", "OneCard credit card", "One Card credit card"}},
	{ID: "uni", DisplayName: "Uni", TypeSupport: []string{"credit_card"}, AssetKey: "credit-card", Aliases: []string{"Uni Card", "Uni credit card"}},
	{ID: "jupiter", DisplayName: "Jupiter", TypeSupport: []string{"bank", "debit_card"}, AssetKey: "bank", Aliases: []string{"Jupiter account", "Jupiter Federal", "Jupiter Federal Bank"}},
	{ID: "fi", DisplayName: "Fi", TypeSupport: []string{"bank", "debit_card"}, AssetKey: "bank", Aliases: []string{"Fi Money", "Fi account", "Fi Federal", "Fi Federal Bank"}},
	{ID: "google-pay", DisplayName: "Google Pay", TypeSupport: []string{"upi"}, AssetKey: "google", Aliases: []string{"GPay", "GooglePay"}},
	{ID: "phonepe", DisplayName: "PhonePe", TypeSupport: []string{"upi", "wallet"}, AssetKey: "alpha-p-circle", Aliases: []string{"Phone Pe", "PhonePe Wallet"}},
	{ID: "paytm", DisplayName: "Paytm", TypeSupport: []string{"upi", "wallet"}, AssetKey: "wallet", Aliases: []string{"Paytm UPI", "Paytm Wallet"}},
	{ID: "bhim", DisplayName: "BHIM", TypeSupport: []string{"upi"}, AssetKey: "qrcode-scan", Aliases: []string{"BHIM UPI"}},
	{ID: "amazon-pay", DisplayName: "Amazon Pay", TypeSupport: []string{"upi", "wallet"}, AssetKey: "amazon", Aliases: []string{"AmazonPay", "Amazon Pay UPI"}},
	{ID: "mobikwik", DisplayName: "MobiKwik", TypeSupport: []string{"wallet"}, AssetKey: "wallet", Aliases: []string{"Mobi Kwik"}},
}

func listAccountProviders(c *gin.Context) {
	requestedType := strings.TrimSpace(c.Query("type"))
	if requestedType != "" {
		requestedType = normalizeAccountType(requestedType)
		if requestedType == "" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_account_type"})
			return
		}
	}
	providers := make([]accountProvider, 0, len(accountProviders))
	for _, provider := range accountProviders {
		if requestedType == "" || providerSupportsType(provider, requestedType) {
			providers = append(providers, provider)
		}
	}
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].DisplayName < providers[j].DisplayName })
	c.JSON(http.StatusOK, providers)
}

func accountProviderByID(id string) (accountProvider, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, provider := range accountProviders {
		if provider.ID == id {
			return provider, true
		}
	}
	return accountProvider{}, false
}

func normalizeProviderText(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return -1
	}, strings.TrimSpace(value))
}

func matchAccountProvider(value string) (accountProvider, bool) {
	needle := normalizeProviderText(value)
	if needle == "" {
		return accountProvider{}, false
	}
	for _, provider := range accountProviders {
		candidates := append([]string{provider.ID, provider.DisplayName}, provider.Aliases...)
		for _, candidate := range candidates {
			if normalizeProviderText(candidate) == needle {
				return provider, true
			}
		}
	}
	return accountProvider{}, false
}

func providerSupportsType(provider accountProvider, accountType string) bool {
	for _, supported := range provider.TypeSupport {
		if supported == accountType {
			return true
		}
	}
	return false
}

func hydrateAccountProvider(account *models.Account) {
	if account.ProviderID == "" {
		if provider, ok := matchAccountProvider(account.Provider); ok && providerSupportsType(provider, normalizeAccountType(account.Type)) {
			account.ProviderID = provider.ID
		}
	}
	if provider, ok := accountProviderByID(account.ProviderID); ok {
		copy := provider
		account.ProviderDetails = &copy
	}
	if account.Identifier == "" {
		account.Identifier = accountDisplayIdentifier(*account)
	}
}
