/**
 * Admin user-provisioning list (Module F, Blueprint §4.3/8.1).
 *
 * Renders the bounded user collection as a semantic table (username, role
 * badge, active state with icon + text, last login) with offset pagination,
 * debounced username search, and row actions: Edit (navigates the parent to
 * UserFormPage via `onEdit`) and Deprovision (guarded by ConfirmDialog).
 *
 * The page is intentionally store/router-free: session RBAC (`canManageUsers`),
 * navigation (`onEdit`/`onCreateUser`), and toasts (`onToast`) are injected by
 * the parent (sibling to AuditLogsPage/UserFormPage). This keeps the module
 * inside its dependency contract and testable in isolation.
 *
 * @security
 * - XSS (CWE-79): every user-supplied string (`username`, `role` fallback,
 *   `lastLoginAt`) is rendered as a React text node only. There is NO
 *   `dangerouslySetInnerHTML`/`innerHTML` anywhere in this file. The role badge
 *   is built from a static label map with a text fallback, never from string
 *   concatenation into markup.
 * - CSRF (CWE-352): Deprovision routes exclusively through `deprovisionUser` →
 *   `apiRequest`, which attaches `Authorization: Bearer`, an idempotency key,
 *   and `credentials: same-origin` (never `include`). This file introduces no
 *   divergent auth path, and the request fires only after the user explicitly
 *   confirms in ConfirmDialog.
 * - Authorization: the client `canManageUsers` guard is a UX affordance only.
 *   The server (gateway + RBAC) is the authoritative boundary. On a FORBIDDEN
 *   response the page renders an insufficient-permissions state - never a
 *   blank/empty admin list that would mask an authorization failure.
 * - Last-admin invariant: the disabled-with-tooltip state mirrors the
 *   server-enforced rule. A 409 (CONFLICT_STATE) response from the server is
 *   authoritative and overrides any cached client state.
 */

import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { Pagination } from '../../components/ui/Pagination';
import { Skeleton } from '../../components/ui/Skeleton';
import { EmptyState } from '../../components/ui/EmptyState';
import { ConfirmDialog } from '../../components/ui/ConfirmDialog';
import { useDebounce, useDebouncedQuery } from '../../hooks/useDebounce';
import { listUsers, deprovisionUser } from '../../services/api/endpoints';
import type { UserDTO, UserRole } from '../../services/api/types';

/** Colocated stylesheet, injected once (matching the shared UI atoms' pattern)
 * so the module holds under a strict `style-src` CSP. Only transform/opacity/
 * background/border/color are animated - never layout properties. */
