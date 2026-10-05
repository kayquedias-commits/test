import React, { useCallback, useEffect, useMemo, useState, type ReactElement } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { EmptyState } from '../../components/ui/EmptyState';
import { Pagination } from '../../components/ui/Pagination';
import { Skeleton } from '../../components/ui/Skeleton';
import { DeleteAccountDialog } from './DeleteAccountDialog';
import { usePermission } from '../../hooks/usePermission';
import { listAccounts, reconcileAccount } from '../../services/api/endpoints';
import type { AccountDTO, AccountType, Money } from '../../services/api/types';
import { formatMinorUnits } from '../../utils/currency';
import { useNotifications } from '../../app/stores/notifications';

/** Server-side offset page size for the bounded account collection. */
const PAGE_SIZE = 100;
/** Local debounce window for the name search input (spec §8.4 requires >= 150ms). */
const SEARCH_DEBOUNCE_MS = 150;
/** Number of skeleton rows rendered while the account list loads. */
const SKELETON_ROWS = 5;

/**
 * Closed set of account types used to derive badge labels. The badge label is
 * always drawn from this map, never from the raw API `type`, so an unknown or
 * unexpected value falls back to the static "Other" label instead of being
 * echoed into markup.
 */
const ACCOUNT_TYPE_META: ReadonlyArray<{ value: AccountType; label: string; className: string }> = [
  { value: 'checking', label: 'Checking', className: 'ap-type--checking' },
  { value: 'savings', label: 'Savings', className: 'ap-type--savings' },
  { value: 'credit', label: 'Credit', className: 'ap-type--credit' },
  { value: 'cash', label: 'Cash', className: 'ap-type--cash' },
  { value: 'other', label: 'Other', className: 'ap-type--other' },
];

const ACCOUNT_TYPE_MAP: Record<AccountType, { label: string; className: string }> = {
  checking: ACCOUNT_TYPE_META[0],
  savings: ACCOUNT_TYPE_META[1],
  credit: ACCOUNT_TYPE_META[2],
  cash: ACCOUNT_TYPE_META[3],
  other: ACCOUNT_TYPE_META[4],
};

