/**
 * Presentational RBAC gate for the UI.
 *
 * This is a UI-only, least-privilege affordance gate: it decides which
 * Create/Edit/Delete controls may be rendered. It is NOT a security control —
 * the backend enforces the same permission matrix at the resource boundary
 * (see technical spec §7.8/§8.4), so every mutating action is rejected with
 * HTTP 403 regardless of what this hook returns.
 *
 * Contract:
 * - Fail-closed on unresolved role: while `selectRole` is null (unauthenticated
 *   or session still hydrating), `can()`/`canAny()`/`canAll()` all return
 *   `false`, so no unauthorized affordance ever flashes and there is no CLS on
 *   hydration. Consumers should let skeleton screens hold layout until the
 *   role resolves.
 * - Hidden, not disabled: when a permission check is false the consumer must
 *   remove the control from the DOM (conditional mount / v-if), never render a
 *   `disabled` control or an `aria-hidden`-on-focusable element. A hidden
 *   control leaves no tab stop and no accessibility residue.
 * - `role` is exposed for display/diagnostics only. Components must never gate
 *   on `role` directly — that would fragment the permission source of truth.
 * - Memoized per permission: `selectRole` returns a primitive string so the
 *   granted set key is stable; `canAny`/`canAll` aggregate over that memoized
 *   set instead of re-running the permission-map lookup, keeping grouped
 *   route/nav guards cheap (INP budget).
 *
 * @security The checks below are strict O(1) set lookups with no regex, URL
 * parsing, or DOM work on inputs, and there is intentionally no `can('*')`
 * wildcard. The granted set mirrors the pure `can()` matrix in
 * app/permissions.ts, keeping the UI mirror aligned with server enforcement.
 */
import { useCallback, useMemo } from "react";
import { permissionsFor } from "../app/permissions";
import { selectRole, useSession } from "../app/stores/session";
import type { Permission, UserRole } from "../services/api/types";

export interface UsePermission {
  /** Authenticated session role, or null while unauthenticated/hydrating. */
  role: UserRole | null;
  /** True only if `permission` is granted for the current session role. */
  can: (permission: Permission, resource?: unknown) => boolean;
  /** True if any of the listed permissions is granted. */
  canAny: (permissions: readonly Permission[]) => boolean;
  /** True if every listed permission is granted. */
  canAll: (permissions: readonly Permission[]) => boolean;
}

/**
 * Subscribes to the session role and exposes memoized permission checks.
 *
 * The granted set is recomputed only when the role string changes. When the
 * role is null the granted set is empty, making every check fail closed.
 */
export function usePermission(): UsePermission {
  const role = useSession(selectRole);
  const granted = useMemo(
    () => new Set<Permission>(role ? permissionsFor(role) : []),
    [role],
  );

  const can = useCallback(
    (permission: Permission, _resource?: unknown) => granted.has(permission),
    [granted],
  );

  const canAny = useCallback(
    (permissions: readonly Permission[]) =>
      permissions.some((permission) => granted.has(permission)),
    [granted],
  );

  // `canAll([])` vacuously returns true; treat an empty list as a call-site
  // error rather than adding a defensive special case here.
  const canAll = useCallback(
    (permissions: readonly Permission[]) =>
      permissions.every((permission) => granted.has(permission)),
    [granted],
  );

  return { role, can, canAny, canAll };
}