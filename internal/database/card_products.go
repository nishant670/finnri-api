package database

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/models"
)

// card_products_seed.json is the launch catalogue: the 36 popular cards drawn
// in Round 19 (docs/redesign/sketch-cards/library.json, without the four
// entries that are the owner's own cards and have no product). Fees are left
// empty on purpose: they are filled from each issuer's published terms, with a
// source, in the web admin.
//
//go:embed card_products_seed.json
var cardProductSeedJSON []byte

// CardProductSeed returns the launch catalogue.
func CardProductSeed() ([]models.CardProduct, error) {
	var products []models.CardProduct
	if err := json.Unmarshal(cardProductSeedJSON, &products); err != nil {
		return nil, fmt.Errorf("card product seed: %w", err)
	}
	return products, nil
}

// SeedCardProducts inserts the launch catalogue's missing products and leaves
// existing ones alone, so an edit made in the web admin survives every deploy.
// It is safe to run on every migration.
func SeedCardProducts(db *gorm.DB) error {
	products, err := CardProductSeed()
	if err != nil {
		return err
	}
	if len(products) == 0 {
		return nil
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		Create(&products).Error
}
