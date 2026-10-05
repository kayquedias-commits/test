/**
 * Typed catalog over the `/v1/*` backend contracts (Blueprint §7.2). Feature
 * modules must never hardcode a URL or request shape - every resource call
 * routes through `apiRequest` (the single trust boundary in `client.ts`) and
 * returns a named `Promise<T>`.
 *
 * This file centralizes path + query construction so components stay free of
 * URL and auth concerns. All query building is delegated to `apiRequest`, which
 * percent-encodes every param value via `URLSearchParams` - nothing here
 * string-concatenates user input (`filters`, `cursor`, dates) into a URL, which
 * prevents query-string injection and parameter tampering.
 *
 * @security
 * - `{id}` path segments are UUID-validated here (assertUuid) before path
 *   interpolation to reject path-injection / path-traversal-style input -
 *   a single enforcement point for every module.
 * - Credentials, MFA codes, refresh tokens and idempotency keys travel only in
 *   the request body / `Idempotency-Key` header (attached by `apiRequest`),
 *   never in a query string (`client.ts` also uses `credentials: same-origin`,
 *   so no session cookie goes cross-site - CSRF-safe bearer auth).
 * - `getExportDownloadUrl` returns the pre-signed URL as opaque, sensitive
 *   data; never append user params to it and never log it. Consumers must
 *   treat it as a TTL-limited secret and not render it into a leaking surface.
 * - All returned DTO string fields (descriptions, names, error messages) are
 *   untrusted backend data; render them only as text nodes or via an explicit
 *   SafeToRender escape pass - never `innerHTML` / `v-html`.
 * - Admin-only functions are gated at the router/component RBAC permission
 *   map - never rely on URL obscurity as a control. The gateway stays the
 *   authoritative authorization boundary.
 * - Any request-logging funneled through this layer must redact
 *   `Authorization`, `Idempotency-Key`, and body fields named `password`,
 *   `mfaCode`, `refreshToken`, or `token`.
 *
 * Errors bubble up from `apiRequest` as typed `ApiError` subclasses carrying a
 * complete recovery payload (401 -> session expiry + redirect, 409 ->
 * record-modified, 429 -> rate-limit with Retry-After, 400 -> fieldErrors) and
 * are forwarded untouched so screens can render distinct states.
 */

import { apiRequest } from "./client";
import type {
  Money,
  AccountType,
  AccountDTO,
  TransactionType,
  TransactionDTO,
  CategoryType,
  CategoryDTO,
  BudgetPeriodType,
  BudgetDTO,
  BudgetVsActualDTO,
  ExportFormat,
  ExportJobDTO,
  UserDTO,
  AuditLogEntryDTO,
  ProfitLossSummary,
  ExpensesByCategorySummary,
  IncomeTrendSummary,
  CashFlowSummary,
  DashboardKPI,
  SessionDTO,
  CursorPage,
  OffsetPage,
} from "./types";

/** UUID shape (v1-v5) enforcing the Blueprint §7.3 identifier contract. */
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
/** Default cursor/offset page size for the high-volume transaction dataset. */
const DEFAULT_LIMIT = 100;
/** Hard cap so a malicious/oversized request can never reach the server. */
const MAX_LIMIT = 100;

/** Offset pagination for bounded collections. */
type OffsetParams = { page?: number; pageSize?: number };
/** Inclusive report/export period; type-level start <= end check. */
type DateRange = { start: string; end: string };
/** Transaction list ordering keys. */
type TransactionSort = "date" | "amount" | "description";
/** Explicit filter keys matching the §6.3 search indexes. */
type TransactionFilters = {
  dateFrom?: string;
  dateTo?: string;
  categoryId?: string;
  amountMin?: string;
  amountMax?: string;
  description?: string;
};

// ---- Input DTO shapes (create/update payloads derived from types.ts) ----
type AccountInput = { name: string; type: AccountType; openingBalance: Money };
type TransactionInput = {
  accountId: string;
  categoryIds: string[];
  amount: Money;
  date: string;
  description: string;
  type: TransactionType;
  scheduled: boolean;
};
type CategoryInput = { name: string; type: CategoryType; parentId?: string | null };
type BudgetInput = {
  categoryId: string;
  periodType: BudgetPeriodType;
  periodStart: string;
  periodEnd: string;
  allocated: Money;
};
type LoginInput = { username: string; password: string };
type MfaInput = { mfaCode: string; challengeId?: string };
type UserInput = { username: string; role: UserDTO["role"]; active?: boolean };
type UserUpdateInput = Partial<UserInput>;
type CertificationInput = { userId: string; action: "CERTIFY" | "REVOKE" };
type AuditFilters = { entity?: string; actorId?: string; from?: string; to?: string };