const USERS_PAGE_CSS = `
.up-page{ max-width:1280px; margin:0 auto; padding:var(--space-6,24px); box-sizing:border-box; }
.up-page__header{ margin-bottom:var(--space-4,16px); }
.up-page__title{ margin:0; font-size:var(--text-2xl,24px); font-weight:700; line-height:1.25; color:var(--color-text-primary,#0f172a); }
.up-page__toolbar{ display:flex; flex-wrap:wrap; align-items:flex-end; justify-content:space-between; gap:var(--space-4,16px); margin-bottom:var(--space-4,16px); }
.up-page__search{ display:flex; flex-direction:column; gap:var(--space-1,4px); flex:0 1 auto; }
.up-page__search-label{ font-size:var(--text-sm,14px); font-weight:500; color:var(--color-text-secondary,#475569); }
.up-page__search-input{
  min-height:44px; padding:0 var(--space-3,12px); box-sizing:border-box;
  border:1px solid var(--color-border,#e2e8f0); border-radius:var(--radius-md,8px);
  font-size:var(--text-base,16px); line-height:1.5; color:var(--color-text-primary,#0f172a);
  background-color:var(--color-surface,#ffffff); transition:border-color 150ms ease-out,box-shadow 150ms ease-out;
}
.up-page__search-input:focus-visible{ outline:none; border-color:var(--color-primary,#2563eb); box-shadow:0 0 0 3px rgba(37,99,235,0.2); }
.up-page__table-wrap{ overflow-x:auto; }
.up-page__table{ width:100%; min-width:640px; border-collapse:collapse; background-color:var(--color-surface,#ffffff); }
.up-page__table caption{ text-align:left; font-size:var(--text-sm,14px); color:var(--color-text-secondary,#475569); padding:var(--space-2,8px); }
.up-page__table thead th{
  text-align:left; font-size:var(--text-sm,14px); font-weight:600; color:var(--color-text-secondary,#475569);
  padding:var(--space-3,12px) var(--space-4,16px); border-bottom:1px solid var(--color-border,#e2e8f0);
  background-color:var(--color-surface-muted,#f1f5f9); white-space:nowrap;
}
.up-page__table tbody tr{ border-bottom:1px solid var(--color-border,#e2e8f0); transition:background-color 150ms ease-out; }
.up-page__table tbody tr:hover{ background-color:var(--color-surface-muted,#f1f5f9); }
.up-page__table td{ padding:var(--space-3,12px) var(--space-4,16px); vertical-align:middle; }
.up-page__username{ font-size:var(--text-base,16px); font-weight:500; color:var(--color-text-primary,#0f172a); white-space:nowrap; }
.up-page__last-login{ font-size:var(--text-sm,14px); font-weight:400; color:var(--color-text-muted,#64748b); white-space:nowrap; }
.up-page__badge{
  display:inline-flex; align-items:center; gap:var(--space-1,4px);
  padding:var(--space-1,4px) var(--space-2,8px); border-radius:var(--radius-full,9999px);
  font-size:var(--text-xs,12px); font-weight:600; line-height:1.25; white-space:nowrap;
}
.up-page__badge--admin{ background-color:var(--color-surface-muted,#f1f5f9); color:var(--color-text-primary,#0f172a); border:1px solid var(--color-border,#e2e8f0); }
.up-page__badge--finance_manager{ background-color:#eff6ff; color:#1e40af; }
.up-page__badge--accountant{ background-color:#ecfeff; color:#155e75; }
.up-page__badge--auditor{ background-color:#f5f3ff; color:#5b21b6; }
.up-page__badge--budget_owner{ background-color:#fef3c7; color:#92400e; }
.up-page__badge--view_only{ background-color:#f0fdf4; color:#166534; }
.up-page__badge--employee{ background-color:#f5f5f4; color:#44403c; }
.up-page__active{ display:inline-flex; align-items:center; gap:var(--space-1,4px); font-size:var(--text-sm,14px); font-weight:500; }
.up-page__active--on{ color:var(--color-success,#16a34a); }
.up-page__active--off{ color:var(--color-text-muted,#64748b); }
.up-page__active-icon{ display:inline-flex; width:16px; height:16px; align-items:center; justify-content:center; }
.up-page__actions{ display:flex; gap:var(--space-2,8px); align-items:center; white-space:nowrap; }
.up-page__btn{
  display:inline-flex; align-items:center; justify-content:center; gap:var(--space-2,8px);
  min-height:44px; min-width:44px; padding:var(--space-2,8px) var(--space-3,12px); box-sizing:border-box;
  border:1px solid transparent; border-radius:var(--radius-md,8px);
  font-size:var(--text-sm,14px); font-weight:500; line-height:1.5; cursor:pointer;
  transition:background-color 150ms ease-out,border-color 150ms ease-out,color 150ms ease-out,opacity 150ms ease-out;
}
.up-page__btn--ghost{ background-color:transparent; color:var(--color-primary,#2563eb); }
.up-page__btn--ghost:hover{ background-color:var(--color-surface-muted,#f1f5f9); }
.up-page__btn--danger{ background-color:transparent; color:var(--color-danger,#dc2626); }
.up-page__btn--danger:hover{ background-color:#fef2f2; }
.up-page__btn--primary{ background-color:var(--color-primary,#2563eb); color:var(--color-surface,#ffffff); font-weight:600; }
.up-page__btn--primary:hover{ background-color:#1d4ed8; }
.up-page__btn:focus-visible{ outline:2px solid var(--color-primary,#2563eb); outline-offset:2px; }
.up-page__btn--disabled{ opacity:0.5; cursor:not-allowed; color:var(--color-text-muted,#64748b); }
.up-page__depro-wrap{ position:relative; display:inline-flex; }
.up-page__tooltip{
  display:none; position:absolute; z-index:10; top:calc(100% + 4px); left:0;
  max-width:16rem; padding:var(--space-1,4px) var(--space-2,8px);
  background-color:var(--color-text-primary,#0f172a); color:#ffffff;
  border-radius:var(--radius-sm,4px); box-shadow:var(--shadow-md,0 4px 6px -1px rgba(0,0,0,0.1));
  font-size:var(--text-sm,14px); font-weight:400; line-height:1.5; white-space:normal;
}
.up-page__depro-wrap:hover .up-page__tooltip,
.up-page__depro-wrap:focus-within .up-page__tooltip{ display:block; }
.up-page__skeletons{ display:flex; flex-direction:column; gap:var(--space-2,8px); padding:var(--space-4,16px) 0; }
@media (max-width:767px){
  .up-page__search, .up-page__search-input{ flex:1 1 100%; width:100%; }
  .up-page__toolbar{ align-items:stretch; }
}
@media (prefers-reduced-motion: reduce){
  .up-page__table tbody tr,.up-page__btn,.up-page__search-input{ transition-duration:0.01ms; }
}
`;

