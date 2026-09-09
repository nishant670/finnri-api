package http

import (
	"strings"
	"testing"

	"finnri/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestOwnedEntryMutationAlwaysScopesUser(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=localhost user=test dbname=test sslmode=disable",
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}

	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return ownedEntries(tx, 42).Where("id = ?", 7).Delete(&models.Entry{})
	})
	if !strings.Contains(sql, "user_id") || !strings.Contains(sql, "id") ||
		!strings.Contains(sql, "42") || !strings.Contains(sql, "7") {
		t.Fatalf("ownership predicate missing from mutation: %s", sql)
	}
}
