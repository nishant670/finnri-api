package database

import (
	"strings"
	"testing"
)

func TestRuntimeSchemaIncludesDataIntegrityConstraints(t *testing.T) {
	joined := strings.Join(runtimeSchemaStatements(), "\n")
	required := []string{
		"entries_amount_positive_check",
		"entries_type_check",
		"entries_source_check",
		"accounts_type_check",
		"accounts_credit_limit_non_negative_check",
		"pg_constraint",
		"to_regclass('accounts_user_id_id_unique') IS NULL",
		"fk_entries_owned_account",
		"FOREIGN KEY (user_id, account_id) REFERENCES accounts(user_id, id)",
		"split_bills_total_amount_positive_check",
		"split_participants_direction_check",
		"split_settlements_direction_check",
		"CREATE TABLE IF NOT EXISTS split_groups",
		"idx_split_group_members_unique",
		"INSERT INTO accounts",
		"SET source = 'manual'",
		"CREATE TABLE IF NOT EXISTS plans",
		"CREATE TABLE IF NOT EXISTS credit_grants",
		"CREATE TABLE IF NOT EXISTS ai_usage_events",
		"CREATE TABLE IF NOT EXISTS credit_ledger",
		"CREATE TABLE IF NOT EXISTS daily_credit_usage",
		"CREATE TABLE IF NOT EXISTS guest_usage_keys",
		"CREATE TABLE IF NOT EXISTS lifetime_quote_requests",
		"CREATE TABLE IF NOT EXISTS ai_usage_limit_events",
		"CREATE TABLE IF NOT EXISTS ai_model_pricings",
		"CREATE TABLE IF NOT EXISTS ai_abuse_blocks",
		"credit_grants_amount_check",
		"credits_remaining >= 0",
		"idx_credit_grants_once_user_free_trial",
		"idx_credit_grants_once_guest_free_trial",
		"idx_credit_grants_subscription_period",
		"idx_ai_usage_events_user_idempotency",
		"idx_credit_ledger_grant_idempotency",
		"idx_daily_credit_usage_user_date",
		"ai_usage_events_status_check",
		"lifetime_quote_requests_status_check",
		"ai_usage_limit_events_reason_check",
		"ai_model_pricings_operation_check",
		"ai_abuse_blocks_scope_check",
	}
	for _, fragment := range required {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("runtime schema is missing %q", fragment)
		}
	}

	// The inverse of the old rule. An entry with no account is legitimate —
	// capture is never blocked on account setup — so the runtime schema must
	// drop the constraint on databases that predate the change, and must not
	// backfill unlinked rows onto the default account, which would erase the
	// state the "Unlinked" affordance and the review queue are built on.
	if strings.Contains(joined, "ALTER COLUMN account_id SET NOT NULL") {
		t.Fatal("runtime schema must not force entries.account_id to be non-null")
	}
	if strings.Contains(joined, "AND entries.account_id IS NULL") {
		t.Fatal("runtime schema must not backfill deliberately unlinked entries")
	}
	if !strings.Contains(joined, "ALTER COLUMN account_id DROP NOT NULL") {
		t.Fatal("runtime schema must drop a legacy entries.account_id NOT NULL constraint")
	}
}

func TestEntryDateMigrationAuditsMalformedLegacyValues(t *testing.T) {
	joined := strings.Join(preAutoMigrateStatements(), "\n")
	for _, required := range []string{
		"entry_date_migration_rejects",
		"original_date",
		"AT TIME ZONE 'Asia/Kolkata'",
		"ALTER COLUMN date TYPE DATE",
		"USING BTRIM(date)::DATE",
		"ALTER COLUMN date SET NOT NULL",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("entry date migration missing %q", required)
		}
	}
}

func TestLegacyConstraintRenameIsSafeOnFreshAndExistingDatabases(t *testing.T) {
	joined := strings.Join(legacyConstraintRenames(), "\n")
	for _, required := range []string{
		"RENAME CONSTRAINT plans_code_key TO uni_plans_code",
		"c.conname = 'plans_code_key'",
		"c.conname = 'uni_plans_code'",
		"AND NOT EXISTS",
		"JOIN pg_class",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("legacy constraint rename missing %q", required)
		}
	}
	if strings.Contains(joined, "::regclass") {
		t.Fatal("legacy constraint rename must not cast a possibly missing table to regclass")
	}
}