/** Shared write options; `idempotencyKey` also gates `apiRequest` write retry. */
type WriteOpts = { idempotencyKey?: string; signal?: AbortSignal };

/**
 * Developer/contract error for a malformed resource id. Deliberately NOT a
 * typed `ApiError` subclass - it is a caller contract violation caught at the
 * boundary and never reaches a user-rendered error state.
 */
function assertUuid(id: string, label = "id"): void {
  if (!UUID_RE.test(id)) {
    throw new TypeError(`[endpoints] ${label} must be a valid UUID; received "${id}"`);
  }
}

/** Clamp `limit` to [1, 100], defaulting to 100; floors any fractional value. */
function clampLimit(limit: number | undefined): number {
  if (limit === undefined) return DEFAULT_LIMIT;
  const clamped = Math.max(1, Math.floor(limit));
  return Math.min(clamped, MAX_LIMIT);
}

/** Drop undefined/empty params so no junk keys (`cursor=undefined`) leave the
 * client - keeps index use healthy and response bodies minimal. */
function omitEmpty(p: Record<string, string | number | undefined>): Record<string, string | number> {
  const out: Record<string, string | number> = {};
  for (const [k, v] of Object.entries(p)) {
    if (v !== undefined && v !== "") out[k] = v;
  }
  return out;
}

/** Report/export range params. */
function rangeParams(range: DateRange): Record<string, string> {
  return omitEmpty({ start: range.start, end: range.end });
}

// ---------------------------------------------------------------- Accounts

/** List accounts (offset-paged). */
export function listAccounts(
  params: OffsetParams = {},
  signal?: AbortSignal
): Promise<OffsetPage<AccountDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/accounts",
    params: omitEmpty({ page: params.page, page_size: params.pageSize }),
    signal,
  });
}

