/**
 * DTO contracts mirroring the backend JSON schema (Blueprint §7.3,
 * `application/vnd.fms.*.v1+json`).
 *
 * This is a pure type-level module: no runtime DOM, no third-party imports.
 * Enums are `as const` string-literal unions; type guards are pure predicates
 * used at API-ingestion time for schema-drift checks (dev-only strict mode),
 * never in the render path.
 *
 * @security All message/error/description/name fields are untrusted strings
 * coming from the backend. Render them only as text nodes (or through an
 * explicit escape/SafeToRender pass) - never via innerHTML / v-html. No
 * `eval` / `new Function` is used anywhere, keeping this module compatible
 * with strict CSP `script-src` (no `unsafe-eval`).
 */

/** Marker interface forcing untrusted strings to be explicitly sanitized
 * ("SafeToRender" pass) before reaching a DOM renderer. A raw backend string
 * must be converted via an escape helper; this branded type records that the
 * conversion happened so the render path cannot accept a raw untrusted value. */
export interface SafeToRender<T extends string = string> {
  readonly __brand: "SafeToRender";
  readonly value: T;
}

/** ISO 4217 currency code, constrained to the uppercase three-letter shape. */
export type IsoCurrency = Uppercase<string>;
// @security currency is derived from backend data; normalize uppercased and
// validate against ^[A-Z]{3}$ before formatting.

/**
 * Monetary value. `amount` is a decimal string in minor-unit precision
 * (validator-enforced, matching currency.ts: exactly two decimal places) -
 * NEVER a binary float, to avoid floating-point rounding errors in displayed
 * balances. `currency` is an ISO 4217 code.
 */
export interface Money {
  /** Integer/decimal string, two decimal places max (e.g. "1234.50"). */
  amount: string;
  /** ISO 4217 uppercase code (e.g. "USD"). */
  currency: IsoCurrency;
}

/** Account classification. */
export type AccountType = "checking" | "savings" | "credit" | "cash" | "other";

/** Direction of a transaction entry. */
export type TransactionType = "INCOME" | "EXPENSE";

/**
 * Financial account. `version` is the optimistic-lock counter; when a write
 * returns 409 CONFLICT_STATE, the UI surfaces the "record modified by another
 * user / Refresh" recovery path rather than silently overwriting.
 */
export interface AccountDTO {
  id: string;
  name: string;
  type: AccountType;
  openingBalance: Money;
  currentBalance: Money;
  version: number;
  createdAt: string;
}

/**
 * Ledger entry. `date` is an ISO 8601 UTC string (`YYYY-MM-DD`); locale
 * formatting is applied only at presentation. `description` is capped at 255
 * characters (guard-enforced). `scheduled` drives the visible "Scheduled" tag
 * for future-dated entries. `categoryIds` is non-empty (>= 1 category).
 */
export interface TransactionDTO {
  id: string;
  accountId: string;
  categoryIds: string[];
  amount: Money;
  date: string;
  description: string;
  type: TransactionType;
  scheduled: boolean;
  version: number;
}

/** Direction of a category (income or expense taxonomy). */
export type CategoryType = "INCOME" | "EXPENSE";

/** Category (or one-level subcategory) in the taxonomy. */
export interface CategoryDTO {
  id: string;
  name: string;
  type: CategoryType;
  parentId: string | null;
  inUse: boolean;
  version: number;
}

/** Budget cycle: monthly, quarterly, or annual. */
export type BudgetPeriodType = "MONTHLY" | "QUARTERLY" | "ANNUAL";

/**
 * Period-bound allocation tied to a category. `periodStart` <= `periodEnd`
 * (guard-checked); `version` is the optimistic-lock counter for the 409
 * recovery UI.
 */
export interface BudgetDTO {
  id: string;
  categoryId: string;
  periodType: BudgetPeriodType;
  periodStart: string;
  periodEnd: string;
  allocated: Money;
  version: number;
}

