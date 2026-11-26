package http

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The card catalogue (docs/CREDIT_CARD_SYSTEM_PLAN_2026-11-13.md §5–6): real
// card products, so Add card can offer "HDFC Millennia" rather than only
// "HDFC Bank", and the card can be drawn as a sketch of the real one.

// listCardProducts answers GET /v1/card-products?issuer=hdfc with that bank's
// cards, most-held first. Without issuer it lists the whole catalogue. Hidden
// products are left out.
func listCardProducts(c *gin.Context) {
	query := database.DB.Where("hidden = ?", false)
	if raw := strings.TrimSpace(c.Query("issuer")); raw != "" {
		issuer, ok := cardIssuerFor(raw)
		if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_issuer"})
			return
		}
		query = query.Where("issuer_id = ?", issuer)
	}
	var products []models.CardProduct
	if err := query.Order("issuer_id ASC, sort_order ASC, name ASC").Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_card_products"})
		return
	}
	c.JSON(http.StatusOK, products)
}

// cardIssuerFor accepts a provider id ("hdfc") or anything the provider list
// recognises ("HDFC Bank"), and only providers that issue credit cards.
func cardIssuerFor(value string) (string, bool) {
	provider, ok := accountProviderByID(value)
	if !ok {
		provider, ok = matchAccountProvider(value)
	}
	if !ok || !providerSupportsType(provider, "credit_card") {
		return "", false
	}
	return provider.ID, true
}

func normalizeAccountCardNetwork(value string) (string, bool) {
	network := strings.ToLower(strings.TrimSpace(value))
	switch network {
	case "master card", "master-card", "mc":
		network = "mastercard"
	case "american express":
		network = "amex"
	case "diners club":
		network = "diners"
	case "ru pay":
		network = "rupay"
	}
	if network == "" || slices.Contains(models.CardNetworks, network) {
		return network, true
	}
	return "", false
}

// applyAccountCardProduct runs after accountInput.apply has settled the
// account's type and bank. It links the card to its product, checks the
// product is that bank's, and the first time a product is picked fills the
// fee and waiver the user has not typed. It returns field errors for a 422.
func applyAccountCardProduct(input accountInput, account *models.Account) (map[string]string, error) {
	fields := map[string]string{}
	previous := ""
	if account.CardProductID != nil {
		previous = *account.CardProductID
	}

	if account.Type != "credit_card" {
		if input.CardProductID != nil && strings.TrimSpace(*input.CardProductID) != "" {
			fields["card_product_id"] = "is only valid for credit cards"
		}
		if input.CardNetwork != nil && strings.TrimSpace(*input.CardNetwork) != "" {
			fields["card_network"] = "is only valid for credit cards"
		}
		account.CardProductID = nil
		account.CardNetwork = ""
		account.CardProduct = nil
		return fields, nil
	}

	if input.CardNetwork != nil {
		network, ok := normalizeAccountCardNetwork(*input.CardNetwork)
		if !ok {
			fields["card_network"] = "must be visa, mastercard, rupay, amex or diners"
		} else {
			account.CardNetwork = network
		}
	}

	if input.CardProductID == nil {
		// An older client does not know the field. Keep the link unless that
		// client moved the card to another bank, which makes it wrong.
		if previous != "" {
			product, err := loadCardProduct(previous)
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			if err != nil || product.IssuerID != account.ProviderID {
				account.CardProductID = nil
			}
		}
		return fields, nil
	}

	id := strings.ToLower(strings.TrimSpace(*input.CardProductID))
	if id == "" {
		account.CardProductID = nil
		return fields, nil
	}
	product, err := loadCardProduct(id)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && product.Hidden && id != previous) {
		fields["card_product_id"] = "is not in the card catalogue"
		return fields, nil
	}
	if err != nil {
		return nil, err
	}
	if account.ProviderID == "" {
		// Picking the card names the bank.
		if provider, ok := accountProviderByID(product.IssuerID); ok {
			account.ProviderID = provider.ID
			account.Provider = provider.DisplayName
		}
	} else if account.ProviderID != product.IssuerID {
		fields["card_product_id"] = "belongs to another bank"
		return fields, nil
	}
	account.CardProductID = &product.ID
	summary := product.Summary()
	account.CardProduct = &summary

	if id != previous {
		// What the user typed wins; the catalogue only fills a blank.
		if product.AnnualFee != nil && account.AnnualFee == 0 && (input.AnnualFee == nil || *input.AnnualFee == 0) {
			account.AnnualFee = *product.AnnualFee
		}
		if product.FeeWaiverSpend != nil && account.FeeWaiverSpend == 0 && (input.FeeWaiverSpend == nil || *input.FeeWaiverSpend == 0) {
			account.FeeWaiverSpend = *product.FeeWaiverSpend
		}
	}
	return fields, nil
}