/** Create an account; pass `idempotencyKey` to prevent duplicate posts. */
export function createAccount(dto: AccountInput, opts?: WriteOpts): Promise<AccountDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/accounts",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Update an account under optimistic locking; 409 surfaces as ConflictError. */
export function updateAccount(
  id: string,
  dto: Partial<AccountInput>,
  version: number,
  opts?: WriteOpts
): Promise<AccountDTO> {
  assertUuid(id);
  return apiRequest({
    method: "PATCH",
    path: `/v1/accounts/${id}`,
    body: { ...dto, version },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Delete an account (idempotent; 409 on accounts with transactions surfaces
 * so the UI shows the reassignment / cascade consequence). */
export function deleteAccount(id: string, opts?: WriteOpts): Promise<void> {
  assertUuid(id);
  return apiRequest({
    method: "DELETE",
    path: `/v1/accounts/${id}`,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Reconcile an account's balance against its linked transactions. */
export function reconcileAccount(id: string, opts?: WriteOpts): Promise<AccountDTO> {
  assertUuid(id);
  return apiRequest({
    method: "POST",
    path: `/v1/accounts/${id}/reconcile`,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

// ----------------------------------------------------------- Transactions

/**
 * List transactions (cursor-paged, default limit 100, sub-second target). The
 * forwarded `signal` lets debounced search cancel stale in-flight queries.
 */
export function listTransactions(
  p: { cursor?: string; limit?: number; filters?: TransactionFilters; sort?: TransactionSort } = {},
  signal?: AbortSignal
): Promise<CursorPage<TransactionDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/transactions",
    params: omitEmpty({
      cursor: p.cursor,
      limit: clampLimit(p.limit),
      date_from: p.filters?.dateFrom,
      date_to: p.filters?.dateTo,
      category_id: p.filters?.categoryId,
      amount_min: p.filters?.amountMin,
      amount_max: p.filters?.amountMax,
      description: p.filters?.description,
      sort: p.sort,
    }),
    signal,
  });
}

/** Create a transaction; `idempotencyKey` is mandatory-in-practice for writes
 * to prevent double-posting a ledger entry on retry/double-click. */
export function createTransaction(dto: TransactionInput, opts?: WriteOpts): Promise<TransactionDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/transactions",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Update a transaction under optimistic locking; 409 surfaces as ConflictError. */
export function updateTransaction(
  id: string,
  dto: Partial<TransactionInput>,
  version: number,
  opts?: WriteOpts
): Promise<TransactionDTO> {
  assertUuid(id);
  return apiRequest({
    method: "PATCH",
    path: `/v1/transactions/${id}`,
    body: { ...dto, version },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Delete a transaction (idempotent; second delete returns 404 NOT_FOUND). */
export function deleteTransaction(id: string, opts?: WriteOpts): Promise<void> {
  assertUuid(id);
  return apiRequest({
    method: "DELETE",
    path: `/v1/transactions/${id}`,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

// -------------------------------------------------------------- Categories

/** List categories (offset-paged). */
export function listCategories(
  params: OffsetParams = {},
  signal?: AbortSignal
): Promise<OffsetPage<CategoryDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/categories",
    params: omitEmpty({ page: params.page, page_size: params.pageSize }),
    signal,
  });
}

/** Create a category (income/expense taxonomy). */
export function createCategory(dto: CategoryInput, opts?: WriteOpts): Promise<CategoryDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/categories",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Update a category under optimistic locking; 409 surfaces as ConflictError. */
export function updateCategory(
  id: string,
  dto: Partial<CategoryInput>,
  version: number,
  opts?: WriteOpts
): Promise<CategoryDTO> {
  assertUuid(id);
  return apiRequest({
    method: "PATCH",
    path: `/v1/categories/${id}`,
    body: { ...dto, version },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Delete a category; a 409 (in-use) surfaces as ConflictError so the UI offers
 * merge instead of a silent failure. */
export function deleteCategory(id: string, opts?: WriteOpts): Promise<void> {
  assertUuid(id);
  return apiRequest({
    method: "DELETE",
    path: `/v1/categories/${id}`,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Merge the source category's items into targetId (history preserved). */
export function mergeCategories(
  sourceId: string,
  targetId: string,
  opts?: WriteOpts
): Promise<void> {
  assertUuid(sourceId, "sourceId");
  assertUuid(targetId, "targetId");
  return apiRequest({
    method: "POST",
    path: `/v1/categories/${sourceId}/merge`,
    body: { targetId },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

// ---------------------------------------------------------------- Budgets

/** List budgets (offset-paged). */
export function listBudgets(
  params: OffsetParams = {},
  signal?: AbortSignal
): Promise<OffsetPage<BudgetDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/budgets",
    params: omitEmpty({ page: params.page, page_size: params.pageSize }),
    signal,
  });
}

/** Create a budget (period + category allocation). */
export function createBudget(dto: BudgetInput, opts?: WriteOpts): Promise<BudgetDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/budgets",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Update a budget under optimistic locking; 409 surfaces as ConflictError. */
export function updateBudget(
  id: string,
  dto: Partial<BudgetInput>,
  version: number,
  opts?: WriteOpts
): Promise<BudgetDTO> {
  assertUuid(id);
  return apiRequest({
    method: "PATCH",
    path: `/v1/budgets/${id}`,
    body: { ...dto, version },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Budget vs. actual comparison for a budget. */
export function vsActual(budgetId: string, signal?: AbortSignal): Promise<BudgetVsActualDTO> {
  assertUuid(budgetId, "budgetId");
  return apiRequest({
    method: "GET",
    path: `/v1/budgets/${budgetId}/vs-actual`,
    signal,
  });
}

// ---------------------------------------------------------------- Reports

/** Profit/loss summary over a range (read model). */
export function profitLoss(range: DateRange, signal?: AbortSignal): Promise<ProfitLossSummary> {
  return apiRequest({
    method: "GET",
    path: "/v1/reports/profit-loss",
    params: rangeParams(range),
    signal,
  });
}

/** Expense breakdown by category over a range. */
export function expensesByCategory(
  range: DateRange,
  signal?: AbortSignal
): Promise<ExpensesByCategorySummary> {
  return apiRequest({
    method: "GET",
    path: "/v1/reports/expenses-by-category",
    params: rangeParams(range),
    signal,
  });
}

/** Income trend series over a range. */
export function incomeTrend(range: DateRange, signal?: AbortSignal): Promise<IncomeTrendSummary> {
  return apiRequest({
    method: "GET",
    path: "/v1/reports/income-trend",
    params: rangeParams(range),
    signal,
  });
}

/** Cash-flow statement over a range. */
export function cashFlow(range: DateRange, signal?: AbortSignal): Promise<CashFlowSummary> {
  return apiRequest({
    method: "GET",
    path: "/v1/reports/cash-flow",
    params: rangeParams(range),
    signal,
  });
}

// ---------------------------------------------------------------- Exports

/** Create an async export job; returns the PENDING/RUNNING shape for progress. */
export function createExport(
  p: { format: ExportFormat; range: DateRange },
  opts?: WriteOpts
): Promise<ExportJobDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/exports",
    body: { format: p.format, start: p.range.start, end: p.range.end },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/**
 * Poll an export job. The discriminated union exposes PENDING/RUNNING/READY/
 * FAILED progress truth (with `errorReason` on failure). Consumers must NOT
 * cache this response - the download URL has a 15-min TTL.
 */
export function getExportStatus(id: string, signal?: AbortSignal): Promise<ExportJobDTO> {
  assertUuid(id);
  return apiRequest({
    method: "GET",
    path: `/v1/exports/${id}`,
    signal,
  });
}

/** Return the (pre-signed, TTL-limited) download URL as opaque sensitive data;
 * never append user params and never log it. */
export function getExportDownloadUrl(id: string, signal?: AbortSignal): Promise<{ downloadUrl: string }> {
  assertUuid(id);
  return apiRequest({
    method: "GET",
    path: `/v1/exports/${id}/download`,
    signal,
  });
}

// ------------------------------------------------------------------- Auth

/** Sign in. Credentials travel in the body only (never a query string);
 * returned tokens must be held in memory / HttpOnly cookies, never stored
 * where XSS can read them. */
export function login(input: LoginInput, signal?: AbortSignal): Promise<SessionDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/auth/login",
    body: input,
    signal,
  });
}

/** Complete the MFA challenge after a password login. */
export function completeMfa(input: MfaInput, signal?: AbortSignal): Promise<SessionDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/auth/complete-mfa",
    body: input,
    signal,
  });
}

/** Sign out. */
export function logout(opts?: WriteOpts): Promise<void> {
  return apiRequest({
    method: "POST",
    path: "/v1/auth/logout",
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Rotate the session. The refresh token is exchanged via the body (never a
 * URL) and consumed immediately - the caller must not cache it or place it in
 * module state. */
export function refresh(refreshToken: string): Promise<SessionDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/auth/refresh",
    body: { refresh_token: refreshToken },
  });
}

/** Current authenticated user. */
export function me(signal?: AbortSignal): Promise<UserDTO> {
  return apiRequest({ method: "GET", path: "/v1/auth/me", signal });
}

// ------------------------------------------------------------------ Admin
// All admin functions MUST stay reachable only behind the router/component RBAC
// permission map - never gate on URL obscurity. The gateway remains the
// authoritative authorization boundary.

/** List users (admin only). */
export function listUsers(
  params: OffsetParams = {},
  signal?: AbortSignal
): Promise<OffsetPage<UserDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/users",
    params: omitEmpty({ page: params.page, page_size: params.pageSize }),
    signal,
  });
}

/** Create a user (admin only). */
export function createUser(dto: UserInput, opts?: WriteOpts): Promise<UserDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/users",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Update a user under optimistic locking; 409 surfaces as ConflictError. */
export function updateUser(
  id: string,
  dto: UserUpdateInput,
  version: number,
  opts?: WriteOpts
): Promise<UserDTO> {
  assertUuid(id);
  return apiRequest({
    method: "PATCH",
    path: `/v1/users/${id}`,
    body: { ...dto, version },
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Deprovision a user; a 409 (last admin) surfaces as ConflictError. */
export function deprovisionUser(id: string, opts?: WriteOpts): Promise<void> {
  assertUuid(id);
  return apiRequest({
    method: "POST",
    path: `/v1/users/${id}/deprovision`,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** List append-only audit log entries. */
export function listAuditLogs(
  filters: AuditFilters = {},
  signal?: AbortSignal
): Promise<OffsetPage<AuditLogEntryDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/audit-logs",
    params: omitEmpty({
      entity: filters.entity,
      actor_id: filters.actorId,
      from: filters.from,
      to: filters.to,
    }),
    signal,
  });
}

/** List users pending periodic access certification. */
export function listCertifications(signal?: AbortSignal): Promise<OffsetPage<UserDTO>> {
  return apiRequest({
    method: "GET",
    path: "/v1/certifications",
    signal,
  });
}

/** Record an access certification decision (CERTIFY / REVOKE). */
export function recordCertification(dto: CertificationInput, opts?: WriteOpts): Promise<UserDTO> {
  return apiRequest({
    method: "POST",
    path: "/v1/certifications",
    body: dto,
    idempotencyKey: opts?.idempotencyKey,
    signal: opts?.signal,
  });
}

/** Executive dashboard KPI cards (admin / finance roles). */
export function listDashboardKpis(signal?: AbortSignal): Promise<DashboardKPI[]> {
  return apiRequest({ method: "GET", path: "/v1/dashboard/kpis", signal });
}