/**
 * Budget vs. actual comparison. `overrun`/`approaching` are semantic booleans
 * only; color is a pure presentation concern applied by the progress-bar
 * component (WCAG 1.4.1: color is never the only indicator - pair with icon +
 * text). `pct` is a percentage 0-100.
 */
export interface BudgetVsActualDTO {
  allocated: Money;
  spent: Money;
  pct: number;
  overrun: boolean;
  approaching: boolean;
}

/** Supported report/transaction export formats. */
export type ExportFormat = "CSV" | "PDF";

/** Lifecycle of an asynchronous export job. */
export type ExportJobStatus = "PENDING" | "RUNNING" | "READY" | "FAILED";

/**
 * Asynchronous export job, discriminated on `status` so impossible UI states
 * cannot type-check: `errorReason` exists only on FAILED and `downloadUrl`
 * only on READY.
 */
export type ExportJobDTO =
  | { id: string; format: ExportFormat; createdAt: string; status: "PENDING" }
  | { id: string; format: ExportFormat; createdAt: string; status: "RUNNING" }
  | { id: string; format: ExportFormat; createdAt: string; status: "FAILED"; errorReason: string }
  | { id: string; format: ExportFormat; createdAt: string; status: "READY"; downloadUrl: string };

/** Business role. Drives role-tailored dashboards and RBAC gating. */
export type UserRole =
  | "ADMIN"
  | "FINANCE_MANAGER"
  | "ACCOUNTANT"
  | "AUDITOR"
  | "BUDGET_OWNER"
  | "VIEW_ONLY"
  | "EMPLOYEE";

/** Fine-grained permission. A single RBAC map consumes these to gate routes,
 * navigation, and action buttons (view-only users render no Create/Edit/Delete
 * affordances - hidden, not merely disabled). */
export type Permission =
  | "account:read"
  | "account:write"
  | "transaction:read"
  | "transaction:write"
  | "category:read"
  | "category:write"
  | "budget:read"
  | "budget:write"
  | "report:read"
  | "export:read"
  | "export:create"
  | "admin:users"
  | "audit:read";

/** System user with role and derived permission set. */
export interface UserDTO {
  id: string;
  username: string;
  role: UserRole;
  permissions: Permission[];
  active: boolean;
  lastLoginAt: string | null;
}

/** Operations recorded in the audit log. `READ` pre-supports configurable
 * read-access logging (Blueprint §9.4) without a breaking type change. */
export type AuditAction = "CREATE" | "EDIT" | "DELETE" | "READ";

/**
 * Append-only audit entry.
 * @security `before`/`after` are untrusted unvalidated snapshots - render only
 * via safe text/escaped interpolation, never innerHTML/v-html. Entries are
 * immutable; never treat them as persistence-trusted.
 */
export interface AuditLogEntryDTO {
  id: string;
  action: AuditAction;
  actorId: string;
  entity: string;
  entityId: string;
  timestamp: string;
  before?: unknown;
  after?: unknown;
}

/** Single profit-and-loss line item. */
export interface ProfitLossLine {
  categoryId: string | null;
  categoryName: string | null;
  type: CategoryType;
  total: Money;
}

/** Empty datasets must yield empty summaries (`items` is empty-allowed), never
 * an error. */
export interface ProfitLossSummary {
  periodStart: string;
  periodEnd: string;
  income: Money;
  expense: Money;
  net: Money;
  items: ProfitLossLine[];
}

/** Per-category expense total within a report range. */
export interface CategoryTotal {
  categoryId: string;
  categoryName: string;
  total: Money;
  pct: number;
}

/** Empty datasets yield an empty summary, never an error. */
export interface ExpensesByCategorySummary {
  periodStart: string;
  periodEnd: string;
  totalExpenses: Money;
  items: CategoryTotal[];
}

/** Single point along an income-trend series. */
export interface TrendPoint {
  period: string;
  amount: Money;
}

/** Empty datasets yield an empty summary, never an error. */
export interface IncomeTrendSummary {
  periodStart: string;
  periodEnd: string;
  items: TrendPoint[];
}