if (typeof document !== 'undefined') {
  const USERS_PAGE_STYLE_ID = 'fms-users-page-styles';
  if (!document.getElementById(USERS_PAGE_STYLE_ID)) {
    const styleNode = document.createElement('style');
    styleNode.id = USERS_PAGE_STYLE_ID;
    styleNode.textContent = USERS_PAGE_CSS;
    document.head.appendChild(styleNode);
  }
}

/** Default bounded-collection page size (offset pagination, ≤50 per Miller's law). */
const PAGE_SIZE = 20;

/** Display labels for the known business roles; the fallback uses the raw role
 * string as a text node (safe, indistinguishable from a stale/custom role). */
const ROLE_LABELS: Record<UserRole, string> = {
  ADMIN: 'System administrator',
  FINANCE_MANAGER: 'Finance manager',
  ACCOUNTANT: 'Accountant',
  BUDGET_OWNER: 'Budget owner',
  AUDITOR: 'Auditor',
  VIEW_ONLY: 'View only',
  EMPLOYEE: 'Employee',
};

/** The client error classes live in `client.ts`, which is outside this file's
 * import contract, so the error `code` field is duck-typed. The `code` field
 * is a constant-valued string on every ApiError subclass. */
function hasErrorCode(err: unknown, code: string): boolean {
  return (
    err instanceof Error &&
    typeof (err as { code?: unknown }).code === 'string' &&
    (err as { code: string }).code === code
  );
}

export interface UsersPageProps {
  /** Admin-only route guard resolved by the parent. UX affordance only - the
   * server (gateway RBAC) is the authoritative authorization boundary. */
  canManageUsers: boolean;
  /** Parent navigates to UserFormPage in create mode (empty-state CTA + toolbar). */
  onCreateUser?: () => void;
  /** Parent navigates to UserFormPage in edit mode with the selected user. */
  onEdit?: (user: UserDTO) => void;
  /** Parent-owned toast sink (success/error notifications). */
  onToast?: (input: {
    severity: 'success' | 'error' | 'warning' | 'info';
    title: string;
    body?: string;
  }) => void;
  /** id whose deprovision control must be disabled (server-enforced last-admin
   * invariant mirrored by the parent, which owns the authoritative source). */
  isLastAdminId?: string | null;
  /** Re-fetch trigger for the 403 / retry path. */
  onRefresh?: () => void;
  /** 403 insufficient-permissions fallback action. */
  onBackToDashboard?: () => void;
}

/**
 * Admin user-provisioning list. Fetches its own bounded page via
 * `useDebouncedQuery(listUsers)` (abort-aware), filters by the debounced
 * username locally (listUsers has no search param), and owns the deprovision
 * ConfirmDialog flow.
 */