/**
 * Generate a unique idempotency key for the reconcile POST so that retries of
 * the same mutation are deduplicated server-side.
 */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `ap-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

/** Format a minor-unit money value for display, falling back to the raw amount. */
function formatKpiValue(money: Money): string {
  try {
    return formatMinorUnits(money.amount, money.currency);
  } catch {
    return money.amount.toString();
  }
}

export interface AccountsPageProps {
  /** Navigate to the create-account form. Absent -> the header CTA is hidden. */
  onCreateAccount?: () => void;
  /** Navigate to the edit-account form for a specific account. Absent -> Edit is omitted. */
  onEditAccount?: (account: AccountDTO) => void;
  /** Stable tenant/user scope appended to query keys to prevent cross-session cache leakage. */
  scopeKey?: string;
}

const STYLE_ID = 'fms-accounts-page-styles';

function ensureStyles(): void {
  if (typeof document === 'undefined' || document.getElementById(STYLE_ID)) return;
  const style = document.createElement('style');
  style.id = STYLE_ID;
  style.textContent = FMS_ACCOUNTS_PAGE_CSS;
  document.head.appendChild(style);
}

export function AccountsPage({
  onCreateAccount,
  onEditAccount,
  scopeKey,
}: AccountsPageProps): ReactElement {
  ensureStyles();

  const queryClient = useQueryClient();
  const { can } = usePermission();
  const canRead = can('account:read');
  const canWrite = can('account:write');

  const [page, setPage] = useState(1);
  const [searchInput, setSearchInput] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const [reconcilingId, setReconcilingId] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<AccountDTO | null>(null);

  useEffect(() => {
    const t = setTimeout(() => setDebouncedSearch(searchInput.trim().toLowerCase()), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [searchInput]);

  // A search change re-baselines the collection to the first page.
  useEffect(() => {
    setPage(1);
  }, [debouncedSearch]);

  const invalidateAccounts = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: scopeKey ? ['accounts', scopeKey] : ['accounts'] });
  }, [queryClient, scopeKey]);

  const queryKey = useMemo(() => (scopeKey ? ['accounts', scopeKey, page] : ['accounts', page]), [scopeKey, page]);

  const { data, isPending, isFetching, isError, refetch } = useQuery({
    queryKey,
    queryFn: ({ signal }) => listAccounts({ page, pageSize: PAGE_SIZE }, signal),
    refetchOnWindowFocus: false,
  });

  const reconcile = useMutation({
    mutationFn: (id: string) => reconcileAccount(id, { idempotencyKey: newIdempotencyKey() }),
    onSuccess: () => {
      invalidateAccounts();
      useNotifications
        .getState()
        .push({ severity: 'success', title: 'Reconcile complete', body: 'Account balance verified.' });
    },
    onError: (err: unknown) => {
      const name = (err as { name?: string } | null)?.name ?? '';
      const body =
        name === 'ConflictError'
          ? 'This account was modified by another user. Refresh and try again.'
          : name === 'ForbiddenError'
            ? "You don't have permission to reconcile accounts."
            : 'Reconciliation failed. Re-check the account balance and try again.';
      useNotifications.getState().push({ severity: 'error', title: 'Reconcile failed', body });
    },
  });

  const handleReconcile = useCallback(
    (id: string) => {
      setReconcilingId(id);
      reconcile.mutate(id, { onSettled: () => setReconcilingId(null) });
    },
    [reconcile],
  );

  const handleSearchChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setSearchInput(e.target.value);
  }, []);

  const filtered = useMemo(() => {
    if (!data) return [];
    const items = data.items ?? [];
    if (!debouncedSearch) return items;
    return items.filter((a) => a.name.toLowerCase().includes(debouncedSearch));
  }, [data, debouncedSearch]);

  const total = data?.total ?? 0;
  const hasQueryError = isError && data === undefined;

  if (!canRead) {
    return (
      <main className="ap-page">
        <section className="ap-state" role="alert">
          <h2 className="ap-state-title">Insufficient permissions</h2>
          <p className="ap-state-body">You don&apos;t have access to accounts.</p>
        </section>
      </main>
    );
  }

  return (
    <main className="ap-page">
      <div className="ap-header">
        <h1 className="ap-title">Accounts</h1>
        {canWrite && onCreateAccount && (
          <button type="button" className="ap-btn fms-btn--primary" onClick={onCreateAccount}>
            New Account
          </button>
        )}
      </div>

      <div className="ap-toolbar">
        <label className="ap-search" htmlFor="ap-search">
          <span className="ap-sr">Search accounts</span>
          <input
            id="ap-search"
            type="search"
            autoComplete="off"
            placeholder="Search by name"
            value={searchInput}
            onChange={handleSearchChange}
          />
        </label>
      </div>

      {hasQueryError && (
        <section className="ap-state" role="alert" aria-live="assertive">
          <h2 className="ap-state-title">Couldn&apos;t load your accounts</h2>
          <p className="ap-state-body">
            There was a problem fetching your account list. Please try again.
          </p>
          <button type="button" className="ap-btn fms-btn--secondary" onClick={() => refetch()}>
            Try again
          </button>
        </section>
      )}

      {!hasQueryError && isPending && (
        <>
          <p className="ap-sr" role="status">
            Loading accounts
          </p>
          <div className="ap-card">
            <div className="ap-table-wrap">
              <table className="ap-table" aria-label="Financial accounts">
                <caption className="ap-sr">Financial accounts</caption>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Type</th>
                    <th scope="col" className="ap-num">
                      Opening Balance
                    </th>
                    <th scope="col" className="ap-num">
                      Current Balance
                    </th>
                    {canWrite && (
                      <th scope="col" className="ap-actions">
                        Actions
                      </th>
                    )}
                  </tr>
                </thead>
                <tbody>
                  {Array.from({ length: SKELETON_ROWS }).map((_, i) => (
                    <tr key={i}>
                      <td className="ap-skeleton-cell ap-col-name">
                        <Skeleton variant="text" lines={1} announceOnLoad={false} ariaLabel="" />
                      </td>
                      <td className="ap-skeleton-cell ap-col-type">
                        <Skeleton variant="text" lines={1} announceOnLoad={false} ariaLabel="" />
                      </td>
                      <td className="ap-skeleton-cell ap-num ap-col-open">
                        <Skeleton variant="text" lines={1} announceOnLoad={false} ariaLabel="" />
                      </td>
                      <td className="ap-skeleton-cell ap-num ap-col-current">
                        <Skeleton variant="text" lines={1} announceOnLoad={false} ariaLabel="" />
                      </td>
                      {canWrite && (
                        <td className="ap-skeleton-cell ap-actions">
                          <Skeleton variant="text" lines={1} announceOnLoad={false} ariaLabel="" />
                        </td>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}

      {!hasQueryError && !isPending && total === 0 && !debouncedSearch && (
        <div className="ap-card">
          {canWrite && onCreateAccount ? (
            <EmptyState
              title="Create your first account"
              description="Add a bank account, credit card, or cash account to start tracking your balance."
              actionLabel="Create account"
              onAction={onCreateAccount}
              tone="info"
            />
          ) : (
            <EmptyState title="No accounts yet" description="Accounts will appear here." tone="neutral" />
          )}
        </div>
      )}

      {!hasQueryError && !isPending && total > 0 && filtered.length === 0 && (
        <div className="ap-card">
          <EmptyState
            title="No accounts match your search"
            description="Try a different account name."
            tone="neutral"
          />
        </div>
      )}

      {!hasQueryError && !isPending && filtered.length > 0 && (
        <div className="ap-card">
          <div className="ap-table-wrap">
            <table className="ap-table">
              <caption className="ap-sr">Financial accounts</caption>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Type</th>
                  <th scope="col" className="ap-num">
                    Opening Balance
                  </th>
                  <th scope="col" className="ap-num">
                    Current Balance
                  </th>
                  {canWrite && (
                    <th scope="col" className="ap-actions">
                      Actions
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {filtered.map((account) => {
                  const typeMeta = ACCOUNT_TYPE_MAP[account.type] ?? { label: 'Other', className: 'ap-type--other' };
                  const isReconciling = reconcilingId === account.id;
                  return (
                    <tr key={account.id}>
                      <td className="ap-col-name">
                        <span className="ap-name">{account.name}</span>
                      </td>
                      <td>
                        <span className={`ap-type ${typeMeta.className}`}>{typeMeta.label}</span>
                      </td>
                      <td className="ap-num ap-col-open">{formatKpiValue(account.openingBalance)}</td>
                      <td className="ap-num ap-col-current">{formatKpiValue(account.currentBalance)}</td>
                      {canWrite && (
                        <td className="ap-actions">
                          {onEditAccount && (
                            <button
                              type="button"
                              className="ap-btn fms-btn--ghost"
                              aria-label={`Edit ${account.name}`}
                              onClick={() => onEditAccount(account)}
                            >
                              Edit
                            </button>
                          )}
                          <button
                            type="button"
                            className="ap-btn fms-btn--secondary"
                            aria-label={`Reconcile ${account.name}`}
                            disabled={isReconciling}
                            onClick={() => handleReconcile(account.id)}
                          >
                            {isReconciling ? 'Reconciling…' : 'Reconcile'}
                          </button>
                          <button
                            type="button"
                            className="ap-btn fms-btn--danger"
                            aria-label={`Delete ${account.name}`}
                            onClick={() => setDeleteTarget(account)}
                          >
                            Delete
                          </button>
                        </td>
                      )}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <div className="ap-pagination">
            <Pagination
              offset={{ page, total, pageSize: PAGE_SIZE }}
              onChange={(p) => p.page && setPage(p.page)}
              loading={isFetching}
              labels={{ range: 'Showing {start}–{end} of {total} accounts', pageStatus: 'Page {page} of {total}' }}
            />
          </div>
        </div>
      )}

      {deleteTarget && (
        <DeleteAccountDialog
          account={deleteTarget}
          open
          onCancel={() => setDeleteTarget(null)}
          onDeleted={() => {
            setDeleteTarget(null);
            invalidateAccounts();
            useNotifications
              .getState()
              .push({ severity: 'success', title: 'Account deleted', body: 'The account and its associated data have been removed.' });
          }}
        />
      )}
    </main>
  );
}

export default AccountsPage;

const FMS_ACCOUNTS_PAGE_CSS = `
.ap-page {
  max-width: 1280px;
  margin: 0 auto;
  padding: 16px;
}
@media (min-width: 768px) {
  .ap-page { padding: 24px; }
}