/** Single cash-flow movement line. */
export interface CashFlowLine {
  date: string;
  description: string;
  amount: Money;
  type: TransactionType;
}

/** Empty datasets yield an empty summary, never an error. */
export interface CashFlowSummary {
  periodStart: string;
  periodEnd: string;
  openingBalance: Money;
  netCashFlow: Money;
  closingBalance: Money;
  items: CashFlowLine[];
}

/** Dashboard KPI card. The dashboard renders no more than 7-9 cards
 * (Miller's Law); `hint` offers optional drill-down guidance. */
export interface DashboardKPI {
  id: string;
  label: string;
  value: Money;
  hint?: string;
}

/** Standard API error codes (Blueprint §7.7). */
export type ApiErrorCode =
  | "VALIDATION_FAILED"
  | "UNAUTHENTICATED"
  | "FORBIDDEN"
  | "NOT_FOUND"
  | "CONFLICT_STATE"
  | "RATE_LIMITED"
  | "DEPENDENCY_UNAVAILABLE"
  | "INTERNAL_ERROR";

/**
 * Aggregate field-level validation error.
 * @security `message` is an untrusted unsafe-string - bind as a text node
 * only. `field` keys off the input's `aria-describedby` inline-error linkage
 * (WCAG §8.3).
 */
export interface FieldError {
  field: string;
  message: string;
}

/**
 * RFC 7807-style error envelope. `message` is human and solution-oriented with
 * no stack traces / internal state. `correlation_id` enables support tracing.
 * 403 (FORBIDDEN) routes to the insufficient-permissions state (never a blank
 * screen); 409 (CONFLICT_STATE) routes to the "record modified by another
 * user / Refresh" recovery UI.
 * @security `message` and `field_errors[].message` must be rendered as text
 * nodes, never innerHTML/v-html.
 */
export interface ApiErrorEnvelope {
  error: {
    code: ApiErrorCode;
    message: string;
    field_errors?: FieldError[];
    correlation_id: string;
  };
}

/** Cursor-based page of items (transactions, the high-volume dataset). The
 * terminal page renders a clean end state when next_cursor is null/absent. */
export interface CursorPage<T> {
  items: T[];
  next_cursor?: string | null;
  prev_cursor?: string | null;
  limit: number;
}

/** Offset-based page for bounded collections (accounts, categories, budgets,
 * users). */
export interface OffsetPage<T> {
  items: T[];
  page: number;
  page_size: number;
  total: number;
  next_page?: number | null;
}

/**
 * Authentication session response. `mfa_required` drives the password ->
 * MFA-challenge second step (goal-gradient progression).
 * @security access_token/refresh_token are sensitive credentials - store in
 * memory or HttpOnly Secure SameSite=Strict cookies; NEVER in
 * localStorage/sessionStorage (XSS-readable). Never log token payloads.
 * Guards fail closed if a token field contains HTML/script markers.
 */
export interface SessionDTO {
  access_token: string;
  refresh_token: string;
  user: UserDTO;
  mfa_required: boolean;
}

/* ---------------------------------------------------------------------
 * Type guards
 *
 * Pure predicate functions for API-ingestion validation and dev-time
 * schema-drift checks. They are intentionally NOT used in the render path:
 * evaluating guards per row on hot 100-row transaction lists would inflate INP
 * past the 200ms target. A failed guard results in a generic error boundary
 * state at the API layer - never partially-rendered data. No eval / new
 * Function is used (CSP script-src compliance).
 * --------------------------------------------------------------------- */