export function UsersPage({
  canManageUsers,
  onCreateUser,
  onEdit,
  onToast,
  isLastAdminId,
  onRefresh,
  onBackToDashboard,
}: UsersPageProps) {
  const tooltipId = useId();
  const tableWrapRef = useRef<HTMLDivElement>(null);
  const [searchInput, setSearchInput] = useState('');
  const [page, setPage] = useState(1);
  const [deprovisionTarget, setDeprovisionTarget] = useState<UserDTO | null>(null);
  const [deprovisioningId, setDeprovisioningId] = useState<string | null>(null);
  const [deprovisionedIds, setDeprovisionedIds] = useState<ReadonlySet<string>>(new Set());
  const [serverLastAdminId, setServerLastAdminId] = useState<string | null>(null);

  const debouncedTerm = useDebounce(searchInput, 150);
  const lastAdminId = isLastAdminId ?? serverLastAdminId;

  const { data, pending, error } = useDebouncedQuery(
    (signal) => listUsers({ page, pageSize: PAGE_SIZE }, signal),
    [page],
    0,
  );

  // Reset to page 1 whenever the search term settles so a deep-page filter
  // never yields an empty windowed result.
  useEffect(() => {
    setPage(1);
  }, [debouncedTerm]);

  const filteredItems = useMemo(() => {
    const items = data?.items ?? [];
    const term = debouncedTerm.trim().toLowerCase();
    if (!term) return items;
    return items.filter((u) => u.username.toLowerCase().includes(term));
  }, [data, debouncedTerm]);

  const isInitialLoading = data === null && error === null;

  const clearSearch = () => {
    setSearchInput('');
    setPage(1);
  };

  const handleDeprovision = async () => {
    const target = deprovisionTarget;
    if (!target || deprovisioningId !== null) return;
    setDeprovisioningId(target.id);
    try {
      await deprovisionUser(target.id);
      setDeprovisionedIds((prev) => new Set(prev).add(target.id));
      onToast?.({
        severity: 'success',
        title: 'User deprovisioned',
        body: `${target.username} was deprovisioned.`,
      });
    } catch (err) {
      if (hasErrorCode(err, 'CONFLICT_STATE')) {
        // Server-authoritative last-admin invariant; overrides cached client state.
        setServerLastAdminId(target.id);
        onToast?.({
          severity: 'error',
          title: 'Cannot deprovision the last administrator',
          body: `${target.username} must remain an administrator.`,
        });
      } else if (hasErrorCode(err, 'FORBIDDEN')) {
        onToast?.({
          severity: 'error',
          title: 'Could not deprovision user',
          body: 'You do not have permission to perform this action.',
        });
      } else {
        const message =
          err instanceof Error && err.message
            ? err.message
            : 'The user was not deprovisioned. Please retry.';
        onToast?.({
          severity: 'error',
          title: 'Could not deprovision user',
          body: message,
        });
      }
    } finally {
      setDeprovisioningId(null);
      setDeprovisionTarget(null);
    }
  };

  if (!canManageUsers) {
    return (
      <main className="up-page">
        <header className="up-page__header">
          <h1 className="up-page__title">Users</h1>
        </header>
        <EmptyState
          tone="info"
          title="Insufficient permissions"
          description="You don't have access to manage users. Contact a system administrator if you believe this is a mistake."
          actionLabel={onBackToDashboard ? 'Back to dashboard' : undefined}
          onAction={onBackToDashboard}
          role="alert"
        />
      </main>
    );
  }

  const hasSearchTerm = debouncedTerm.trim() !== '';
  const listFailed = error !== null;

  return (
    <main className="up-page">
      <header className="up-page__header">
        <h1 className="up-page__title">Users</h1>
      </header>

      <section className="up-page__toolbar" aria-label="User management controls">
        <div className="up-page__search">
          <label className="up-page__search-label" htmlFor="up-search">
            Search by username
          </label>
          <input
            id="up-search"
            className="up-page__search-input"
            type="search"
            value={searchInput}
            onChange={(e) => setSearchInput(e.target.value)}
            placeholder="Search by username"
            autoComplete="off"
          />
        </div>
        {onCreateUser ? (
          <button type="button" className="up-page__btn up-page__btn--primary" onClick={onCreateUser}>
            New user
          </button>
        ) : null}
      </section>

      {isInitialLoading ? (
        <div className="up-page__skeletons" aria-hidden="true">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} variant="table-row" announceOnLoad={false} />
          ))}
        </div>
      ) : listFailed ? (
        <EmptyState
          tone="info"
          title="Failed to load users"
          description="We couldn't load the user list. Check your connection and try again."
          actionLabel={onRefresh ? 'Retry' : undefined}
          onAction={onRefresh}
          role="alert"
        />
      ) : hasSearchTerm && filteredItems.length === 0 ? (
        <EmptyState
          tone="neutral"
          title="No users match your search"
          description="No users found."
          actionLabel="Clear search"
          onAction={clearSearch}
        />
      ) : (data?.total ?? 0) === 0 ? (
        <EmptyState
          tone="neutral"
          title="No users yet"
          description="You haven't created any users. Create your first user to get started."
          actionLabel={onCreateUser ? 'Create your first user' : undefined}
          onAction={onCreateUser}
        />
      ) : (
        <>
          <div className="up-page__table-wrap" ref={tableWrapRef} tabIndex={-1}>
            <table className="up-page__table">
              <caption>Registered users</caption>
              <thead>
                <tr>
                  <th scope="col">Username</th>
                  <th scope="col">Role</th>
                  <th scope="col">Status</th>
                  <th scope="col">Last login</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {filteredItems.map((user) => {
                  const isLastAdmin = lastAdminId !== null && user.id === lastAdminId;
                  const active = user.active && !deprovisionedIds.has(user.id);
                  const label = ROLE_LABELS[user.role] ?? user.role;
                  return (
                    <tr key={user.id}>
                      <td className="up-page__username">{user.username}</td>
                      <td>
                        <span className={`up-page__badge up-page__badge--${user.role.toLowerCase()}`}>
                          {label}
                        </span>
                      </td>
                      <td>
                        {active ? (
                          <span className="up-page__active up-page__active--on">
                            <span className="up-page__active-icon" aria-hidden="true">
                              {'\u2713'}
                            </span>
                            Active
                          </span>
                        ) : (
                          <span className="up-page__active up-page__active--off">
                            <span className="up-page__active-icon" aria-hidden="true">
                              {'\u2715'}
                            </span>
                            Deprovisioned
                          </span>
                        )}
                      </td>
                      <td className="up-page__last-login">
                        {user.lastLoginAt ? formatLastLogin(user.lastLoginAt) : 'Never signed in'}
                      </td>
                      <td>
                        <div className="up-page__actions">
                          {onEdit ? (
                            <button
                              type="button"
                              className="up-page__btn up-page__btn--ghost"
                              onClick={() => onEdit(user)}
                            >
                              Edit
                            </button>
                          ) : null}
                          {isLastAdmin ? (
                            <span
                              className="up-page__depro-wrap"
                              tabIndex={0}
                              aria-describedby={tooltipId}
                              title="Cannot deprovision the last administrator"
                            >
                              <button
                                type="button"
                                className="up-page__btn up-page__btn--danger up-page__btn--disabled"
                                tabIndex={-1}
                                aria-disabled="true"
                                onClick={() => undefined}
                              >
                                Deprovision
                              </button>
                              <span className="up-page__tooltip" id={tooltipId} role="tooltip">
                                Cannot deprovision the last administrator
                              </span>
                            </span>
                          ) : (
                            <button
                              type="button"
                              className="up-page__btn up-page__btn--danger"
                              onClick={() => setDeprovisionTarget(user)}
                            >
                              Deprovision
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>

          <Pagination
            offset={{ page: data?.page ?? 1, total: data?.total ?? 0, pageSize: data?.page_size ?? PAGE_SIZE }}
            onChange={({ page: nextPage }) => {
              if (typeof nextPage === 'number') setPage(nextPage);
            }}
            loading={pending}
            focusTargetRef={tableWrapRef}
          />
        </>
      )}

      <ConfirmDialog
        open={deprovisionTarget !== null}
        title="Deprovision user"
        consequence="User loses access immediately; their data is retained per retention policy."
        confirmLabel="Deprovision"
        cancelLabel="Cancel"
        danger
        loading={deprovisioningId !== null}
        onConfirm={handleDeprovision}
        onCancel={() => {
          if (deprovisioningId === null) setDeprovisionTarget(null);
        }}
      />
    </main>
  );
}

/** Formats an ISO 8601 UTC timestamp as `YYYY-MM-DD HH:MM:SS` for the "last
 * login" column. Rendered as a text node (`lastLoginAt` is untrusted input). */
function formatLastLogin(value: string): string {
  const datePart = value.slice(0, 10);
  const timeMatch = /[T ](\d{2}:\d{2}:\d{2})/.exec(value);
  const timePart = timeMatch ? timeMatch[1] : '';
  return timePart ? `${datePart} ${timePart}` : datePart;
}

export default UsersPage;