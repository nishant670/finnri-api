package database

// PrepareForAutoMigrate reconciles constraint names between the two mechanisms
// that build this schema, and must run before AutoMigrate.
//
// Tables created by the raw SQL below declare uniqueness inline — `code
// VARCHAR(64) NOT NULL UNIQUE` — which Postgres names for itself:
// `plans_code_key`. The GORM models declare the same uniqueness with
// `uniqueIndex`, which GORM names `uni_plans_code`. Neither side is wrong on
// its own; they simply do not recognise each other's work.
//
// AutoMigrate reconciles the difference by issuing
// `ALTER TABLE plans DROP CONSTRAINT uni_plans_code` for a constraint that has
// never existed, and a failed migration is fatal at boot. That is exactly what
// took production down when `models.Payment` — which reaches `plans` through
// its `Plan` association — entered the AutoMigrate set for the first time.
//
// Renaming is deliberate: the constraint, and the unique index behind it, are
// carried across intact. Dropping and recreating would open a window in which
// two rows could take the same plan code.
func PrepareForAutoMigrate() error {
	for _, statement := range legacyConstraintRenames() {
		if err := DB.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

// renameUniqueConstraint builds an idempotent rename: it acts only when the
// Postgres-named constraint is present and the GORM-named one is not, so it is
// a no-op on a database built entirely by AutoMigrate and on every boot after
// the first.
//
// Constraints are looked up by joining pg_class rather than casting to
// regclass. SQL does not promise to short-circuit AND, so a `to_regclass(...)
// IS NOT NULL` guard sitting beside a `'table'::regclass` cast does not stop
// the cast from throwing — which is how the first version of this broke every
// fresh database, where `plans` does not exist until AutoMigrate creates it.
func renameUniqueConstraint(table, from, to string) string {
	constraintExists := func(name string) string {
		return `EXISTS (
				SELECT 1
				FROM pg_constraint c
				JOIN pg_class t ON t.oid = c.conrelid
				JOIN pg_namespace n ON n.oid = t.relnamespace
				WHERE n.nspname = 'public'
					AND t.relname = '` + table + `'
					AND c.conname = '` + name + `'
			)`
	}
	return `DO $$
	BEGIN
		IF ` + constraintExists(from) + ` AND NOT ` + constraintExists(to) + ` THEN
			ALTER TABLE ` + table + ` RENAME CONSTRAINT ` + from + ` TO ` + to + `;
		END IF;
	END
	$$`
}

func legacyConstraintRenames() []string {
	return []string{
		// plans.code — reached by AutoMigrate through models.Payment.Plan and
		// models.UserSubscription.Plan.
		renameUniqueConstraint("plans", "plans_code_key", "uni_plans_code"),
	}
}

func EnsureRuntimeSchema() error {
	for _, statement := range runtimeSchemaStatements() {
		if err := DB.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func runtimeSchemaStatements() []string {
	return []string{
		// Must run before any composite FK that references accounts(user_id, id):
		// Postgres requires the unique constraint to exist first.
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_constraint
				WHERE conrelid = 'accounts'::regclass
					AND conname = 'accounts_user_id_id_unique'
			) AND to_regclass('accounts_user_id_id_unique') IS NULL THEN
				ALTER TABLE accounts
					ADD CONSTRAINT accounts_user_id_id_unique UNIQUE (user_id, id);
			END IF;
		EXCEPTION
			WHEN duplicate_object OR duplicate_table THEN NULL;
		END
		$$`,
		`ALTER TABLE users
			ADD COLUMN IF NOT EXISTS failed_login_attempts INTEGER NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS login_locked_until TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS google_subject VARCHAR(255),
			ADD COLUMN IF NOT EXISTS converted_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ`,
		`ALTER TABLE auth_sessions
			ADD COLUMN IF NOT EXISTS kind VARCHAR(16) NOT NULL DEFAULT 'user'`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_kind ON auth_sessions (kind)`,
		`CREATE INDEX IF NOT EXISTS idx_users_last_active_at ON users (last_active_at)`,
		`CREATE INDEX IF NOT EXISTS idx_users_converted_at ON users (converted_at)`,
		`CREATE TABLE IF NOT EXISTS admin_users (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
			role VARCHAR(24) NOT NULL DEFAULT 'viewer',
			disabled_at TIMESTAMPTZ,
			created_by VARCHAR(120) NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_users_role
			ON admin_users (role) WHERE disabled_at IS NULL`,
		`ALTER TABLE admin_users DROP CONSTRAINT IF EXISTS admin_users_role_check`,
		`ALTER TABLE admin_users ADD CONSTRAINT admin_users_role_check
			CHECK (role IN ('viewer', 'support', 'owner'))`,
		`CREATE TABLE IF NOT EXISTS admin_audit_log (
			id BIGSERIAL PRIMARY KEY,
			admin_user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			actor VARCHAR(40) NOT NULL DEFAULT 'admin_user',
			action VARCHAR(80) NOT NULL,
			subject_type VARCHAR(40) NOT NULL DEFAULT '',
			subject_id VARCHAR(120) NOT NULL DEFAULT '',
			payload JSONB NOT NULL DEFAULT '{}'::jsonb,
			ip_hash CHAR(64) NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE admin_audit_log
			ALTER COLUMN admin_user_id DROP NOT NULL`,
		`ALTER TABLE admin_audit_log
			ADD COLUMN IF NOT EXISTS actor VARCHAR(40) NOT NULL DEFAULT 'admin_user'`,
		`CREATE INDEX IF NOT EXISTS idx_admin_audit_log_actor ON admin_audit_log (actor)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_audit_log_created ON admin_audit_log (created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_audit_log_admin ON admin_audit_log (admin_user_id, created_at DESC)`,
		`DROP INDEX IF EXISTS idx_users_device_id`,
		`CREATE INDEX IF NOT EXISTS idx_users_device_id
			ON users (device_id)
			WHERE device_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_guest_device_id
			ON users (device_id)
			WHERE device_id IS NOT NULL AND is_guest = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_users_login_locked_until
			ON users (login_locked_until)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_subject
			ON users (google_subject)
			WHERE google_subject IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user_read_created
			ON notifications (user_id, read_at, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user_created
			ON notifications (user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS feedback (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			type VARCHAR(32) NOT NULL,
			area VARCHAR(64) NOT NULL DEFAULT '',
			title VARCHAR(140) NOT NULL,
			message TEXT NOT NULL,
			impact VARCHAR(32) NOT NULL DEFAULT 'nice_to_have',
			status VARCHAR(32) NOT NULL DEFAULT 'new',
			admin_notes TEXT NOT NULL DEFAULT '',
			resolved_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE feedback
			ADD COLUMN IF NOT EXISTS admin_notes TEXT NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS resolved_by BIGINT REFERENCES users(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_feedback_user_created
			ON feedback (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_feedback_status_created
			ON feedback (status, created_at DESC)`,
		`ALTER TABLE feedback
			DROP CONSTRAINT IF EXISTS feedback_type_check`,
		`ALTER TABLE feedback
			ADD CONSTRAINT feedback_type_check
			CHECK (type IN ('bug', 'idea', 'improvement', 'feature_request', 'other'))`,
		`ALTER TABLE feedback
			DROP CONSTRAINT IF EXISTS feedback_impact_check`,
		`ALTER TABLE feedback
			ADD CONSTRAINT feedback_impact_check
			CHECK (impact IN ('critical', 'high', 'medium', 'nice_to_have'))`,
		`ALTER TABLE feedback
			DROP CONSTRAINT IF EXISTS feedback_status_check`,
		`UPDATE feedback SET status = 'triaged' WHERE status = 'reviewing'`,
		`UPDATE feedback SET status = 'declined' WHERE status = 'closed'`,
		`ALTER TABLE feedback
			ADD CONSTRAINT feedback_status_check
			CHECK (status IN ('new', 'triaged', 'planned', 'shipped', 'declined'))`,
		`CREATE TABLE IF NOT EXISTS budgets (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name VARCHAR(120) NOT NULL,
			period VARCHAR(16) NOT NULL DEFAULT 'monthly',
			category VARCHAR(80) NOT NULL DEFAULT '',
			limit_amount NUMERIC(19,2) NOT NULL,
			currency CHAR(3) NOT NULL DEFAULT 'INR',
			alert_threshold_percent INTEGER NOT NULL DEFAULT 80,
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_budgets_user_active
			ON budgets (user_id, active, period)`,
		`CREATE INDEX IF NOT EXISTS idx_budgets_user_category
			ON budgets (user_id, category)`,
		`ALTER TABLE budgets
			DROP CONSTRAINT IF EXISTS budgets_period_check`,
		`ALTER TABLE budgets
			ADD CONSTRAINT budgets_period_check CHECK (period = 'monthly')`,
		`ALTER TABLE budgets
			DROP CONSTRAINT IF EXISTS budgets_limit_amount_positive_check`,
		`ALTER TABLE budgets
			ADD CONSTRAINT budgets_limit_amount_positive_check CHECK (limit_amount > 0)`,
		`ALTER TABLE budgets
			DROP CONSTRAINT IF EXISTS budgets_currency_check`,
		`ALTER TABLE budgets
			ADD CONSTRAINT budgets_currency_check CHECK (currency = 'INR')`,
		`ALTER TABLE budgets
			DROP CONSTRAINT IF EXISTS budgets_alert_threshold_percent_check`,
		`ALTER TABLE budgets
			ADD CONSTRAINT budgets_alert_threshold_percent_check
			CHECK (alert_threshold_percent BETWEEN 1 AND 100)`,
		`CREATE TABLE IF NOT EXISTS budget_alerts (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			budget_id BIGINT NOT NULL REFERENCES budgets(id) ON DELETE CASCADE,
			period_start DATE NOT NULL,
			kind VARCHAR(24) NOT NULL,
			spend_amount NUMERIC(19,2) NOT NULL,
			limit_amount NUMERIC(19,2) NOT NULL,
			notification_id BIGINT REFERENCES notifications(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_budget_alert_once_period
			ON budget_alerts (user_id, budget_id, period_start, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_budget_alerts_user_budget
			ON budget_alerts (user_id, budget_id)`,
		`ALTER TABLE budget_alerts
			DROP CONSTRAINT IF EXISTS budget_alerts_kind_check`,
		`ALTER TABLE budget_alerts
			ADD CONSTRAINT budget_alerts_kind_check CHECK (kind IN ('warning', 'exceeded'))`,
		`ALTER TABLE budget_alerts
			DROP CONSTRAINT IF EXISTS budget_alerts_spend_amount_non_negative_check`,
		`ALTER TABLE budget_alerts
			ADD CONSTRAINT budget_alerts_spend_amount_non_negative_check CHECK (spend_amount >= 0)`,
		`ALTER TABLE budget_alerts
			DROP CONSTRAINT IF EXISTS budget_alerts_limit_amount_positive_check`,
		`ALTER TABLE budget_alerts
			ADD CONSTRAINT budget_alerts_limit_amount_positive_check CHECK (limit_amount > 0)`,
		`CREATE TABLE IF NOT EXISTS subscriptions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			account_id BIGINT,
			name VARCHAR(120) NOT NULL,
			merchant VARCHAR(120) NOT NULL DEFAULT '',
			category VARCHAR(80) NOT NULL DEFAULT '',
			amount NUMERIC(19,2) NOT NULL,
			currency CHAR(3) NOT NULL DEFAULT 'INR',
			billing_interval VARCHAR(16) NOT NULL DEFAULT 'monthly',
			next_due_date DATE NOT NULL,
			last_charged_date VARCHAR(10) NOT NULL DEFAULT '',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			reminder_days INTEGER NOT NULL DEFAULT 3,
			cancel_before_due BOOLEAN NOT NULL DEFAULT FALSE,
			notes TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_subscriptions_user_status_due
			ON subscriptions (user_id, status, next_due_date)`,
		`CREATE INDEX IF NOT EXISTS idx_subscriptions_user_merchant
			ON subscriptions (user_id, merchant)`,
		`CREATE INDEX IF NOT EXISTS idx_subscriptions_user_category
			ON subscriptions (user_id, category)`,
		`ALTER TABLE subscriptions
			ADD COLUMN IF NOT EXISTS cancel_before_due BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE subscriptions
			ADD COLUMN IF NOT EXISTS cancel_on_date VARCHAR(10) NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS autopay BOOLEAN NOT NULL DEFAULT FALSE,
			ADD COLUMN IF NOT EXISTS payment_mode VARCHAR(24) NOT NULL DEFAULT 'Cash',
			ADD COLUMN IF NOT EXISTS transaction_tag VARCHAR(40) NOT NULL DEFAULT 'Subscription',
			ADD COLUMN IF NOT EXISTS purpose_type VARCHAR(40) NOT NULL DEFAULT 'normal_spend'`,
		`CREATE INDEX IF NOT EXISTS idx_subscriptions_autopay_due
			ON subscriptions (status, autopay, next_due_date)`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS subscriptions_amount_positive_check`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT subscriptions_amount_positive_check CHECK (amount > 0)`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS subscriptions_currency_check`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT subscriptions_currency_check CHECK (currency = 'INR')`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS subscriptions_billing_interval_check`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT subscriptions_billing_interval_check
			CHECK (billing_interval IN ('daily', 'business_daily', 'weekly', 'biweekly', 'monthly', 'quarterly', 'yearly'))`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS subscriptions_status_check`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT subscriptions_status_check CHECK (status IN ('active', 'paused', 'cancelled'))`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS subscriptions_reminder_days_check`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT subscriptions_reminder_days_check CHECK (reminder_days BETWEEN 0 AND 30)`,
		`ALTER TABLE subscriptions
			DROP CONSTRAINT IF EXISTS fk_subscriptions_owned_account`,
		`ALTER TABLE subscriptions
			ADD CONSTRAINT fk_subscriptions_owned_account
			FOREIGN KEY (user_id, account_id) REFERENCES accounts(user_id, id)`,
		`CREATE TABLE IF NOT EXISTS subscription_reminders (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			subscription_id BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
			due_date DATE NOT NULL,
			kind VARCHAR(24) NOT NULL,
			notification_id BIGINT REFERENCES notifications(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_subscription_reminder_once_due
			ON subscription_reminders (user_id, subscription_id, due_date, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_subscription_reminders_user_subscription
			ON subscription_reminders (user_id, subscription_id)`,
		`ALTER TABLE subscription_reminders
			DROP CONSTRAINT IF EXISTS subscription_reminders_kind_check`,
		`ALTER TABLE subscription_reminders
			ADD CONSTRAINT subscription_reminders_kind_check CHECK (kind IN ('due', 'overdue', 'cancel'))`,
		`CREATE TABLE IF NOT EXISTS subscription_occurrences (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			subscription_id BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
			entry_id BIGINT NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
			due_date DATE NOT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			confirmed_at TIMESTAMPTZ,
			reverted_at TIMESTAMPTZ,
			notification_id BIGINT REFERENCES notifications(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_subscription_occurrence_once
			ON subscription_occurrences (user_id, subscription_id, due_date)`,
		`CREATE INDEX IF NOT EXISTS idx_subscription_occurrences_user_status
			ON subscription_occurrences (user_id, status, created_at DESC)`,
		`ALTER TABLE subscription_occurrences
			DROP CONSTRAINT IF EXISTS subscription_occurrences_status_check`,
		`ALTER TABLE subscription_occurrences
			ADD CONSTRAINT subscription_occurrences_status_check
			CHECK (status IN ('pending', 'confirmed', 'reverted'))`,
		`CREATE TABLE IF NOT EXISTS recurring_candidate_decisions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			candidate_key VARCHAR(180) NOT NULL,
			merchant VARCHAR(120) NOT NULL DEFAULT '',
			category VARCHAR(80) NOT NULL DEFAULT '',
			decision VARCHAR(24) NOT NULL,
			snoozed_until DATE,
			last_reviewed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_recurring_decision_user_key
			ON recurring_candidate_decisions (user_id, candidate_key)`,
		`CREATE INDEX IF NOT EXISTS idx_recurring_decision_user_decision
			ON recurring_candidate_decisions (user_id, decision)`,
		`ALTER TABLE recurring_candidate_decisions
			DROP CONSTRAINT IF EXISTS recurring_candidate_decisions_decision_check`,
		`ALTER TABLE recurring_candidate_decisions
			ADD CONSTRAINT recurring_candidate_decisions_decision_check
			CHECK (decision IN ('dismissed', 'snoozed', 'tracked'))`,
		`CREATE TABLE IF NOT EXISTS push_devices (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token VARCHAR(255) NOT NULL UNIQUE,
			platform VARCHAR(16) NOT NULL,
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_push_devices_user_active
			ON push_devices (user_id, active)`,
		`CREATE TABLE IF NOT EXISTS plans (
			id BIGSERIAL PRIMARY KEY,
			code VARCHAR(64) NOT NULL UNIQUE,
			name VARCHAR(120) NOT NULL,
			billing_interval VARCHAR(24) NOT NULL,
			price_minor BIGINT NOT NULL DEFAULT 0,
			list_price_minor BIGINT NOT NULL DEFAULT 0,
			currency CHAR(3) NOT NULL DEFAULT 'INR',
			included_credits INTEGER NOT NULL DEFAULT 0,
			daily_credit_limit INTEGER NOT NULL DEFAULT 0,
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			requires_login BOOLEAN NOT NULL DEFAULT TRUE,
			requires_prior_paid_months INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE plans ADD COLUMN IF NOT EXISTS list_price_minor BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE plans
			DROP CONSTRAINT IF EXISTS plans_billing_interval_check`,
		`ALTER TABLE plans
			ADD CONSTRAINT plans_billing_interval_check
			CHECK (billing_interval IN ('weekly', 'monthly', 'quarterly', 'yearly', 'lifetime_quote'))`,
		`ALTER TABLE plans
			DROP CONSTRAINT IF EXISTS plans_price_minor_non_negative_check`,
		`ALTER TABLE plans
			ADD CONSTRAINT plans_price_minor_non_negative_check CHECK (price_minor >= 0)`,
		`ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_list_price_minor_non_negative_check`,
		`ALTER TABLE plans ADD CONSTRAINT plans_list_price_minor_non_negative_check CHECK (list_price_minor >= 0)`,
		`INSERT INTO plans (code, name, billing_interval, price_minor, list_price_minor, currency, included_credits, daily_credit_limit, is_public, requires_login, requires_prior_paid_months)
			VALUES
			('weekly_pass', 'Weekly Pass', 'weekly', 7900, 19900, 'INR', 800, 200, TRUE, TRUE, 0),
			('monthly', 'Monthly', 'monthly', 14900, 49900, 'INR', 3600, 250, TRUE, TRUE, 0),
			('quarterly', 'Quarterly', 'quarterly', 32900, 129900, 'INR', 11000, 300, TRUE, TRUE, 0),
			('yearly', 'Yearly', 'yearly', 79900, 399900, 'INR', 48000, 350, TRUE, TRUE, 0),
			('lifetime_quote', 'Lifetime Quote', 'lifetime_quote', 499900, 1999900, 'INR', 5000, 500, TRUE, TRUE, 3)
			ON CONFLICT (code) DO UPDATE SET
				name = EXCLUDED.name,
				billing_interval = EXCLUDED.billing_interval,
				price_minor = CASE WHEN plans.price_minor = 0 THEN EXCLUDED.price_minor ELSE plans.price_minor END,
				list_price_minor = CASE WHEN plans.list_price_minor = 0 THEN EXCLUDED.list_price_minor ELSE plans.list_price_minor END,
				included_credits = CASE WHEN plans.included_credits IN (0, 700, 3000, 10000, 45000) THEN EXCLUDED.included_credits ELSE plans.included_credits END,
				daily_credit_limit = CASE WHEN plans.daily_credit_limit IN (0, 150, 200, 250, 300) THEN EXCLUDED.daily_credit_limit ELSE plans.daily_credit_limit END,
				is_public = TRUE,
				updated_at = NOW()`,
		`CREATE TABLE IF NOT EXISTS admin_daily_metrics (
			id BIGSERIAL PRIMARY KEY,
			metric_date DATE NOT NULL UNIQUE,
			active_users INTEGER NOT NULL DEFAULT 0,
			ai_events INTEGER NOT NULL DEFAULT 0,
			ai_credits INTEGER NOT NULL DEFAULT 0,
			ai_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
			successful_ai_events INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE plans
			DROP CONSTRAINT IF EXISTS plans_currency_check`,
		`ALTER TABLE plans
			ADD CONSTRAINT plans_currency_check CHECK (currency = 'INR')`,
		`ALTER TABLE plans
			DROP CONSTRAINT IF EXISTS plans_credit_limits_non_negative_check`,
		`ALTER TABLE plans
			ADD CONSTRAINT plans_credit_limits_non_negative_check
			CHECK (included_credits >= 0 AND daily_credit_limit >= 0 AND requires_prior_paid_months >= 0)`,
		`CREATE TABLE IF NOT EXISTS user_subscriptions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			plan_id BIGINT NOT NULL REFERENCES plans(id),
			status VARCHAR(24) NOT NULL,
			current_period_start TIMESTAMPTZ NOT NULL,
			current_period_end TIMESTAMPTZ NOT NULL,
			provider VARCHAR(40) NOT NULL DEFAULT '',
			provider_customer_id VARCHAR(120) NOT NULL DEFAULT '',
			provider_subscription_id VARCHAR(120) NOT NULL DEFAULT '',
			cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_subscriptions_user_status_period
			ON user_subscriptions (user_id, status, current_period_end DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_user_subscriptions_plan
			ON user_subscriptions (plan_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_subscriptions_provider_customer
			ON user_subscriptions (provider, provider_customer_id)
			WHERE provider_customer_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_subscriptions_provider_subscription
			ON user_subscriptions (provider, provider_subscription_id)
			WHERE provider_subscription_id <> ''`,
		`ALTER TABLE user_subscriptions
			DROP CONSTRAINT IF EXISTS user_subscriptions_status_check`,
		`ALTER TABLE user_subscriptions
			ADD CONSTRAINT user_subscriptions_status_check
			CHECK (status IN ('trialing', 'active', 'past_due', 'cancelled', 'expired'))`,
		`ALTER TABLE user_subscriptions
			DROP CONSTRAINT IF EXISTS user_subscriptions_period_check`,
		`ALTER TABLE user_subscriptions
			ADD CONSTRAINT user_subscriptions_period_check CHECK (current_period_end > current_period_start)`,
		// Payments and the webhook ledger. A signed, deduplicated webhook is the
		// only thing permitted to move a subscription to active or write a
		// credit grant, so these two tables are the whole trust boundary
		// between Razorpay and the ledger.
		`CREATE TABLE IF NOT EXISTS payments (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			plan_id BIGINT NOT NULL REFERENCES plans(id),
			provider VARCHAR(40) NOT NULL,
			provider_order_id VARCHAR(120) NOT NULL UNIQUE,
			provider_payment_id VARCHAR(120) NOT NULL DEFAULT '',
			status VARCHAR(24) NOT NULL,
			amount_minor BIGINT NOT NULL,
			amount_refunded_minor BIGINT NOT NULL DEFAULT 0,
			currency CHAR(3) NOT NULL DEFAULT 'INR',
			method VARCHAR(40) NOT NULL DEFAULT '',
			receipt VARCHAR(64) NOT NULL DEFAULT '',
			failure_reason VARCHAR(255) NOT NULL DEFAULT '',
			subscription_id BIGINT REFERENCES user_subscriptions(id) ON DELETE SET NULL,
			captured_at TIMESTAMPTZ,
			refunded_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payments_user_created ON payments (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payments_status ON payments (status)`,
		`CREATE INDEX IF NOT EXISTS idx_payments_provider_payment
			ON payments (provider, provider_payment_id) WHERE provider_payment_id <> ''`,
		`ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_status_check`,
		`ALTER TABLE payments ADD CONSTRAINT payments_status_check
			CHECK (status IN ('created', 'captured', 'failed', 'refunded'))`,
		`ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_amount_check`,
		// A captured payment can be refunded in part, but never for more than
		// was taken - that would be money invented by an arithmetic slip.
		`ALTER TABLE payments ADD CONSTRAINT payments_amount_check
			CHECK (amount_minor > 0 AND amount_refunded_minor >= 0 AND amount_refunded_minor <= amount_minor)`,
		`CREATE TABLE IF NOT EXISTS payment_webhook_events (
			id BIGSERIAL PRIMARY KEY,
			provider VARCHAR(40) NOT NULL,
			event_id VARCHAR(160) NOT NULL,
			event_type VARCHAR(80) NOT NULL DEFAULT '',
			payload_hash CHAR(64) NOT NULL DEFAULT '',
			status VARCHAR(24) NOT NULL,
			detail VARCHAR(255) NOT NULL DEFAULT '',
			processed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// The unique key is the replay guard. Inserting it before the event is
		// handled means a concurrent redelivery loses the insert and returns
		// without granting anything a second time.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_webhook_events_identity
			ON payment_webhook_events (provider, event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_webhook_events_status_created
			ON payment_webhook_events (status, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS credit_grants (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			source VARCHAR(40) NOT NULL,
			credits_granted INTEGER NOT NULL,
			credits_remaining INTEGER NOT NULL,
			valid_from TIMESTAMPTZ NOT NULL,
			expires_at TIMESTAMPTZ,
			subscription_id BIGINT REFERENCES user_subscriptions(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_credit_grants_user_available
			ON credit_grants (user_id, expires_at, valid_from)
			WHERE user_id IS NOT NULL AND credits_remaining > 0`,
		`CREATE INDEX IF NOT EXISTS idx_credit_grants_guest_available
			ON credit_grants (guest_device_id_hash, expires_at, valid_from)
			WHERE guest_device_id_hash <> '' AND credits_remaining > 0`,
		`CREATE INDEX IF NOT EXISTS idx_credit_grants_subscription
			ON credit_grants (subscription_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_grants_once_user_free_trial
			ON credit_grants (user_id, source)
			WHERE user_id IS NOT NULL AND source = 'free_trial'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_grants_once_guest_free_trial
			ON credit_grants (guest_device_id_hash, source)
			WHERE guest_device_id_hash <> '' AND source = 'free_trial'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_grants_subscription_period
			ON credit_grants (subscription_id, valid_from, source)
			WHERE subscription_id IS NOT NULL AND source = 'subscription_period'`,
		`ALTER TABLE credit_grants
			DROP CONSTRAINT IF EXISTS credit_grants_identity_check`,
		`ALTER TABLE credit_grants
			ADD CONSTRAINT credit_grants_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE credit_grants
			DROP CONSTRAINT IF EXISTS credit_grants_source_check`,
		`ALTER TABLE credit_grants
			ADD CONSTRAINT credit_grants_source_check
			CHECK (source IN ('free_trial', 'subscription_period', 'manual_adjustment', 'refund', 'promo', 'lifetime_quote'))`,
		`ALTER TABLE credit_grants
			DROP CONSTRAINT IF EXISTS credit_grants_amount_check`,
		`ALTER TABLE credit_grants
			ADD CONSTRAINT credit_grants_amount_check
			CHECK (credits_granted > 0 AND credits_remaining >= 0 AND credits_remaining <= credits_granted)`,
		`ALTER TABLE credit_grants
			DROP CONSTRAINT IF EXISTS credit_grants_expiry_check`,
		`ALTER TABLE credit_grants
			ADD CONSTRAINT credit_grants_expiry_check
			CHECK (expires_at IS NULL OR expires_at > valid_from)`,
		`CREATE TABLE IF NOT EXISTS ai_usage_events (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			session_id VARCHAR(120) NOT NULL DEFAULT '',
			request_id VARCHAR(120) NOT NULL UNIQUE,
			idempotency_key VARCHAR(120) NOT NULL DEFAULT '',
			action_code VARCHAR(80) NOT NULL,
			input_kind VARCHAR(20) NOT NULL,
			status VARCHAR(32) NOT NULL,
			provider VARCHAR(40) NOT NULL DEFAULT '',
			model VARCHAR(80) NOT NULL DEFAULT '',
			secondary_provider VARCHAR(40) NOT NULL DEFAULT '',
			secondary_model VARCHAR(80) NOT NULL DEFAULT '',
			estimated_credits INTEGER NOT NULL,
			reserved_credits INTEGER NOT NULL DEFAULT 0,
			final_credits INTEGER NOT NULL DEFAULT 0,
			estimated_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
			actual_cost_usd_micros BIGINT,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			total_tokens INTEGER,
			audio_duration_ms INTEGER,
			audio_bytes BIGINT,
			input_chars INTEGER,
			response_bytes INTEGER,
			error_code VARCHAR(80) NOT NULL DEFAULT '',
			started_at TIMESTAMPTZ NOT NULL,
			provider_started_at TIMESTAMPTZ,
			finished_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_events_user_started
			ON ai_usage_events (user_id, started_at DESC)
			WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_events_guest_started
			ON ai_usage_events (guest_device_id_hash, started_at DESC)
			WHERE guest_device_id_hash <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_events_action_status
			ON ai_usage_events (action_code, status, started_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_usage_events_user_idempotency
			ON ai_usage_events (user_id, idempotency_key)
			WHERE user_id IS NOT NULL AND idempotency_key <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_usage_events_guest_idempotency
			ON ai_usage_events (guest_device_id_hash, idempotency_key)
			WHERE guest_device_id_hash <> '' AND idempotency_key <> ''`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_identity_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_input_kind_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_input_kind_check
			CHECK (input_kind IN ('text', 'voice', 'image', 'file', 'chat'))`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_status_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_status_check
			CHECK (status IN ('reserved', 'succeeded', 'failed_before_provider', 'failed_after_provider', 'refunded', 'cancelled'))`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_credit_non_negative_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_credit_non_negative_check
			CHECK (estimated_credits >= 0 AND reserved_credits >= 0 AND final_credits >= 0)`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_cost_non_negative_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_cost_non_negative_check
			CHECK (estimated_cost_usd_micros >= 0 AND (actual_cost_usd_micros IS NULL OR actual_cost_usd_micros >= 0))`,
		`ALTER TABLE ai_usage_events
			DROP CONSTRAINT IF EXISTS ai_usage_events_usage_non_negative_check`,
		`ALTER TABLE ai_usage_events
			ADD CONSTRAINT ai_usage_events_usage_non_negative_check
			CHECK (
				(prompt_tokens IS NULL OR prompt_tokens >= 0)
				AND (completion_tokens IS NULL OR completion_tokens >= 0)
				AND (total_tokens IS NULL OR total_tokens >= 0)
				AND (audio_duration_ms IS NULL OR audio_duration_ms >= 0)
				AND (audio_bytes IS NULL OR audio_bytes >= 0)
				AND (input_chars IS NULL OR input_chars >= 0)
				AND (response_bytes IS NULL OR response_bytes >= 0)
			)`,
		`CREATE TABLE IF NOT EXISTS credit_ledger (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			grant_id BIGINT REFERENCES credit_grants(id) ON DELETE SET NULL,
			direction VARCHAR(20) NOT NULL,
			credits INTEGER NOT NULL,
			balance_after INTEGER NOT NULL,
			reason_code VARCHAR(64) NOT NULL,
			idempotency_key VARCHAR(120) NOT NULL DEFAULT '',
			ai_usage_event_id BIGINT REFERENCES ai_usage_events(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_credit_ledger_user_created
			ON credit_ledger (user_id, created_at DESC)
			WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_credit_ledger_guest_created
			ON credit_ledger (guest_device_id_hash, created_at DESC)
			WHERE guest_device_id_hash <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_credit_ledger_grant
			ON credit_ledger (grant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_credit_ledger_ai_usage_event
			ON credit_ledger (ai_usage_event_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_ledger_grant_idempotency
			ON credit_ledger (grant_id, idempotency_key, direction)
			WHERE grant_id IS NOT NULL AND idempotency_key <> ''`,
		`ALTER TABLE credit_ledger
			DROP CONSTRAINT IF EXISTS credit_ledger_identity_check`,
		`ALTER TABLE credit_ledger
			ADD CONSTRAINT credit_ledger_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE credit_ledger
			DROP CONSTRAINT IF EXISTS credit_ledger_direction_check`,
		`ALTER TABLE credit_ledger
			ADD CONSTRAINT credit_ledger_direction_check
			CHECK (direction IN ('grant', 'debit', 'refund', 'adjustment', 'expiry'))`,
		`ALTER TABLE credit_ledger
			DROP CONSTRAINT IF EXISTS credit_ledger_amount_check`,
		`ALTER TABLE credit_ledger
			ADD CONSTRAINT credit_ledger_amount_check
			CHECK (credits > 0 AND balance_after >= 0)`,
		`CREATE TABLE IF NOT EXISTS daily_credit_usage (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			usage_date DATE NOT NULL,
			credits_used INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_daily_credit_usage_user_date
			ON daily_credit_usage (user_id, usage_date)
			WHERE user_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_daily_credit_usage_guest_date
			ON daily_credit_usage (guest_device_id_hash, usage_date)
			WHERE guest_device_id_hash <> ''`,
		`ALTER TABLE daily_credit_usage
			DROP CONSTRAINT IF EXISTS daily_credit_usage_identity_check`,
		`ALTER TABLE daily_credit_usage
			ADD CONSTRAINT daily_credit_usage_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE daily_credit_usage
			DROP CONSTRAINT IF EXISTS daily_credit_usage_amount_check`,
		`ALTER TABLE daily_credit_usage
			ADD CONSTRAINT daily_credit_usage_amount_check CHECK (credits_used >= 0)`,
		`CREATE TABLE IF NOT EXISTS guest_usage_keys (
			id BIGSERIAL PRIMARY KEY,
			guest_device_id_hash CHAR(64) NOT NULL UNIQUE,
			ip_hash CHAR(64) NOT NULL,
			first_seen_at TIMESTAMPTZ NOT NULL,
			last_seen_at TIMESTAMPTZ NOT NULL,
			trial_grant_id BIGINT REFERENCES credit_grants(id) ON DELETE SET NULL,
			abuse_score INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_guest_usage_keys_ip_hash
			ON guest_usage_keys (ip_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_guest_usage_keys_last_seen
			ON guest_usage_keys (last_seen_at DESC)`,
		`ALTER TABLE guest_usage_keys
			DROP CONSTRAINT IF EXISTS guest_usage_keys_abuse_score_check`,
		`ALTER TABLE guest_usage_keys
			ADD CONSTRAINT guest_usage_keys_abuse_score_check CHECK (abuse_score >= 0)`,
		`ALTER TABLE guest_usage_keys
			DROP CONSTRAINT IF EXISTS guest_usage_keys_seen_order_check`,
		`ALTER TABLE guest_usage_keys
			ADD CONSTRAINT guest_usage_keys_seen_order_check CHECK (last_seen_at >= first_seen_at)`,
		`CREATE TABLE IF NOT EXISTS lifetime_quote_requests (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status VARCHAR(24) NOT NULL DEFAULT 'requested',
			paid_months_completed INTEGER NOT NULL DEFAULT 0,
			usage_window_start TIMESTAMPTZ NOT NULL,
			usage_window_end TIMESTAMPTZ NOT NULL,
			usage_event_count INTEGER NOT NULL DEFAULT 0,
			credits_used INTEGER NOT NULL DEFAULT 0,
			average_monthly_credits INTEGER NOT NULL DEFAULT 0,
			estimated_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
			average_monthly_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
			notes TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_lifetime_quote_requests_user_created
			ON lifetime_quote_requests (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_lifetime_quote_requests_status_created
			ON lifetime_quote_requests (status, created_at DESC)`,
		`ALTER TABLE lifetime_quote_requests
			DROP CONSTRAINT IF EXISTS lifetime_quote_requests_status_check`,
		`ALTER TABLE lifetime_quote_requests
			ADD CONSTRAINT lifetime_quote_requests_status_check
			CHECK (status IN ('requested', 'reviewed', 'quoted', 'declined', 'cancelled'))`,
		`ALTER TABLE lifetime_quote_requests
			DROP CONSTRAINT IF EXISTS lifetime_quote_requests_non_negative_check`,
		`ALTER TABLE lifetime_quote_requests
			ADD CONSTRAINT lifetime_quote_requests_non_negative_check
			CHECK (
				paid_months_completed >= 0
				AND usage_event_count >= 0
				AND credits_used >= 0
				AND average_monthly_credits >= 0
				AND estimated_cost_usd_micros >= 0
				AND average_monthly_cost_usd_micros >= 0
			)`,
		`ALTER TABLE lifetime_quote_requests
			DROP CONSTRAINT IF EXISTS lifetime_quote_requests_window_check`,
		`ALTER TABLE lifetime_quote_requests
			ADD CONSTRAINT lifetime_quote_requests_window_check CHECK (usage_window_end > usage_window_start)`,
		`CREATE TABLE IF NOT EXISTS ai_usage_limit_events (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			action_code VARCHAR(80) NOT NULL,
			reason VARCHAR(64) NOT NULL,
			required_credits INTEGER NOT NULL DEFAULT 0,
			available_credits INTEGER NOT NULL DEFAULT 0,
			daily_limit INTEGER NOT NULL DEFAULT 0,
			used_today INTEGER NOT NULL DEFAULT 0,
			daily_remaining INTEGER NOT NULL DEFAULT 0,
			plan_code VARCHAR(64) NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_limit_events_user_created
			ON ai_usage_limit_events (user_id, created_at DESC)
			WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_limit_events_guest_created
			ON ai_usage_limit_events (guest_device_id_hash, created_at DESC)
			WHERE guest_device_id_hash <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_ai_usage_limit_events_reason_created
			ON ai_usage_limit_events (reason, created_at DESC)`,
		`ALTER TABLE ai_usage_limit_events
			DROP CONSTRAINT IF EXISTS ai_usage_limit_events_identity_check`,
		`ALTER TABLE ai_usage_limit_events
			ADD CONSTRAINT ai_usage_limit_events_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE ai_usage_limit_events
			DROP CONSTRAINT IF EXISTS ai_usage_limit_events_reason_check`,
		`ALTER TABLE ai_usage_limit_events
			ADD CONSTRAINT ai_usage_limit_events_reason_check
			CHECK (reason IN ('insufficient_ai_credits', 'daily_ai_limit_reached', 'feature_locked', 'guest_not_allowed', 'subject_required'))`,
		`ALTER TABLE ai_usage_limit_events
			DROP CONSTRAINT IF EXISTS ai_usage_limit_events_non_negative_check`,
		`ALTER TABLE ai_usage_limit_events
			ADD CONSTRAINT ai_usage_limit_events_non_negative_check
			CHECK (
				required_credits >= 0
				AND available_credits >= 0
				AND daily_limit >= 0
				AND used_today >= 0
				AND daily_remaining >= 0
			)`,
		`CREATE TABLE IF NOT EXISTS ai_model_pricings (
			id BIGSERIAL PRIMARY KEY,
			provider VARCHAR(40) NOT NULL,
			model VARCHAR(80) NOT NULL,
			operation VARCHAR(32) NOT NULL,
			input_token_usd_micros BIGINT NOT NULL DEFAULT 0,
			output_token_usd_micros BIGINT NOT NULL DEFAULT 0,
			audio_minute_usd_micros BIGINT NOT NULL DEFAULT 0,
			request_usd_micros BIGINT NOT NULL DEFAULT 0,
			credit_usd_micros BIGINT NOT NULL DEFAULT 100,
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_model_pricings_provider_model_operation
			ON ai_model_pricings (provider, model, operation)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_model_pricings_active
			ON ai_model_pricings (active, provider, model)`,
		`ALTER TABLE ai_model_pricings
			DROP CONSTRAINT IF EXISTS ai_model_pricings_operation_check`,
		`ALTER TABLE ai_model_pricings
			ADD CONSTRAINT ai_model_pricings_operation_check
			CHECK (operation IN ('llm', 'transcription', 'credit_fallback'))`,
		`ALTER TABLE ai_model_pricings
			DROP CONSTRAINT IF EXISTS ai_model_pricings_non_negative_check`,
		`ALTER TABLE ai_model_pricings
			ADD CONSTRAINT ai_model_pricings_non_negative_check
			CHECK (
				input_token_usd_micros >= 0
				AND output_token_usd_micros >= 0
				AND audio_minute_usd_micros >= 0
				AND request_usd_micros >= 0
				AND credit_usd_micros >= 0
			)`,
		`CREATE TABLE IF NOT EXISTS ai_abuse_blocks (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
			guest_device_id_hash CHAR(64) NOT NULL DEFAULT '',
			scope VARCHAR(32) NOT NULL DEFAULT 'ai_parse',
			reason_code VARCHAR(80) NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			active BOOLEAN NOT NULL DEFAULT TRUE,
			expires_at TIMESTAMPTZ,
			created_by VARCHAR(120) NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_abuse_blocks_user_active
			ON ai_abuse_blocks (user_id, active, expires_at)
			WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_ai_abuse_blocks_guest_active
			ON ai_abuse_blocks (guest_device_id_hash, active, expires_at)
			WHERE guest_device_id_hash <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_ai_abuse_blocks_scope_active
			ON ai_abuse_blocks (scope, active, created_at DESC)`,
		`ALTER TABLE ai_abuse_blocks
			DROP CONSTRAINT IF EXISTS ai_abuse_blocks_identity_check`,
		`ALTER TABLE ai_abuse_blocks
			ADD CONSTRAINT ai_abuse_blocks_identity_check
			CHECK (user_id IS NOT NULL OR guest_device_id_hash <> '')`,
		`ALTER TABLE ai_abuse_blocks
			DROP CONSTRAINT IF EXISTS ai_abuse_blocks_scope_check`,
		`ALTER TABLE ai_abuse_blocks
			ADD CONSTRAINT ai_abuse_blocks_scope_check
			CHECK (scope IN ('ai_parse', 'all_ai'))`,
		`UPDATE accounts
			SET type = 'credit_card'
			WHERE LOWER(type) = 'credit'`,
		`UPDATE accounts
			SET type = 'debit_card'
			WHERE LOWER(type) = 'debit'`,
		`UPDATE accounts
			SET type = 'wallet'
			WHERE LOWER(type) = 'wallets'`,
		`ALTER TABLE accounts
			ALTER COLUMN credit_limit TYPE NUMERIC(19,2) USING ROUND(credit_limit::numeric, 2),
			ALTER COLUMN credit_limit SET DEFAULT 0,
			ALTER COLUMN credit_limit SET NOT NULL,
			ALTER COLUMN balance TYPE NUMERIC(19,2) USING ROUND(balance::numeric, 2),
			ALTER COLUMN balance SET DEFAULT 0,
			ALTER COLUMN balance SET NOT NULL`,
		`INSERT INTO accounts (user_id, type, name, color, is_default, created_at, updated_at)
			SELECT users.id, 'cash', 'Cash', '#2ECC71', true, NOW(), NOW()
			FROM users
			WHERE NOT EXISTS (
				SELECT 1 FROM accounts WHERE accounts.user_id = users.id
			)`,
		`UPDATE entries
			SET source = 'manual'
			WHERE source IS NULL OR TRIM(source) = ''`,
		// An entry may have no account. Capture must never be blocked on setting
		// one up, so `account_id` is nullable and an unlinked row is surfaced as
		// a review item instead (see `dashboardReviewItems`). This DROPs rather
		// than merely omitting the constraint, because databases created before
		// the rule changed still carry it. The backfill that used to sit above
		// this statement was removed with it: silently attaching the default
		// account would erase exactly the "unlinked" state the app now relies on.
		`ALTER TABLE entries
			ALTER COLUMN account_id DROP NOT NULL,
			ALTER COLUMN amount SET NOT NULL,
			ALTER COLUMN source SET DEFAULT 'manual',
			ALTER COLUMN source SET NOT NULL`,
		`ALTER TABLE entries
			ADD COLUMN IF NOT EXISTS refundable_amount NUMERIC(19,2),
			ADD COLUMN IF NOT EXISTS refund_expected_on TEXT,
			ADD COLUMN IF NOT EXISTS refund_reminder_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS refund_status VARCHAR(16)`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_refundable_amount_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_refundable_amount_check
			CHECK (refundable_amount IS NULL OR (refundable_amount > 0 AND refundable_amount <= amount))`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_refund_status_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_refund_status_check
			CHECK (refund_status IS NULL OR refund_status IN ('pending', 'received', 'written_off'))`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_refund_tracking_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_refund_tracking_check
			CHECK (
				(refundable_amount IS NULL AND refund_expected_on IS NULL AND refund_reminder_at IS NULL AND refund_status IS NULL)
				OR
				(refundable_amount IS NOT NULL AND refund_expected_on IS NOT NULL AND refund_status IS NOT NULL)
			)`,
		`DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'entries'
				  AND column_name = 'refund_expected_on'
				  AND data_type = 'date'
			) THEN
				ALTER TABLE entries
					ALTER COLUMN refund_expected_on TYPE TEXT
					USING to_char(refund_expected_on, 'YYYY-MM-DD');
			END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_entries_pending_refunds
			ON entries (refund_expected_on, refund_reminder_at)
			WHERE refund_status = 'pending'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_refund_due_unique
			ON notifications (user_id, type, action_url)
			WHERE type = 'refund.due'`,
		`UPDATE entries
			SET type = LOWER(type)
			WHERE LOWER(type) IN ('expense', 'income')`,
		`UPDATE entries
			SET source = LOWER(source)
			WHERE LOWER(source) IN ('manual', 'text', 'voice')`,
		`ALTER TABLE accounts
			DROP CONSTRAINT IF EXISTS accounts_type_check`,
		`ALTER TABLE accounts
			ADD CONSTRAINT accounts_type_check
			CHECK (type IN ('cash', 'upi', 'bank', 'credit_card', 'debit_card', 'wallet', 'other'))`,
		`ALTER TABLE accounts
			DROP CONSTRAINT IF EXISTS accounts_credit_limit_non_negative_check`,
		`ALTER TABLE accounts
			ADD CONSTRAINT accounts_credit_limit_non_negative_check CHECK (credit_limit >= 0)`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_amount_positive_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_amount_positive_check CHECK (amount > 0)`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_type_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_type_check CHECK (type IN ('expense', 'income'))`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS entries_source_check`,
		`ALTER TABLE entries
			ADD CONSTRAINT entries_source_check CHECK (source IN ('manual', 'text', 'voice'))`,
		`ALTER TABLE entries
			DROP CONSTRAINT IF EXISTS fk_entries_owned_account`,
		`ALTER TABLE entries
			ADD CONSTRAINT fk_entries_owned_account
			FOREIGN KEY (user_id, account_id) REFERENCES accounts(user_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_split_friends_user_archived
			ON split_friends (user_id, archived, name)`,
		`ALTER TABLE split_bills
			ADD COLUMN IF NOT EXISTS group_id BIGINT`,
		`CREATE TABLE IF NOT EXISTS split_groups (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			archived BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS split_group_members (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
			friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_split_groups_user_archived
			ON split_groups (user_id, archived, name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_members_unique
			ON split_group_members (user_id, group_id, friend_id)`,
		`CREATE TABLE IF NOT EXISTS split_group_invites (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE split_group_invites
			DROP CONSTRAINT IF EXISTS split_group_invites_status_check`,
		`ALTER TABLE split_group_invites
			ADD CONSTRAINT split_group_invites_status_check CHECK (status IN ('active', 'revoked'))`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_invites_group_active
			ON split_group_invites (user_id, group_id, status, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS split_group_direct_invites (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
			invite_id BIGINT NOT NULL REFERENCES split_group_invites(id) ON DELETE CASCADE,
			target_email VARCHAR(254) NOT NULL DEFAULT '',
			target_phone VARCHAR(32) NOT NULL DEFAULT '',
			invited_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE split_group_direct_invites
			DROP CONSTRAINT IF EXISTS split_group_direct_invites_target_check`,
		`ALTER TABLE split_group_direct_invites
			ADD CONSTRAINT split_group_direct_invites_target_check CHECK (target_email <> '' OR target_phone <> '')`,
		`ALTER TABLE split_group_direct_invites
			DROP CONSTRAINT IF EXISTS split_group_direct_invites_status_check`,
		`ALTER TABLE split_group_direct_invites
			ADD CONSTRAINT split_group_direct_invites_status_check CHECK (status IN ('pending', 'accepted', 'revoked'))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_direct_invites_email_unique
			ON split_group_direct_invites (group_id, LOWER(target_email))
			WHERE target_email <> '' AND status = 'pending'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_direct_invites_phone_unique
			ON split_group_direct_invites (group_id, target_phone)
			WHERE target_phone <> '' AND status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_direct_invites_invited_user
			ON split_group_direct_invites (invited_user_id, status, created_at DESC)`,
		`ALTER TABLE split_group_direct_invites
			ADD COLUMN IF NOT EXISTS friend_id BIGINT REFERENCES split_friends(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_direct_invites_friend
			ON split_group_direct_invites (friend_id)
			WHERE friend_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS split_group_user_members (
			id BIGSERIAL PRIMARY KEY,
			group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role VARCHAR(16) NOT NULL DEFAULT 'member',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE split_group_user_members
			DROP CONSTRAINT IF EXISTS split_group_user_members_role_check`,
		`ALTER TABLE split_group_user_members
			ADD CONSTRAINT split_group_user_members_role_check CHECK (role IN ('member'))`,
		`ALTER TABLE split_group_user_members
			DROP CONSTRAINT IF EXISTS split_group_user_members_status_check`,
		`ALTER TABLE split_group_user_members
			ADD CONSTRAINT split_group_user_members_status_check CHECK (status IN ('active', 'removed'))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_user_members_unique
			ON split_group_user_members (group_id, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_user_members_user_active
			ON split_group_user_members (user_id, status, group_id)`,
		`CREATE TABLE IF NOT EXISTS split_group_member_links (
			id BIGSERIAL PRIMARY KEY,
			group_id BIGINT NOT NULL REFERENCES split_groups(id) ON DELETE CASCADE,
			slot VARCHAR(32) NOT NULL,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_group_member_links_unique
			ON split_group_member_links (group_id, slot, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_member_links_friend
			ON split_group_member_links (user_id, friend_id)`,
		`ALTER TABLE split_friends
			ADD COLUMN IF NOT EXISTS linked_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL`,
		`ALTER TABLE users
			ADD COLUMN IF NOT EXISTS phone_normalized VARCHAR(10) NOT NULL DEFAULT ''`,
		`ALTER TABLE split_friends
			ADD COLUMN IF NOT EXISTS phone_normalized VARCHAR(10) NOT NULL DEFAULT ''`,
		`ALTER TABLE split_group_direct_invites
			ADD COLUMN IF NOT EXISTS target_phone_normalized VARCHAR(10) NOT NULL DEFAULT ''`,
		`UPDATE users
			SET phone_normalized = RIGHT(REGEXP_REPLACE(phone, '[^0-9]', '', 'g'), 10)
			WHERE phone IS NOT NULL AND LENGTH(REGEXP_REPLACE(phone, '[^0-9]', '', 'g')) >= 10`,
		`UPDATE split_friends
			SET phone_normalized = RIGHT(REGEXP_REPLACE(phone, '[^0-9]', '', 'g'), 10)
			WHERE phone <> '' AND LENGTH(REGEXP_REPLACE(phone, '[^0-9]', '', 'g')) >= 10`,
		`UPDATE split_group_direct_invites
			SET target_phone_normalized = RIGHT(REGEXP_REPLACE(target_phone, '[^0-9]', '', 'g'), 10)
			WHERE target_phone <> '' AND LENGTH(REGEXP_REPLACE(target_phone, '[^0-9]', '', 'g')) >= 10`,
		`CREATE INDEX IF NOT EXISTS idx_users_phone_normalized
			ON users (phone_normalized) WHERE phone_normalized <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_split_friends_phone_normalized
			ON split_friends (phone_normalized) WHERE phone_normalized <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_split_group_direct_invites_phone_normalized
			ON split_group_direct_invites (target_phone_normalized) WHERE target_phone_normalized <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_split_friends_linked_user
			ON split_friends (linked_user_id)
			WHERE linked_user_id IS NOT NULL`,
		// Backfill the friend-to-user link for groups that were shared before
		// the column existed. Matching on a verified email or phone is the same
		// rule invite acceptance already uses to decide which friend row stands
		// for an arriving user.
		`UPDATE split_friends
			SET linked_user_id = users.id
			FROM users
			WHERE split_friends.linked_user_id IS NULL
				AND (
					(split_friends.email <> '' AND LOWER(split_friends.email) = LOWER(users.email))
					OR (split_friends.phone_normalized <> '' AND split_friends.phone_normalized = users.phone_normalized)
				)`,
		`ALTER TABLE split_groups
			ADD COLUMN IF NOT EXISTS kind VARCHAR(16) NOT NULL DEFAULT 'other'`,
		`ALTER TABLE split_groups
			ADD COLUMN IF NOT EXISTS default_split TEXT`,
		`ALTER TABLE split_groups
			DROP CONSTRAINT IF EXISTS split_groups_kind_check`,
		`ALTER TABLE split_groups
			ADD CONSTRAINT split_groups_kind_check CHECK (kind IN ('trip', 'home', 'couple', 'other'))`,
		`CREATE INDEX IF NOT EXISTS idx_split_bills_user_date
			ON split_bills (user_id, date DESC, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_split_participants_user_friend
			ON split_participants (user_id, friend_id)`,
		`CREATE INDEX IF NOT EXISTS idx_split_settlements_user_friend
			ON split_settlements (user_id, friend_id, date DESC)`,
		`ALTER TABLE split_bills
			DROP CONSTRAINT IF EXISTS split_bills_total_amount_positive_check`,
		`ALTER TABLE split_bills
			ADD CONSTRAINT split_bills_total_amount_positive_check CHECK (total_amount > 0)`,
		`ALTER TABLE split_bills
			DROP CONSTRAINT IF EXISTS split_bills_currency_check`,
		`ALTER TABLE split_bills
			ADD CONSTRAINT split_bills_currency_check CHECK (currency = 'INR')`,
		`ALTER TABLE split_participants
			DROP CONSTRAINT IF EXISTS split_participants_share_amount_positive_check`,
		`ALTER TABLE split_participants
			ADD CONSTRAINT split_participants_share_amount_positive_check CHECK (share_amount > 0)`,
		`ALTER TABLE split_participants
			DROP CONSTRAINT IF EXISTS split_participants_direction_check`,
		`ALTER TABLE split_participants
			ADD CONSTRAINT split_participants_direction_check
			CHECK (direction IN ('friend_owes_user', 'user_owes_friend'))`,
		`ALTER TABLE split_settlements
			DROP CONSTRAINT IF EXISTS split_settlements_amount_positive_check`,
		`ALTER TABLE split_settlements
			ADD CONSTRAINT split_settlements_amount_positive_check CHECK (amount > 0)`,
		`ALTER TABLE split_settlements
			DROP CONSTRAINT IF EXISTS split_settlements_direction_check`,
		`ALTER TABLE split_settlements
			ADD CONSTRAINT split_settlements_direction_check
			CHECK (direction IN ('friend_paid_user', 'user_paid_friend'))`,
		// --- Split ledger integrity. See migrations/0043_repair_split_ledger.sql
		// for the full account of what these repair and why.
		//
		// The structural half runs every boot, because every statement in it is a
		// no-op once it has been applied. The repair half below does not: it
		// deletes rows, and this function runs on every start.
		`ALTER TABLE split_groups
			ADD COLUMN IF NOT EXISTS photo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE split_settlements
			ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES split_groups(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS idx_split_settlements_group
			ON split_settlements (group_id) WHERE group_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS split_friend_merges (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			from_friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
			to_friend_id BIGINT NOT NULL REFERENCES split_friends(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_split_friend_merges_target
			ON split_friend_merges (user_id, to_friend_id)`,
		// A row can only be merged away once. Stated as its own index rather
		// than an inline UNIQUE on the column: an inline one becomes a
		// *constraint* named by Postgres, AutoMigrate looks for a GORM-named
		// one, and finding neither it tries to drop the name it expected and
		// fails the boot outright. An index the model does not declare is a
		// thing AutoMigrate has no opinion about.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_split_friend_merges_source
			ON split_friend_merges (from_friend_id)`,
		// Participants are now a bill's rows in the schema, not just by
		// convention. Applied after the repair below has cleared the ones that
		// already have no bill, since the constraint cannot be added over them.
		`DELETE FROM split_participants
			WHERE NOT EXISTS (
				SELECT 1 FROM split_bills WHERE split_bills.id = split_participants.bill_id
			)`,
		`ALTER TABLE split_participants
			DROP CONSTRAINT IF EXISTS fk_split_participants_bill`,
		`ALTER TABLE split_participants
			ADD CONSTRAINT fk_split_participants_bill
			FOREIGN KEY (bill_id) REFERENCES split_bills(id) ON DELETE CASCADE`,

		// A ledger for one-time repairs, so a destructive fix can ship in this
		// function without running again on the next start.
		`CREATE TABLE IF NOT EXISTS schema_repairs (
			name TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// The one-time half. Every statement here deletes or rewrites rows that
		// predate the fixes above, and none of them should be applied to data
		// written afterwards:
		//
		//   - Bills stranded on an archived group. Deleting a group already takes
		//     the owner's own bills; a group shared with another member kept
		//     theirs, on a group no screen will open.
		//   - Settlements for a friend with no expense history at all. A
		//     settlement means "this much of what we owed each other is paid", so
		//     with nothing on either side it cannot describe a debt — only invent
		//     one. Re-running this on live data would silently delete a legitimate
		//     settlement recorded before the first expense, which is exactly why
		//     it is guarded.
		//   - Backfilling the group a legacy settlement belongs to, where the
		//     friend shares exactly one group with the user and there is no other
		//     ledger it could have settled.
		`DO $$
		BEGIN
			IF EXISTS (SELECT 1 FROM schema_repairs WHERE name = '0043_repair_split_ledger') THEN
				RETURN;
			END IF;

			DELETE FROM split_participants
			WHERE bill_id IN (
				SELECT b.id FROM split_bills b
				LEFT JOIN split_groups g ON g.id = b.group_id
				WHERE b.group_id IS NOT NULL AND (g.id IS NULL OR g.archived)
			);

			DELETE FROM split_bills b USING split_groups g
			WHERE b.group_id = g.id AND g.archived;

			DELETE FROM split_bills
			WHERE group_id IS NOT NULL
				AND NOT EXISTS (SELECT 1 FROM split_groups g WHERE g.id = split_bills.group_id);

			DELETE FROM split_settlements s
			WHERE s.group_id IS NULL
				AND NOT EXISTS (
					SELECT 1 FROM split_participants p
					WHERE p.user_id = s.user_id AND p.friend_id = s.friend_id
				);

			UPDATE split_settlements s
			SET group_id = single.group_id
			FROM (
				SELECT m.user_id, m.friend_id, MIN(m.group_id) AS group_id
				FROM split_group_members m
				JOIN split_groups g ON g.id = m.group_id AND NOT g.archived
				GROUP BY m.user_id, m.friend_id
				HAVING COUNT(DISTINCT m.group_id) = 1
			) AS single
			WHERE s.group_id IS NULL
				AND s.user_id = single.user_id
				AND s.friend_id = single.friend_id;

			INSERT INTO schema_repairs (name) VALUES ('0043_repair_split_ledger');
		END
		$$`,
		// The monthly review's once-per-month guarantee lives in this index
		// rather than in the job that writes through it.
		`CREATE TABLE IF NOT EXISTS monthly_reviews (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			month CHAR(7) NOT NULL,
			total_spent NUMERIC(19,2) NOT NULL DEFAULT 0,
			transaction_count INTEGER NOT NULL DEFAULT 0,
			notification_id BIGINT REFERENCES notifications(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_monthly_review_once
			ON monthly_reviews (user_id, month)`,
		`CREATE INDEX IF NOT EXISTS idx_monthly_reviews_notification
			ON monthly_reviews (notification_id)`,
		// Credit card statements. AutoMigrate creates the columns; the
		// constraints and partial indexes below are the parts it cannot
		// express, and the unique index is what makes statement parsing
		// re-runnable without producing duplicate bills.
		`ALTER TABLE accounts
			ADD COLUMN IF NOT EXISTS statement_day INTEGER NOT NULL DEFAULT 0,
			ADD COLUMN IF NOT EXISTS reminder_days_before INTEGER NOT NULL DEFAULT 3,
			ADD COLUMN IF NOT EXISTS reminder_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			ADD COLUMN IF NOT EXISTS card_ledger_reset_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS card_ledger_reset_entry_id BIGINT,
			ADD COLUMN IF NOT EXISTS autopay_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE accounts
			ADD COLUMN IF NOT EXISTS provider_id VARCHAR(64) NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS last4 VARCHAR(4) NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS upi_handle VARCHAR(120) NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS wallet_nickname VARCHAR(120) NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS account_nickname VARCHAR(120) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_provider_id ON accounts (provider_id)`,
		`UPDATE accounts
			SET last4 = identifier
			WHERE type IN ('bank', 'credit_card', 'debit_card')
				AND last4 = '' AND identifier ~ '^[0-9]{4}$'`,
		`UPDATE accounts
			SET upi_handle = identifier
			WHERE type = 'upi' AND upi_handle = '' AND identifier <> ''`,
		`UPDATE accounts
			SET wallet_nickname = identifier
			WHERE type = 'wallet' AND wallet_nickname = '' AND identifier <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_card_statement_once
			ON card_statements (user_id, account_id, statement_date)`,
		`CREATE INDEX IF NOT EXISTS idx_card_statements_account_date
			ON card_statements (account_id, statement_date DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_card_statements_due
			ON card_statements (due_date)
			WHERE status <> 'paid'`,
		`ALTER TABLE card_statements
			DROP CONSTRAINT IF EXISTS card_statements_amounts_non_negative`,
		`ALTER TABLE card_statements
			ADD CONSTRAINT card_statements_amounts_non_negative
			CHECK (total_due >= 0 AND minimum_due >= 0 AND paid_amount >= 0)`,
		`ALTER TABLE card_statements
			DROP CONSTRAINT IF EXISTS card_statements_cycle_ordered`,
		`ALTER TABLE card_statements
			ADD CONSTRAINT card_statements_cycle_ordered
			CHECK (cycle_end >= cycle_start AND due_date > statement_date)`,
		`ALTER TABLE card_statements
			DROP CONSTRAINT IF EXISTS card_statements_status_check`,
		`ALTER TABLE card_statements
			ADD CONSTRAINT card_statements_status_check
			CHECK (status IN ('draft', 'unpaid', 'partial', 'paid'))`,
		`CREATE INDEX IF NOT EXISTS idx_card_statement_payments_statement
			ON card_statement_payments (statement_id, paid_on DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_card_statement_payments_user
			ON card_statement_payments (user_id, paid_on DESC)`,
		`ALTER TABLE card_statement_payments
			DROP CONSTRAINT IF EXISTS card_statement_payments_amount_positive`,
		`ALTER TABLE card_statement_payments
			ADD CONSTRAINT card_statement_payments_amount_positive CHECK (amount > 0)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_card_statement_reminder_once
			ON card_statement_reminders (user_id, statement_id, kind)`,
		// Card EMI plans. The instalment status vocabulary is what stops the
		// same principal reducing the limit twice — see the migration.
		`CREATE INDEX IF NOT EXISTS idx_card_emi_plans_account
			ON card_emi_plans (account_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_card_emi_plans_user
			ON card_emi_plans (user_id, status)`,
		`ALTER TABLE card_emi_plans
			DROP CONSTRAINT IF EXISTS card_emi_plans_status_check`,
		`ALTER TABLE card_emi_plans
			ADD CONSTRAINT card_emi_plans_status_check
			CHECK (status IN ('active', 'closed', 'foreclosed'))`,
		`ALTER TABLE card_emi_plans
			DROP CONSTRAINT IF EXISTS card_emi_plans_principal_positive`,
		`ALTER TABLE card_emi_plans
			ADD CONSTRAINT card_emi_plans_principal_positive CHECK (principal > 0)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_card_emi_installment_once
			ON card_emi_installments (plan_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_card_emi_installments_blocked
			ON card_emi_installments (account_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_card_emi_installments_due
			ON card_emi_installments (due_date)
			WHERE status = 'scheduled'`,
		`ALTER TABLE card_emi_installments
			DROP CONSTRAINT IF EXISTS card_emi_installments_status_check`,
		`ALTER TABLE card_emi_installments
			ADD CONSTRAINT card_emi_installments_status_check
			CHECK (status IN ('scheduled', 'billed', 'paid'))`,
	}
}
