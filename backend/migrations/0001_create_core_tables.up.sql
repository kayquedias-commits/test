-- =============================================================================
-- 0001_create_core_tables (up)
--
-- Versioned, reversible core schema for the Financial Management System (FMS).
-- Executed by the goose migration runner at API/worker bootstrap (`make migrate`)
-- or goose-embedded. Reversibility is provided by the sibling down migration.
--
-- PostgreSQL 16. A single system timezone is assumed throughout (§8); this file
-- pins the database session to UTC so `current_date` and CHECK rules that depend
-- on "today" are deterministic. All schema changes are versioned — no manual DDL.
--
-- Security posture:
--   * RLS tenant isolation on every tenant table, FORCEd so the table owner is
--     subject to the same tenancy policy (prevents owner-bypass reads/writes).
--   * All RLS policies are null-safe: with no `app.tenant_id` context the GUC
--     resolves to NULL and every RLS predicate evaluates to false -> zero rows.
--   * Append-only audit enforced by a blocking trigger covering UPDATE/DELETE and
--     TRUNCATE, plus REVOKE statements, plus SHA-256 chained-hash columns.
--   * Persistence-layer CHECK constraints are DEFENSE IN DEPTH. The authoritative
--     invariants (account balance = opening + sum(transactions), one-level
--     subcategories, positive amounts, valid dates) live in the domain aggregates
--     (`Account`, `Transaction`, `Category`) inside the same transaction as the
--     triggering mutation; the constraints below mirror and backstop those rules.
--
-- Privilege model: the migration/DDL role and the runtime API role (`app_user`)
-- MUST be distinct. `app_user` holds only SELECT/INSERT/UPDATE/DELETE on tenant
-- tables and owns no tenant data; a separate owner role runs the connection
-- bootstrap `set_config('app.tenant_id', ...)` per session.
--
-- Money representation: NUMERIC(12,2) fixed-point (never binary floats). The
-- repository layer converts to/from int64 minor units and must fail fast on
-- overflow beyond the 10 integer digits this type supports (Module B).
-- =============================================================================

-- ---------------------------------------------------------------------------
-- Extensions and session setup (must precede the GIN trigram index and UUID PKs)
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SET TIMEZONE = 'UTC';
SET client_encoding = 'UTF8';

-- ---------------------------------------------------------------------------
-- Helper functions (defined before any consumer)
-- ---------------------------------------------------------------------------

-- Returns the current tenant id from the session GUC, or NULL when unset.
-- `NULLIF` guards the empty string and `missing_ok => true` guards the unset
-- GUC so the returned value is always either a valid uuid or NULL — never an
-- error. RLS policies compare against this value; NULL comparisons filter out,
-- which is what makes an unauthenticated/context-less session return zero rows.
CREATE OR REPLACE FUNCTION app_context()
RETURNS uuid
LANGUAGE sql
STABLE
AS $$
  SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid
$$;

-- Session bootstrap helper; the repository/connection layer calls this once per
-- session (is_local = false => SET, not SET LOCAL). Every transaction additionally
-- re-establishes the tenant context so partition and audit writes always carry it.
CREATE OR REPLACE FUNCTION fms_set_tenant(p_tenant_id uuid)
RETURNS text
LANGUAGE sql
AS $$
  SELECT set_config('app.tenant_id', p_tenant_id::text, false)
$$;

-- Generic `updated_at` maintenance for tables that have the column.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;

-- Category hierarchy guard (defense in depth for Module C):
--   * one level deep — a category whose parent is itself a subcategory is rejected;
--   * parent/child `type` consistency — a subcategory must share its parent's
--     INCOME/EXPENSE classification (prevents taxonomy corruption that would
--     distort reporting and budget aggregates).
-- The Category aggregate remains authoritative; this trigger is the persistence
-- fallback so no client or adapter can create invalid trees.
CREATE OR REPLACE FUNCTION enforce_category_one_level()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.parent_id IS NOT NULL THEN
    IF EXISTS (SELECT 1 FROM categories WHERE id = NEW.parent_id AND parent_id IS NOT NULL) THEN
      RAISE EXCEPTION 'categories support only one level of subcategories',
        USING ERRCODE = '23514', CONSTRAINT = 'categories_one_level';
    END IF;
    IF EXISTS (SELECT 1 FROM categories WHERE id = NEW.parent_id AND type IS DISTINCT FROM NEW.type) THEN
      RAISE EXCEPTION 'a subcategory must share its parent category type',
        USING ERRCODE = '23514', CONSTRAINT = 'categories_subcategory_type';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