const AMOUNT_RE = /^[+-]?[0-9]{1,15}(\.[0-9]{1,2})?$/;
const CURRENCY_RE = /^[A-Z]{3}$/;
const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isString(value: unknown): value is string {
  return typeof value === "string";
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

function isBoolean(value: unknown): value is boolean {
  return typeof value === "boolean";
}

/** Structural + content guard: `amount` is a two-decimal string, `currency` is
 * a three-letter uppercase code. Rejects binary floats by construction. */
export function isMoney(value: unknown): value is Money {
  if (!isRecord(value)) return false;
  if (!isString(value.amount) || !AMOUNT_RE.test(value.amount)) return false;
  if (!isString(value.currency) || !CURRENCY_RE.test(value.currency)) return false;
  return true;
}

function isStringArray(value: unknown, minLength = 0): value is string[] {
  if (!Array.isArray(value)) return false;
  if (value.length < minLength) return false;
  return value.every(isString);
}

function hasVersion(rec: Record<string, unknown>): boolean {
  return typeof rec.version === "number" && Number.isInteger(rec.version) && rec.version >= 0;
}

/** Guard enforcing structure AND content constraints (description <= 255,
 * non-empty categoryIds, valid money + ISO date). */
export function isTransactionDTO(value: unknown): value is TransactionDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id)) return false;
  if (!isString(value.accountId)) return false;
  if (!isStringArray(value.categoryIds, 1)) return false;
  if (!isMoney(value.amount)) return false;
  if (!isString(value.date) || !ISO_DATE_RE.test(value.date)) return false;
  if (!isString(value.description) || value.description.length > 255) return false;
  if (value.type !== "INCOME" && value.type !== "EXPENSE") return false;
  if (!isBoolean(value.scheduled)) return false;
  return hasVersion(value);
}

export function isAccountDTO(value: unknown): value is AccountDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id)) return false;
  if (!isString(value.name)) return false;
  if (
    value.type !== "checking" &&
    value.type !== "savings" &&
    value.type !== "credit" &&
    value.type !== "cash" &&
    value.type !== "other"
  )
    return false;
  if (!isMoney(value.openingBalance) || !isMoney(value.currentBalance)) return false;
  return hasVersion(value);
}

export function isCategoryDTO(value: unknown): value is CategoryDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id) || !isString(value.name)) return false;
  if (value.type !== "INCOME" && value.type !== "EXPENSE") return false;
  if (value.parentId !== null && !isString(value.parentId)) return false;
  if (!isBoolean(value.inUse)) return false;
  return hasVersion(value);
}

export function isBudgetDTO(value: unknown): value is BudgetDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id) || !isString(value.categoryId)) return false;
  if (
    value.periodType !== "MONTHLY" &&
    value.periodType !== "QUARTERLY" &&
    value.periodType !== "ANNUAL"
  )
    return false;
  if (!isString(value.periodStart) || !ISO_DATE_RE.test(value.periodStart)) return false;
  if (!isString(value.periodEnd) || !ISO_DATE_RE.test(value.periodEnd)) return false;
  if (value.periodStart > value.periodEnd) return false;
  if (!isMoney(value.allocated)) return false;
  return hasVersion(value);
}

export function isBudgetVsActualDTO(value: unknown): value is BudgetVsActualDTO {
  if (!isRecord(value)) return false;
  if (!isMoney(value.allocated) || !isMoney(value.spent)) return false;
  if (!isFiniteNumber(value.pct) || value.pct < 0 || value.pct > 100) return false;
  if (!isBoolean(value.overrun) || !isBoolean(value.approaching)) return false;
  return true;
}

export function isExportJobDTO(value: unknown): value is ExportJobDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id)) return false;
  if (value.format !== "CSV" && value.format !== "PDF") return false;
  if (!isString(value.createdAt)) return false;
  switch (value.status) {
    case "PENDING":
    case "RUNNING":
      return true;
    case "FAILED":
      return typeof value.errorReason === "string" && value.errorReason.length > 0;
    case "READY":
      return isString(value.downloadUrl) && value.downloadUrl.length > 0;
    default:
      return false;
  }
}

const USER_ROLES: ReadonlyArray<UserRole> = [
  "ADMIN",
  "FINANCE_MANAGER",
  "ACCOUNTANT",
  "AUDITOR",
  "BUDGET_OWNER",
  "VIEW_ONLY",
  "EMPLOYEE",
];