func loadCardProduct(id string) (models.CardProduct, error) {
	var product models.CardProduct
	err := database.DB.Where("id = ?", id).First(&product).Error
	return product, err
}

// hydrateCardProduct attaches the product an account points at, so the app
// can draw the card without a second request.
func hydrateCardProduct(account *models.Account) {
	account.CardProduct = nil
	if account.CardProductID == nil || *account.CardProductID == "" || database.DB == nil {
		return
	}
	product, err := loadCardProduct(*account.CardProductID)
	if err != nil {
		return
	}
	summary := product.Summary()
	account.CardProduct = &summary
}

// --- Web admin: the catalogue editor ---

type cardProductInput struct {
	ID             string        `json:"id"`
	IssuerID       string        `json:"issuer_id"`
	Name           string        `json:"name"`
	Tone           string        `json:"tone"`
	Drawing        string        `json:"drawing"`
	Orientation    string        `json:"orientation"`
	ArtIdea        string        `json:"art_idea"`
	ArtDrawnAs     string        `json:"art_drawn_as"`
	ArtReviewed    bool          `json:"art_reviewed"`
	JoiningFee     *models.Money `json:"joining_fee"`
	AnnualFee      *models.Money `json:"annual_fee"`
	FeeWaiverSpend *models.Money `json:"fee_waiver_spend"`
	SourceURLs     []string      `json:"source_urls"`
	// A day ("2026-11-13") or a full timestamp; empty clears it.
	LastVerifiedAt string `json:"last_verified_at"`
	SortOrder      int    `json:"sort_order"`
	Hidden         bool   `json:"hidden"`
}

var (
	cardProductIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	cardDrawingPattern   = regexp.MustCompile(`^[a-z0-9]+([+-][a-z0-9]+)*$`)
)

const maxCardProductSources = 10

// validate checks an admin's product and returns it cleaned, or field errors.
// Fees need a source: the catalogue stores facts from the issuer's own terms
// and the link they came from, never a guess.
func (input cardProductInput) validate() (models.CardProduct, map[string]string) {
	fields := map[string]string{}
	product := models.CardProduct{
		ID:             strings.ToLower(strings.TrimSpace(input.ID)),
		Name:           strings.Join(strings.Fields(input.Name), " "),
		Tone:           strings.ToLower(strings.TrimSpace(input.Tone)),
		Drawing:        strings.ToLower(strings.TrimSpace(input.Drawing)),
		Orientation:    strings.ToLower(strings.TrimSpace(input.Orientation)),
		ArtIdea:        strings.TrimSpace(input.ArtIdea),
		ArtDrawnAs:     strings.TrimSpace(input.ArtDrawnAs),
		ArtReviewed:    input.ArtReviewed,
		JoiningFee:     input.JoiningFee,
		AnnualFee:      input.AnnualFee,
		FeeWaiverSpend: input.FeeWaiverSpend,
		SourceURLs:     models.StringList{},
		SortOrder:      input.SortOrder,
		Hidden:         input.Hidden,
	}
	if product.Orientation == "" {
		product.Orientation = models.CardOrientationLandscape
	}
	if !cardProductIDPattern.MatchString(product.ID) || len(product.ID) > 64 {
		fields["id"] = "must be lowercase words joined by hyphens, like hdfc-millennia"
	}
	if issuer, ok := cardIssuerFor(input.IssuerID); ok {
		product.IssuerID = issuer
	} else {
		fields["issuer_id"] = "must be a bank that issues credit cards"
	}
	if product.Name == "" || len(product.Name) > 80 {
		fields["name"] = "is required, up to 80 characters"
	}
	if !slices.Contains(models.CardTones, product.Tone) {
		fields["tone"] = "must be deep, solid, tint or pale"
	}
	if !cardDrawingPattern.MatchString(product.Drawing) || len(product.Drawing) > 32 {
		fields["drawing"] = "must name one of the app's drawings"
	}
	if !slices.Contains(models.CardOrientations, product.Orientation) {
		fields["orientation"] = "must be landscape or portrait"
	}
	if len(product.ArtIdea) > 120 {
		fields["art_idea"] = "must be at most 120 characters"
	}
	if len(product.ArtDrawnAs) > 120 {
		fields["art_drawn_as"] = "must be at most 120 characters"
	}
	for name, fee := range map[string]*models.Money{"joining_fee": input.JoiningFee, "annual_fee": input.AnnualFee, "fee_waiver_spend": input.FeeWaiverSpend} {
		if fee != nil && *fee < 0 {
			fields[name] = "must not be negative"
		}
	}
	for _, raw := range input.SourceURLs {
		source := strings.TrimSpace(raw)
		if source == "" {
			continue
		}
		parsed, err := url.Parse(source)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || len(source) > 500 {
			fields["source_urls"] = "must be web links"
			break
		}
		product.SourceURLs = append(product.SourceURLs, source)
	}
	if len(product.SourceURLs) > maxCardProductSources {
		fields["source_urls"] = "at most 10 links"
	}
	if (input.JoiningFee != nil || input.AnnualFee != nil || input.FeeWaiverSpend != nil) && len(product.SourceURLs) == 0 {
		fields["source_urls"] = "add the issuer page the fees come from"
	}
	if raw := strings.TrimSpace(input.LastVerifiedAt); raw != "" {
		checked, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			checked, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			fields["last_verified_at"] = "must be a date like 2026-11-13"
		} else {
			product.LastVerifiedAt = &checked
		}
	}
	return product, fields
}