.ap-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 24px;
  margin-bottom: 24px;
}
.ap-title {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  line-height: 1.25;
  color: #111827;
}
@media (min-width: 768px) {
  .ap-title { font-size: 24px; }
}

.ap-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 16px;
  margin-bottom: 16px;
}
.ap-search {
  flex: 1 1 260px;
  min-width: 220px;
  max-width: 380px;
}
.ap-search input {
  width: 100%;
  min-height: 44px;
  padding: 10px 12px;
  font-size: 14px;
  color: #111827;
  background: #FFFFFF;
  border: 1px solid #D1D5DB;
  border-radius: 8px;
}
.ap-search input::placeholder { color: #6B7280; }
.ap-search input:focus-visible {
  outline: 2px solid #4f46e5;
  outline-offset: 2px;
}

.ap-card {
  background: #FFFFFF;
  border: 1px solid #E5E7EB;
  border-radius: 12px;
  overflow: hidden;
}
.ap-table-wrap { overflow-x: auto; }

.ap-table {
  width: 100%;
  border-collapse: collapse;
  min-width: 720px;
}
.ap-table th {
  font-size: 14px;
  font-weight: 600;
  color: #6B7280;
  text-align: left;
  border-bottom: 2px solid #D1D5DB;
  padding: 12px 16px;
}
.ap-table td {
  font-size: 16px;
  color: #111827;
  border-bottom: 1px solid #E5E7EB;
  padding: 12px 16px;
  vertical-align: middle;
}
.ap-table tbody tr:last-child td { border-bottom: none; }
.ap-table tbody tr { transition: background-color 150ms ease-out; }
.ap-table tbody tr:hover { background: #F3F4F6; }
.ap-table th:first-child,
.ap-table td:first-child {
  position: sticky;
  left: 0;
  background: #FFFFFF;
}
.ap-table tbody tr:hover td:first-child { background: #F3F4F6; }

.ap-num { text-align: right; font-variant-numeric: tabular-nums; }
.ap-col-current { font-weight: 600; }
.ap-col-name .ap-name { font-weight: 500; }

.ap-type {
  display: inline-flex;
  align-items: center;
  border-radius: 9999px;
  padding: 2px 8px;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}
.ap-type--checking { background: #DBEAFE; color: #1E40AF; }
.ap-type--savings { background: #DCFCE7; color: #166534; }
.ap-type--credit { background: #FEF3C7; color: #92400E; }
.ap-type--cash { background: #F3F4F6; color: #374151; }
.ap-type--other { background: #EDE9FE; color: #5B21B6; }

.ap-actions {
  text-align: right;
  white-space: nowrap;
}
.ap-actions .ap-btn { margin-left: 8px; }

.ap-btn {
  min-height: 44px;
  min-width: 44px;
  padding: 10px 14px;
  font-size: 14px;
  font-weight: 500;
  line-height: 1.2;
  border-radius: 8px;
  border: 1px solid transparent;
  cursor: pointer;
  transition: background-color 150ms ease-out, border-color 150ms ease-out, color 150ms ease-out, opacity 150ms ease-out;
}
.ap-btn:focus-visible {
  outline: 2px solid #4f46e5;
  outline-offset: 2px;
}
.ap-btn:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}
.fms-btn--primary {
  background: #2563EB;
  color: #FFFFFF;
}
.fms-btn--primary:hover { background: #1D4ED8; }
.fms-btn--secondary {
  background: #FFFFFF;
  color: #374151;
  border-color: #D1D5DB;
}
.fms-btn--secondary:hover { border-color: #2563EB; color: #2563EB; }
.fms-btn--ghost {
  background: transparent;
  color: #374151;
}
.fms-btn--ghost:hover { color: #2563EB; }
.fms-btn--danger {
  background: #FFFFFF;
  color: #DC2626;
  border-color: #FCA5A5;
}
.fms-btn--danger:hover { background: #FEE2E2; }

.ap-skeleton-cell .fms-skeleton { width: 100%; }
.ap-col-name .fms-skeleton { max-width: 220px; }
.ap-col-type .fms-skeleton { max-width: 80px; }
.ap-col-open .fms-skeleton { max-width: 120px; }
.ap-col-current .fms-skeleton { max-width: 120px; }

.ap-pagination {
  border-top: 1px solid #E5E7EB;
  padding-top: 16px;
  margin-top: 16px;
}

.ap-state {
  text-align: center;
  padding: 64px 24px;
  max-width: 42ch;
  margin: 0 auto;
}
.ap-state-title {
  margin: 0 0 8px;
  font-size: 20px;
  font-weight: 600;
  line-height: 1.25;
  color: #111827;
}
.ap-state-body {
  margin: 0 0 16px;
  font-size: 16px;
  line-height: 1.5;
  color: #6B7280;
}

.ap-sr {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

@media (prefers-reduced-motion: reduce) {
  .ap-table tbody tr,
  .ap-btn {
    transition: none;
    animation: none;
  }
}
`;