package http

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

// maxUserCategoriesPerType bounds how many categories one user can add for
// expenses, and again for income. Far past what a picker can show usefully;
// it is there so the table cannot be filled by a script.
const maxUserCategoriesPerType = 100

// CustomCategoriesResponse is the user's own categories, per entry type, for
// the picker to offer beside the canonical ones from GET /v1/categories.
type CustomCategoriesResponse struct {
	Expense []string `json:"expense"`
	Income  []string `json:"income"`
}

// CustomCategoryResponse answers an add. Canonical is true when the name was
// one of the built-in categories (or an alias of one), in which case nothing
// was stored and Name is the built-in it means.
type CustomCategoryResponse struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Canonical bool   `json:"canonical"`
}

type customCategoryInput struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// customCategoryType reads an entry type, which arrives in any case. The
// second return is false for anything but expense or income.
func customCategoryType(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "expense":
		return "expense", true
	case "income":
		return "income", true
	}
	return "", false
}

// cleanCategoryName trims and collapses runs of whitespace, so "Pet  Care "
// and "Pet Care" are one category.
func cleanCategoryName(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// placeholderCategoryNames are values older builds wrote when an entry had no
// category. They are not categories anyone chose, so they are never offered.
var placeholderCategoryNames = map[string]bool{
	"income":        true,
	"expense":       true,
	"uncategorized": true,
	"uncategorised": true,
}

// listCustomCategories serves the categories a user has made their own.
//
// Two sources, because custom categories existed before this list did: every
// app build so far stored a typed category on the entry alone, so the user's
// entries are read for names they already use. Rows in user_categories add the
// ones added from the picker before any entry used them, and hide the ones the
// user removed.
func (s *Server) listCustomCategories(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	categories, err := customCategoriesFor(database.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_categories"})
		return
	}
	c.JSON(http.StatusOK, categories)
}

func customCategoriesFor(db *gorm.DB, userID uint) (CustomCategoriesResponse, error) {
	var rows []models.UserCategory
	if err := db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return CustomCategoriesResponse{}, err
	}
	var used []struct {
		Type     string
		Category string
	}
	if err := db.Model(&models.Entry{}).
		Select("LOWER(type) AS type, category").
		Where("user_id = ? AND TRIM(category) <> ''", userID).
		Group("LOWER(type), category").
		Scan(&used).Error; err != nil {
		return CustomCategoriesResponse{}, err
	}

	lists := map[string][]string{"expense": {}, "income": {}}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.Type+"\x00"+row.NameKey] = true
		if !row.Removed {
			lists[row.Type] = append(lists[row.Type], row.Name)
		}
	}
	for _, entry := range used {
		entryType := "expense"
		if entry.Type == "income" {
			entryType = "income"
		}
		name := cleanCategoryName(entry.Category)
		key := strings.ToLower(name)
		if name == "" || placeholderCategoryNames[key] ||
			len([]rune(name)) > maxCustomCategoryLength {
			continue
		}
		// Built-in names, and the legacy spellings older rows still carry,
		// are already in the picker.
		if _, ok := canonicalCategoryForType(name, entryType); ok {
			continue
		}
		if seen[entryType+"\x00"+key] {
			continue
		}
		seen[entryType+"\x00"+key] = true
		lists[entryType] = append(lists[entryType], name)
	}

	for _, list := range lists {
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i]) < strings.ToLower(list[j])
		})
	}
	return CustomCategoriesResponse{Expense: lists["expense"], Income: lists["income"]}, nil
}

// readCustomCategory validates a name and type from a request.
func readCustomCategory(name, rawType string) (string, string, map[string]string) {
	fields := map[string]string{}
	entryType, ok := customCategoryType(rawType)
	if !ok {
		fields["type"] = "must be expense or income"
	}
	cleaned := cleanCategoryName(name)
	switch {
	case cleaned == "":
		fields["name"] = "is required"
	case len([]rune(cleaned)) > maxCustomCategoryLength:
		fields["name"] = categoryLengthMessage()
	}
	return cleaned, entryType, fields
}

// addCustomCategory keeps a category the user typed, so the picker offers it
// on their next transaction. Adding one they removed brings it back.
func (s *Server) addCustomCategory(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input customCategoryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	name, entryType, fields := readCustomCategory(input.Name, input.Type)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_category", "fields": fields})
		return
	}
	// "food" is Food & Drinks, not a second category that looks like it.
	if canonical, ok := canonicalCategoryForType(name, entryType); ok {
		c.JSON(http.StatusOK, CustomCategoryResponse{Name: canonical, Type: entryType, Canonical: true})
		return
	}

	key := strings.ToLower(name)
	var existing models.UserCategory
	err := database.DB.Where("user_id = ? AND type = ? AND name_key = ?", userID, entryType, key).
		First(&existing).Error
	if err == nil && !existing.Removed {
		c.JSON(http.StatusOK, CustomCategoryResponse{Name: existing.Name, Type: entryType})
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_category"})
		return
	}

	var count int64
	if err := database.DB.Model(&models.UserCategory{}).
		Where("user_id = ? AND type = ? AND removed = ?", userID, entryType, false).
		Count(&count).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_category"})
		return
	}
	if count >= maxUserCategoriesPerType {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "too_many_categories"})
		return
	}

	if err := saveUserCategory(database.DB, userID, entryType, name, false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_category"})
		return
	}
	c.JSON(http.StatusCreated, CustomCategoryResponse{Name: name, Type: entryType})
}

// removeCustomCategory takes a category out of the user's picker. Entries
// that use it keep it: this changes what is offered, not what was recorded.
func (s *Server) removeCustomCategory(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	name, entryType, fields := readCustomCategory(c.Query("name"), c.Query("type"))
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_category", "fields": fields})
		return
	}
	if err := saveUserCategory(database.DB, userID, entryType, name, true); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_remove_category"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": true})
}

// saveUserCategory writes the row for one name, creating it or flipping
// whether it is removed. The name's spelling follows the latest write.
func saveUserCategory(db *gorm.DB, userID uint, entryType, name string, removed bool) error {
	row := models.UserCategory{
		UserID:  userID,
		Type:    entryType,
		Name:    name,
		NameKey: strings.ToLower(name),
		Removed: removed,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "type"}, {Name: "name_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"name":       name,
			"removed":    removed,
			"updated_at": time.Now().UTC(),
		}),
	}).Create(&row).Error
}
