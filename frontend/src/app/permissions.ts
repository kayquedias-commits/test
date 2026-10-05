/**
 * Role-based access control permission map (policy as data, Blueprint §8.1/§9.7).
 *
 * Pure module: no React imports, no DOM, no async - `can` resolves synchronously
 * so permission gating never blocks render (Doherty's Law). This is a UI-level
 * gate only; the backend enforces the same matrix at the resource boundary (§7.8).
 *
 * @security Fail-closed by construction: any role not present in PERMISSION_MAP
 * resolves to the empty permission set, so unknown roles are denied everything.
 * Roles/permissions are compile-time typed string-literal unions, eliminating
 * case-sensitivity or typo bypasses. All values are static literals consumed as
 * typed tokens - never interpolated into innerHTML/v-html or dynamic CSS.
 */
import type { Permission, UserRole } from "../services/api/types";

/** Signature of the sync permission resolver. Consumed by `usePermission` and
 * `Navigation`; returns `true` only when the role's granted set includes the
 * permission. */
export type CanFn = (permission: Permission, role: UserRole, resource?: unknown) => boolean;

/**
 * Single source of truth for which permissions each role holds. Business roles
 * reference the same permission tokens, so future matrix changes are data edits
 * here rather than code edits.
 *
 * READ-ONLY note: view-only roles (VIEW_ONLY, AUDITOR, EMPLOYEE) carry no write
 * tokens and no `export:create` (least-privilege default per §9.7) - consumers
 * must HIDE those affordances entirely, never render disabled controls.
 *
 * WRITE grants (e.g. BUDGET_OWNER's `budget:write`) are role-wide in this
 * matrix. Consumers must keep such actions VISIBLE but gate per-resource via
 * `can(..., resource)` with an explanatory disabled tooltip when ownership
 * fails, so legitimate actions are never removed.
 */
export const PERMISSION_MAP: Record<UserRole, readonly Permission[]> = {
  ADMIN: [
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
  ],
  FINANCE_MANAGER: [
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
    "audit:read",
  ],
  ACCOUNTANT: [
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
  ],
  AUDITOR: [
    "account:read",
    "transaction:read",
    "category:read",
    "budget:read",
    "report:read",
    "export:read",
    "audit:read",
  ],
  BUDGET_OWNER: [
    "account:read",
    "transaction:read",
    "category:read",
    "budget:read",
    "budget:write",
    "report:read",
    "export:read",
  ],
  VIEW_ONLY: [
    "account:read",
    "transaction:read",
    "category:read",
    "budget:read",
    "report:read",
    "export:read",
  ],
  EMPLOYEE: ["transaction:read", "category:read", "report:read", "export:read"],
};

/** Resolves the granted permission set for a role. Unknown roles resolve to the
 * empty set (deny-all), keeping navigation/action filtering single-source and
 * fail-closed. */
export function permissionsFor(role: UserRole): readonly Permission[] {
  return PERMISSION_MAP[role] ?? [];
}

/**
 * Resolves whether `role` holds `permission`. The optional `resource` argument
 * is accepted for ownership-scoped gating but not used here - all grants in this
 * matrix are role-wide. Per-resource ownership (e.g. BUDGET_OWNER editing only
 * its own budgets) is applied by consumers passing the actual resource before
 * rendering, and the backend re-validates ownership at the resource boundary.
 */
export function can(permission: Permission, role: UserRole, _resource?: unknown): boolean {
  return permissionsFor(role).includes(permission);
}