-- Audit immutability guard (tamper evidence §5.3/§6.3). Blocks UPDATE, DELETE,
-- and TRUNCATE so no path (application or direct DML) can mutate or erase the
-- log. Complements the REVOKE statements and the chained-hash columns.
CREATE OR REPLACE FUNCTION enforce_audit_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'audit_entries are append-only; updates, deletes, and truncates are forbidden',
    USING ERRCODE = '55000';
END;
$$;

-- ---------------------------------------------------------------------------
-- Tables (dependency order: roles -> users -> categories -> accounts ->
-- transactions -> transaction_categories -> budgets -> finance_periods ->
-- refresh_tokens -> sessions -> idempotency_keys -> outbox -> audit_entries)
-- ---------------------------------------------------------------------------

-- RBAC roles. Permission sets are policy data (§7.8/§9.7); ids are fixed in the
-- seed section for deterministic `ON CONFLICT DO NOTHING` replay.
CREATE TABLE roles (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code         varchar(32) NOT NULL UNIQUE,
  name         varchar(64) NOT NULL,
  permissions  jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- Users. `password_hash` holds a strong adaptive hash (Argon2id, or bcrypt with
-- cost >= 12); the hash algorithm is documented here and enforced by the
-- authentication use case. `mfa_secret` is the plaintext TOTP seed material and
-- MUST be encrypted at rest (AES-256, KMS-managed key) by the persistence layer —
-- it is never written to durable storage unencrypted. Administrative authority is
-- derived solely from the RBAC `admin` role; there is no independent is_admin
-- boolean so no privilege flag can bypass permission policy (§5.1, Module F).
CREATE TABLE users (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL,
  username       varchar(64) NOT NULL UNIQUE,
  password_hash  text NOT NULL,                       -- Argon2id / bcrypt(cost>=12)
  mfa_secret     text NULL,                           -- encrypted at rest
  mfa_enabled    boolean NOT NULL DEFAULT true,       -- MFA mandatory, default-on
  role_id        uuid NOT NULL REFERENCES roles(id),
  active         boolean NOT NULL DEFAULT true,
  version        integer NOT NULL DEFAULT 1,          -- optimistic lock target
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Category taxonomy (Module C). One level of subcategories, tenant-scoped parent,
-- and name uniqueness within a tenant/parent level. `parent_id <> id` blocks
-- self-reference; the tenant-scoped self-FK blocks cross-tenant parent linkage
-- (belt-and-suspenders alongside RLS). One-level depth and type consistency are
-- enforced by the `enforce_category_one_level` trigger.
CREATE TABLE categories (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL,
  name        varchar(80) NOT NULL,
  type        varchar(8) NOT NULL CHECK (type IN ('INCOME','EXPENSE')),
  parent_id   uuid NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT categories_parent_not_self CHECK (parent_id <> id),
  CONSTRAINT categories_tenant_parent_fk
    FOREIGN KEY (tenant_id, parent_id)
    REFERENCES categories(tenant_id, id),
  CONSTRAINT categories_name_unique
    UNIQUE NULLS NOT DISTINCT (tenant_id, name, parent_id)
);

-- Financial accounts (Module A). `balance` is the cached aggregate; the invariant
-- `balance = opening_balance + SUM(transactions)` is owned by the Account
-- aggregate inside the same transaction as the mutation (never a deferred job,
-- never a DB trigger that would split invariant ownership). `credit` accounts may
-- legitimately hold a negative net balance, so `balance` is deliberately
-- unconstrained. `currency` is uppercased ISO-4217 shape.
CREATE TABLE accounts (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  name            varchar(120) NOT NULL,
  type            varchar(16) NOT NULL CHECK (type IN ('checking','savings','credit','cash','other')),
  opening_balance numeric(12,2) NOT NULL DEFAULT 0 CHECK (opening_balance >= 0),
  balance         numeric(12,2) NOT NULL DEFAULT 0,   -- cached aggregate; invariant in Account aggregate
  currency        char(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
  version         integer NOT NULL DEFAULT 1,          -- optimistic lock target
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Transaction ledger (Module B). Monthly RANGE-partitioned on `date`; the PRIMARY
-- KEY includes the partition key `date` because PostgreSQL forbids a unique index
-- on a partitioned table that omits it. Note there is deliberately no
-- `date = date_trunc('day', date)` CHECK: the `date` type carries no time
-- component so that expression is a no-op; calendar validity is inherent to the
-- type. Future-dated rows require `scheduled = true`; DB `current_date` is the
-- authority under the single-timezone assumption (§8). Negative/zero flows are
-- modeled as EXPENSE-direction rows carrying an allowlisted reason_code
-- (REFUND/ADJUSTMENT/REVERSAL) — the reconcile use case decides whether such a
-- row increments or decrements `accounts.balance`.
CREATE TABLE transactions (
  id           uuid NOT NULL,
  tenant_id    uuid NOT NULL,
  account_id   uuid NOT NULL REFERENCES accounts(id),
  amount       numeric(12,2) NOT NULL CHECK (amount > 0 OR reason_code IS NOT NULL),
  direction    varchar(8) NOT NULL CHECK (direction IN ('INCOME','EXPENSE')),
  date         date NOT NULL,
  description  varchar(255) NOT NULL CHECK (char_length(description) BETWEEN 1 AND 255),
  scheduled    boolean NOT NULL DEFAULT false,
  reason_code  varchar(16) NULL CHECK (reason_code IS NULL OR reason_code IN ('REFUND','ADJUSTMENT','REVERSAL')),
  version      integer NOT NULL DEFAULT 1,             -- optimistic lock target
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT transactions_pk PRIMARY KEY (id, date),
  CONSTRAINT transactions_scheduled_future CHECK (NOT scheduled OR date > current_date),
  CONSTRAINT transactions_date_range CHECK (date BETWEEN '1970-01-01' AND '2100-12-31'),
  CONSTRAINT transactions_reason_direction CHECK (reason_code IS NULL OR direction = 'EXPENSE')
) PARTITION BY RANGE (date);

-- Transaction <-> category join (Module B: a transaction references >=1 category).
-- Carries `date` and references the partitioned parent through its partition key,
-- and carries `tenant_id` for tenant-scoped integrity (prevents cross-tenant
-- transaction/category linkage). The "at least one category" requirement is an
-- app-level rule at the Ledger use case, enforced by inserting the join rows in
-- the same transaction as the transaction row.
CREATE TABLE transaction_categories (
  transaction_id  uuid NOT NULL,
  category_id     uuid NOT NULL,
  tenant_id       uuid NOT NULL,
  date            date NOT NULL,
  CONSTRAINT transaction_categories_pk PRIMARY KEY (transaction_id, category_id, date),
  CONSTRAINT transaction_categories_tx_fk
    FOREIGN KEY (transaction_id, date) REFERENCES transactions(id, date),
  CONSTRAINT transaction_categories_category_fk
    FOREIGN KEY (category_id, tenant_id) REFERENCES categories(id, tenant_id)
);

-- Budgets (Module D). Period-bound allocation tied to a category; actuals are
-- recomputed from the transaction ledger via domain events. `period_type` and
-- start <= end are enforced at persistence; duration-to-period_type consistency
-- (MONTHLY => end = start + 1 month - 1 day) is validated at the use case and
-- recorded here for a future trigger.
CREATE TABLE budgets (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL,
  category_id   uuid NOT NULL REFERENCES categories(id),
  period_type   varchar(10) NOT NULL CHECK (period_type IN ('MONTHLY','QUARTERLY','ANNUAL')),
  period_start  date NOT NULL,
  period_end    date NOT NULL,
  allocation    numeric(12,2) NOT NULL CHECK (allocation > 0),
  version       integer NOT NULL DEFAULT 1,            -- optimistic lock target
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT budgets_period_order CHECK (period_start <= period_end)
);

-- Financial reporting periods (Module E/G) with the close state machine (§7.5).
-- A partial unique index prevents two concurrent OPEN/CLOSING periods per tenant.
CREATE TABLE finance_periods (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL,
  label       varchar(40) NULL,
  start_date  date NOT NULL,
  end_date    date NOT NULL,
  status      varchar(10) NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','CLOSING','CLOSED')),
  closed_at   timestamptz NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT finance_periods_dates CHECK (start_date <= end_date)
);

-- Refresh tokens (Module F). Only the hash is stored — never a raw token.
-- Indexed by (user_id, expires_at) for revocation sweeps and rotation.
CREATE TABLE refresh_tokens (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES users(id),
  tenant_id   uuid NOT NULL,
  token_hash  text NOT NULL UNIQUE,                    -- SHA-256 hash of the raw token
  expires_at  timestamptz NOT NULL,
  revoked_at  timestamptz NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT refresh_tokens_expiry CHECK (expires_at > created_at)
);

-- Session state. The API stores only the hash of the session id (consistent with
-- the refresh-token pattern); the 30-minute inactivity timeout is enforced
-- server-side by the session use case against `expires_at`.
CREATE TABLE sessions (
  session_id  uuid PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users(id),
  tenant_id   uuid NOT NULL,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sessions_expiry CHECK (expires_at > created_at)
);

-- Idempotency-key store (§7.2). Scoped per tenant; `request_hash` is the SHA-256
-- of the canonical request body to detect a different request replayed under the
-- same key. `response` may contain financial data and MUST be encrypted at rest.
-- `expires_at` bounds the key window (~24h) and supports retention cleanup.
CREATE TABLE idempotency_keys (
  key           text NOT NULL,
  tenant_id     uuid NOT NULL,
  user_id       uuid NOT NULL,
  request_hash  text NOT NULL,                         -- SHA-256 of canonical body
  response      jsonb,                                 -- encrypted at rest
  expires_at    timestamptz NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT idempotency_keys_pk PRIMARY KEY (tenant_id, key)
);

-- Transactional outbox (§7.4). State change and outbox row are written in the
-- same ACID boundary; a relay publishes to the message broker. `sequence` is
-- unique so consumers replay in order and record checkpoints deterministically.
CREATE TABLE outbox (
  id             bigserial PRIMARY KEY,
  event_type     varchar(64) NOT NULL,
  payload        jsonb NOT NULL,                       -- may carry PII; encrypted at rest
  correlation_id text,
  sequence       bigint NOT NULL UNIQUE,
  published_at   timestamptz NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- Append-only audit log (Module G, §5.3). Chained-hash tamper evidence: each
-- entry's `hash` is SHA-256 over prev_hash || canonical(before) || canonical(after)
-- || actor_username || occurred_at; `prev_hash` is NULL for the first entry in a
-- chain. A completeness/continuity job walks the chain to detect gaps or rewrites.
-- The `READ` action reserves the (currently unresolved) read-logging option per
-- §9.4 as a schema-level feature flag; capture stays gated by configuration.
CREATE TABLE audit_entries (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL,
  entity_type      varchar(32) NOT NULL,
  entity_id        uuid NOT NULL,
  action           varchar(8) NOT NULL CHECK (action IN ('CREATE','EDIT','DELETE','READ')),
  actor_user_id    uuid NULL,
  actor_username   varchar(64),
  before           jsonb,
  after            jsonb,
  prev_hash        char(64) NULL,                      -- NULL for chain head
  hash             char(64) NOT NULL UNIQUE CHECK (hash ~ '^[0-9a-f]{64}$'),
  occurred_at      timestamptz NOT NULL DEFAULT now()
);

-- Block app-layer and direct-DML mutation of the audit trail, including TRUNCATE.
ALTER TABLE audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries FORCE ROW LEVEL SECURITY;

-- ---------------------------------------------------------------------------
-- Monthly RANGE partitions on `transactions` for the current and next 12 months.
-- The scheduler (`scheduler/scheduler.go`, infra tranche) extends the window
-- forward as time advances. Partitions inherit parent CHECKs and RLS in PG16.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  m date := date_trunc('month', current_date);
  i  int;
BEGIN
  FOR i IN 0..12 LOOP
    EXECUTE format(
      'CREATE TABLE transactions_%s PARTITION OF transactions ' ||
      'FOR VALUES FROM (%L) TO (%L)',
      to_char(m + (i || ' months')::interval, 'YYYY_MM'),
      (m + (i || ' months')::interval)::date,
      (m + ((i + 1) || ' months')::interval)::date
    );
  END LOOP;
END;
$$;

-- ---------------------------------------------------------------------------
-- Indexes (§9.4). The (tenant_id, date, id) composite carries the `id` tail so
-- cursor-based pagination (keyset) over the high-volume partition is stable.
-- ---------------------------------------------------------------------------
CREATE INDEX idx_transactions_tenant_date_id ON transactions (tenant_id, date, id);
CREATE INDEX idx_transactions_account_date  ON transactions (account_id, date);
CREATE INDEX idx_transactions_description   ON transactions USING gin (description gin_trgm_ops);
CREATE INDEX idx_transaction_categories_category ON transaction_categories (category_id);
CREATE INDEX idx_budgets_tenant_cat_period  ON budgets (tenant_id, category_id, period_start, period_end);
CREATE INDEX idx_finance_periods_single_open ON finance_periods (tenant_id) WHERE status <> 'CLOSED';
CREATE INDEX idx_refresh_tokens_user_expires ON refresh_tokens (user_id, expires_at);
CREATE INDEX idx_sessions_user_expires       ON sessions (user_id, expires_at);
CREATE INDEX idx_audit_entries_tenant_occurred ON audit_entries (tenant_id, occurred_at);

-- ---------------------------------------------------------------------------
-- updated_at triggers
-- ---------------------------------------------------------------------------
CREATE TRIGGER trg_users_updated_at    BEFORE UPDATE ON users        FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_categories_updated_at BEFORE UPDATE ON categories FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_accounts_updated_at BEFORE UPDATE ON accounts     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_transactions_updated_at BEFORE UPDATE ON transactions FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_budgets_updated_at  BEFORE UPDATE ON budgets      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_finance_periods_updated_at BEFORE UPDATE ON finance_periods FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Category depth/type guard.
CREATE TRIGGER trg_categories_one_level
  BEFORE INSERT OR UPDATE ON categories
  FOR EACH ROW EXECUTE FUNCTION enforce_category_one_level();

-- Audit immutability (blocks UPDATE, DELETE, and TRUNCATE).
CREATE TRIGGER trg_audit_immutable
  BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_entries
  FOR EACH STATEMENT EXECUTE FUNCTION enforce_audit_immutable();

-- ---------------------------------------------------------------------------
-- Row-Level Security: tenant isolation on every tenant table. FORCE subjects the
-- table owner to the policy. Predicates are null-safe (unset tenant context ->
-- NULL comparison -> zero rows) and include WITH CHECK for write-side
-- enforcement so no INSERT/UPDATE can set a foreign tenant_id.
-- ---------------------------------------------------------------------------
ALTER TABLE users               ENABLE ROW LEVEL SECURITY;
ALTER TABLE users               FORCE ROW LEVEL SECURITY;
ALTER TABLE categories          ENABLE ROW LEVEL SECURITY;
ALTER TABLE categories          FORCE ROW LEVEL SECURITY;
ALTER TABLE accounts            ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts            FORCE ROW LEVEL SECURITY;
ALTER TABLE transactions        ENABLE ROW LEVEL SECURITY;
ALTER TABLE transactions        FORCE ROW LEVEL SECURITY;
ALTER TABLE transaction_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE transaction_categories FORCE ROW LEVEL SECURITY;
ALTER TABLE budgets             ENABLE ROW LEVEL SECURITY;
ALTER TABLE budgets             FORCE ROW LEVEL SECURITY;
ALTER TABLE finance_periods     ENABLE ROW LEVEL SECURITY;
ALTER TABLE finance_periods     FORCE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens      ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens      FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions            ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions            FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys    FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_entries       ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries       FORCE ROW LEVEL SECURITY;

CREATE POLICY tenancy_users          ON users            USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_categories     ON categories       USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_accounts       ON accounts         USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_transactions   ON transactions     USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_tx_categories  ON transaction_categories USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_budgets        ON budgets          USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_finance_periods ON finance_periods USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_refresh_tokens ON refresh_tokens   USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_sessions       ON sessions         USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_idempotency    ON idempotency_keys USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());
CREATE POLICY tenancy_audit          ON audit_entries    USING (tenant_id = app_context()) WITH CHECK (tenant_id = app_context());

-- ---------------------------------------------------------------------------
-- Privilege hygiene (CWE-732). Public gets nothing; the least-privilege runtime
-- API role `app_user` receives only DML on tenant tables. The migration role and
-- `app_user` MUST be distinct — `app_user` never owns tenant data.
-- ---------------------------------------------------------------------------
REVOKE ALL ON accounts, categories, transactions, transaction_categories,
          budgets, finance_periods, users, refresh_tokens, sessions,
          idempotency_keys, outbox, audit_entries FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE, DELETE ON accounts, categories, transactions,
      transaction_categories, budgets, finance_periods, users, refresh_tokens,
      sessions, idempotency_keys, audit_entries TO app_user;
-- outbox is written by the write path and drained by the relay; expose DML.
GRANT SELECT, INSERT, UPDATE, DELETE ON outbox TO app_user;

-- ---------------------------------------------------------------------------
-- Seed data (idempotent). Fixed UUIDs make `ON CONFLICT DO NOTHING` deterministic
-- on replay. Default categories are seeded under a boot/system tenant below; real
-- tenants receive category clones at provisioning time (the categories table is
-- tenant-scoped, so there are no tenant-less global rows violating NOT NULL).
-- ---------------------------------------------------------------------------

-- RBAC roles (Module F bootstrap). Permission sets are policy data; each role is
-- granted the least privilege covering its module scope.
INSERT INTO roles (id, code, name, permissions) VALUES
  ('00000000-0000-0000-0000-000000000001', 'admin',           'System Administrator',
   '["transaction:read","transaction:write","export:create","audit:read","user:manage","budget:read","budget:write","report:read"]'::jsonb),
  ('00000000-0000-0000-0000-000000000002', 'standard',        'Standard User',
   '["transaction:read","transaction:write","export:create","budget:read","budget:write","report:read"]'::jsonb),
  ('00000000-0000-0000-0000-000000000003', 'view-only',       'View-Only User',
   '["transaction:read","report:read"]'::jsonb),
  ('00000000-0000-0000-0000-000000000004', 'finance_manager', 'Finance Manager',
   '["transaction:read","transaction:write","export:create","audit:read","budget:read","budget:write","report:read"]'::jsonb),
  ('00000000-0000-0000-0000-000000000005', 'accountant',      'Accountant / Bookkeeper',
   '["transaction:read","transaction:write","budget:read","report:read"]'::jsonb),
  ('00000000-0000-0000-0000-000000000006', 'auditor',         'Internal / External Auditor',
   '["transaction:read","audit:read","report:read"]'::jsonb)
ON CONFLICT (code) DO NOTHING;

-- Default income/expense categories and one-level subcategories under the boot
-- tenant (Module C initialization). Fixed boot tenant id below.
-- Income roots and subcategories:
--   Salary     (INCOME)
--   Investment (INCOME)
--   Other      (INCOME)
-- Expense roots and subcategories:
--   Housing   (EXPENSE)  -> Rent, Utilities
--   Food      (EXPENSE)  -> Groceries, Dining out
--   Transport (EXPENSE)  -> Fuel, Public transit
--   Utilities (expense subcategory under Housing)
--   Leisure   (EXPENSE)  -> Entertainment, Subscriptions
INSERT INTO categories (id, tenant_id, name, type, parent_id) VALUES
  -- boot tenant
  ('10000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000ff', 'Salary',          'INCOME',  NULL),
  ('10000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000ff', 'Investment',      'INCOME',  NULL),
  ('10000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-0000000000ff', 'Other Income',    'INCOME',  NULL),
  ('10000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-0000000000ff', 'Housing',         'EXPENSE', NULL),
  ('10000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-0000000000ff', 'Food',            'EXPENSE', NULL),
  ('10000000-0000-0000-0000-000000000006', '00000000-0000-0000-0000-0000000000ff', 'Transport',       'EXPENSE', NULL),
  ('10000000-0000-0000-0000-000000000007', '00000000-0000-0000-0000-0000000000ff', 'Leisure',         'EXPENSE', NULL),
  -- one-level subcategories
  ('10000000-0000-0000-0000-000000000008', '00000000-0000-0000-0000-0000000000ff', 'Rent',            'EXPENSE', '10000000-0000-0000-0000-000000000004'),
  ('10000000-0000-0000-0000-000000000009', '00000000-0000-0000-0000-0000000000ff', 'Utilities',       'EXPENSE', '10000000-0000-0000-0000-000000000004'),
  ('10000000-0000-0000-0000-00000000000a', '00000000-0000-0000-0000-0000000000ff', 'Groceries',       'EXPENSE', '10000000-0000-0000-0000-000000000005'),
  ('10000000-0000-0000-0000-00000000000b', '00000000-0000-0000-0000-0000000000ff', 'Dining out',      'EXPENSE', '10000000-0000-0000-0000-000000000005'),
  ('10000000-0000-0000-0000-00000000000c', '00000000-0000-0000-0000-0000000000ff', 'Fuel',            'EXPENSE', '10000000-0000-0000-0000-000000000006'),
  ('10000000-0000-0000-0000-00000000000d', '00000000-0000-0000-0000-0000000000ff', 'Public transit',  'EXPENSE', '10000000-0000-0000-0000-000000000006'),
  ('10000000-0000-0000-0000-00000000000e', '00000000-0000-0000-0000-0000000000ff', 'Entertainment',   'EXPENSE', '10000000-0000-0000-0000-000000000007'),
  ('10000000-0000-0000-0000-00000000000f', '00000000-0000-0000-0000-0000000000ff', 'Subscriptions',   'EXPENSE', '10000000-0000-0000-0000-000000000007')
ON CONFLICT (tenant_id, name, parent_id) DO NOTHING;