func (s *Server) listAdminCardProducts(c *gin.Context) {
	query := database.DB.Model(&models.CardProduct{})
	if raw := strings.TrimSpace(c.Query("issuer")); raw != "" {
		issuer, ok := cardIssuerFor(raw)
		if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_issuer"})
			return
		}
		query = query.Where("issuer_id = ?", issuer)
	}
	var products []models.CardProduct
	if err := query.Order("issuer_id ASC, sort_order ASC, name ASC").Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_card_products"})
		return
	}
	var linked []struct {
		CardProductID string
		Count         int64
	}
	if err := database.DB.Model(&models.Account{}).
		Select("card_product_id, COUNT(*) AS count").
		Where("card_product_id IS NOT NULL").
		Group("card_product_id").
		Scan(&linked).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_card_products"})
		return
	}
	cardsByProduct := map[string]int64{}
	for _, row := range linked {
		cardsByProduct[row.CardProductID] = row.Count
	}
	c.JSON(http.StatusOK, gin.H{"products": products, "cards_by_product": cardsByProduct})
}

func (s *Server) createAdminCardProduct(c *gin.Context) {
	var input cardProductInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	product, fields := input.validate()
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_card_product", "fields": fields})
		return
	}
	if _, err := loadCardProduct(product.ID); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "card_product_exists", "fields": gin.H{"id": "is already in the catalogue"}})
		return
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
		return
	}
	if err := database.DB.Create(&product).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
		return
	}
	c.JSON(http.StatusCreated, product)
}

// updateAdminCardProduct replaces a product's fields. The id is the URL's; a
// different id in the body is refused, since accounts point at it.
func (s *Server) updateAdminCardProduct(c *gin.Context) {
	existing, err := loadCardProduct(strings.ToLower(strings.TrimSpace(c.Param("id"))))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "card_product_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
		return
	}
	var input cardProductInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(input.ID) == "" {
		input.ID = existing.ID
	}
	product, fields := input.validate()
	if product.ID != existing.ID {
		fields["id"] = "cannot change; accounts point at it"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_card_product", "fields": fields})
		return
	}
	if product.IssuerID != existing.IssuerID {
		var linked int64
		if err := database.DB.Model(&models.Account{}).Where("card_product_id = ?", existing.ID).Count(&linked).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
			return
		}
		if linked > 0 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_card_product", "fields": gin.H{"issuer_id": "cannot change while cards use this product"}})
			return
		}
	}
	product.CreatedAt = existing.CreatedAt
	// Select("*") writes false and NULL too: an unticked box and a cleared fee
	// are edits, not omissions.
	if err := database.DB.Model(&existing).Select("*").Omit("id", "created_at").Updates(&product).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
		return
	}
	saved, err := loadCardProduct(existing.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_card_product"})
		return
	}
	c.JSON(http.StatusOK, saved)
}
