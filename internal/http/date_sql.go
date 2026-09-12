package http

import "gorm.io/gorm"

// sqlDateDay and sqlDateMonth keep aggregate projections portable. PostgreSQL
// DATE does not support text functions directly, while SQLite's DATE affinity
// may scan as an RFC3339 timestamp. Both helpers return the unchanged calendar
// value expected by the API.
func sqlDateDay(db *gorm.DB, column string) string {
	if db.Dialector.Name() == "postgres" {
		return "TO_CHAR(" + column + ", 'YYYY-MM-DD')"
	}
	return "SUBSTR(CAST(" + column + " AS TEXT), 1, 10)"
}

func sqlDateMonth(db *gorm.DB, column string) string {
	if db.Dialector.Name() == "postgres" {
		return "TO_CHAR(" + column + ", 'YYYY-MM')"
	}
	return "SUBSTR(CAST(" + column + " AS TEXT), 1, 7)"
}