const PERMISSIONS: ReadonlyArray<Permission> = [
  "account:read",
  "account:write",
  "transaction:read",
  "transaction:write",
  "category:read",
  "category:write",
  "budget:read",
  "budget:write",
  "report:read",
  "export:read",
  "export:create",
  "admin:users",
  "audit:read",
];

export function isUserDTO(value: unknown): value is UserDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id) || !isString(value.username)) return false;
  if (!USER_ROLES.includes(value.role as UserRole)) return false;
  if (!Array.isArray(value.permissions)) return false;
  if (!value.permissions.every((p) => PERMISSIONS.includes(p as Permission))) return false;
  if (!isBoolean(value.active)) return false;
  if (value.lastLoginAt !== null && !isString(value.lastLoginAt)) return false;
  return true;
}

export function isAuditLogEntryDTO(value: unknown): value is AuditLogEntryDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.id) || !isString(value.actorId)) return false;
  if (
    value.action !== "CREATE" &&
    value.action !== "EDIT" &&
    value.action !== "DELETE" &&
    value.action !== "READ"
  )
    return false;
  if (!isString(value.entity) || !isString(value.entityId)) return false;
  if (!isString(value.timestamp)) return false;
  return true;
}

export function isFieldError(value: unknown): value is FieldError {
  if (!isRecord(value)) return false;
  return isString(value.field) && isString(value.message);
}

export function isApiErrorEnvelope(value: unknown): value is ApiErrorEnvelope {
  if (!isRecord(value)) return false;
  const err = value.error;
  if (!isRecord(err)) return false;
  const codes: Array<ApiErrorCode> = [
    "VALIDATION_FAILED",
    "UNAUTHENTICATED",
    "FORBIDDEN",
    "NOT_FOUND",
    "CONFLICT_STATE",
    "RATE_LIMITED",
    "DEPENDENCY_UNAVAILABLE",
    "INTERNAL_ERROR",
  ];
  if (!codes.includes(err.code as ApiErrorCode)) return false;
  if (!isString(err.message)) return false;
  if (!isString(err.correlation_id)) return false;
  if (err.field_errors !== undefined && !Array.isArray(err.field_errors)) return false;
  if (err.field_errors !== undefined && !err.field_errors.every(isFieldError)) return false;
  return true;
}

export function isCursorPage<T>(value: unknown, itemGuard: (item: unknown) => item is T): value is CursorPage<T> {
  if (!isRecord(value)) return false;
  if (!Array.isArray(value.items) || !value.items.every(itemGuard)) return false;
  if (!isFiniteNumber(value.limit) || value.limit < 0) return false;
  if (value.next_cursor !== undefined && value.next_cursor !== null && !isString(value.next_cursor)) return false;
  if (value.prev_cursor !== undefined && value.prev_cursor !== null && !isString(value.prev_cursor)) return false;
  return true;
}

export function isOffsetPage<T>(value: unknown, itemGuard: (item: unknown) => item is T): value is OffsetPage<T> {
  if (!isRecord(value)) return false;
  if (!Array.isArray(value.items) || !value.items.every(itemGuard)) return false;
  if (!isFiniteNumber(value.page) || !isFiniteNumber(value.page_size) || !isFiniteNumber(value.total)) return false;
  if (value.next_page !== undefined && value.next_page !== null && !isFiniteNumber(value.next_page)) return false;
  return true;
}

/** Fail-closed on SessionDTO: token fields must be non-empty strings with no
 * HTML/script markers. */
export function isSessionDTO(value: unknown): value is SessionDTO {
  if (!isRecord(value)) return false;
  if (!isString(value.access_token) || value.access_token.length === 0 || /[<>]/.test(value.access_token)) {
    return false;
  }
  if (!isString(value.refresh_token) || value.refresh_token.length === 0 || /[<>]/.test(value.refresh_token)) {
    return false;
  }
  if (!isBoolean(value.mfa_required)) return false;
  return isUserDTO(value.user);
}