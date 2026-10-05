import { useEffect, useRef, useState } from 'react';

import { useTransactionsStore, type TransactionFilters } from '../../app/stores/transactions';
import { useNotifications } from '../../app/stores/notifications';
import { EmptyState } from '../../components/ui/EmptyState';
import { Pagination } from '../../components/ui/Pagination';
import { Skeleton } from '../../components/ui/Skeleton';
import { TransactionFilters, type TransactionFiltersValue } from './TransactionFilters';
import { TransactionTable, type TransactionRow, type SortKey } from './TransactionTable';
import { DeleteTransactionDialog } from './DeleteTransactionDialog';
import { ConflictRecoveryBanner } from './ConflictRecoveryBanner';
import { usePermission } from '../../hooks/usePermission';
import { listAccounts, listCategories } from '../../services/api/endpoints';

/**
 * Transaction list container (route `/transactions`). Owns the ledger-list
 * screen: composes filters, windowed table, cursor pagination, the 409
 * conflict banner and the delete confirmation dialog, and drives the
 * transactions zustand store. Children stay presentational; the container
 * wires state and mutations. Permission gating (hide, never disable) mirrors
 * the UI-only RBAC affordance — the backend enforces the real authorization.
 */
export function TransactionsPage({
  onCreateTransaction,
  onEdit,
}: TransactionsPageProps) {
  const { role, can } = usePermission();
  const canRead = can('transaction:read');
  const canWrite = can('transaction:write');

  return (
    <main className="tpp-page">
      <header className="tpp-header">
        <h1 className="tpp-title">Transactions</h1>
        {canWrite && onCreateTransaction && (
          <button type="button" className="tpp-button" onClick={onCreateTransaction}>
            New transaction
          </button>
        )}
      </header>

      {role === null ? (
        // Session hydrating/unauthenticated: fail closed with a skeleton that
        // holds layout so no read grant flashes before the role resolves.
        <Skeleton variant="table-row" announceOnLoad ariaLabel="Loading transactions" />
      ) : canRead ? (
        <LedgerView
          onCreateTransaction={onCreateTransaction}
          onEdit={onEdit}
          canWrite={canWrite}
        />
      ) : (
        <EmptyState
          tone="info"
          role="alert"
          title="Insufficient permissions"
          description="You need read access to view transactions."
        />
      )}
    </main>
  );
}

function LedgerView({
  onCreateTransaction,
  onEdit,
  canWrite,
}: {
  onCreateTransaction?: () => void;
  onEdit?: (id: string) => void;
  canWrite: boolean;
}) {
  const store = useTransactionsStore();
  const notify = useNotifications();

  const [currentCursor, setCurrentCursor] = useState<string | null>(null);
  const [accounts, setAccounts] = useState<Record<string, string>>({});
  const [categoriesList, setCategoriesList] = useState<
    Awaited<ReturnType<typeof listCategories>>['items']
  >([]);
  const [deleteTarget, setDeleteTarget] = useState<TxDTO | null>(null);
  const tableWrapRef = useRef<HTMLDivElement | null>(null);

  // Prime the ledger on first mount when nothing is cached yet.
  useEffect(() => {
    if (useTransactionsStore.getState().pages.size === 0) {
      useTransactionsStore.getState().loadPage(null);
    }
  }, []);

  // Eager-load account/category labels once for direct rendering of row names.
  useEffect(() => {
    const ctrl = new AbortController();
    (async () => {
      try {
        const [acc, cats] = await Promise.all([
          listAccounts({ pageSize: PAGE_LIMIT }, ctrl.signal),
          listCategories({ pageSize: PAGE_LIMIT }, ctrl.signal),
        ]);
        setAccounts(Object.fromEntries(acc.items.map((a) => [a.id, a.name])));
        setCategoriesList(cats.items);
      } catch (err) {
        // Label maps are a rendering convenience; an abort on unmount or a
        // transient failure must never break the list itself.
        if ((err as Error).name === 'AbortError') return;
        setAccounts({});
        setCategoriesList([]);
      }
    })();
    return () => ctrl.abort();
  }, []);

  const pageKey = currentCursor ?? FIRST_KEY;
  const page = store.pages.get(pageKey);
  const rows = page?.items ?? [];
  const status = page?.status ?? 'idle';
  const pageError = page?.error ?? null;
  const nextCursor = page?.nextCursor ?? null;
  const prevCursor = prevCursorFor(useTransactionsStore.getState(), currentCursor);
  const total = rows.length;

  const catMap: Record<string, string> = Object.fromEntries(
    categoriesList.map((c) => [c.id, c.name]),
  );
  const tableItems: TransactionRow[] = rows.map((dto) =>
    rowFrom(dto, accounts, catMap),
  );
  const isFiltered = valueIsFiltered(store.filters);

  const handleFiltersChange = (v: TransactionFiltersValue) => {
    const patch: Partial<TransactionFilters> = {
      description: v.search || undefined,
      dateFrom: v.startDate || undefined,
      dateTo: v.endDate || undefined,
      categoryId: v.categoryIds.length ? v.categoryIds[0] : undefined,
    };
    setCurrentCursor(null);

    if (!valueIsFiltered(patch as TransactionFilters) && v.sortField === 'date') {
      store.clearFilters();
      if (store.sort !== 'date') store.setSort('date');
    } else {
      store.setFilters(patch);
      if (v.sortField !== store.sort) store.setSort(v.sortField);
    }
  };

  const handlePagination = (payload: {
    cursor?: { prev?: string | null; next?: string | null };
    page?: number;
  }) => {
    if (payload.cursor?.next && nextCursor) {
      setCurrentCursor(nextCursor);
      store.loadPage(nextCursor);
    } else if (payload.cursor?.prev && prevCursor) {
      setCurrentCursor(prevCursor);
      store.loadPage(prevCursor);
    }
  };

  const handleClearFromEmptyState = () => {
    setCurrentCursor(null);
    store.clearFilters();
    if (store.sort !== 'date') store.setSort('date');
  };

  const handleRetry = () => {
    store.loadPage(currentCursor);
  };

  const handleSort = (key: SortKey) => {
    // Only the store/API ordering fields are server-sortable; client-only
    // sort columns (category/account) are no-ops here.
    if (key === 'date' || key === 'amount' || key === 'description') {
      if (key !== store.sort) store.setSort(key);
    }
  };

  const handleDelete = (id: string) => {
    setDeleteTarget(selectTransactionById(useTransactionsStore.getState(), id));
  };

  const handleDialogDeleted = () => {
    notify.push({
      severity: 'success',
      title: 'Transaction deleted',
      body: 'The transaction was permanently removed.',
    });
    setDeleteTarget(null);
  };

  const handleDialogClose = () => {
    if (!store.isMutating) setDeleteTarget(null);
  };

  const renderEmptySlot = () => {
    if (isFiltered) {
      return (
        <EmptyState
          tone="info"
          role="status"
          title="No transactions match your filters"
          description="Adjust your filters to see more entries."
          actionLabel="Clear filters"
          onAction={handleClearFromEmptyState}
        />
      );
    }
    return (
      <EmptyState
        tone="info"
        role="status"
        title="Add your first transaction"
        description="Record your first income or expense to start building your ledger."
        actionLabel={canWrite ? 'Add your first transaction' : undefined}
        onAction={canWrite ? onCreateTransaction : undefined}
      />
    );
  };

  return (
    <>
      <section className="tpp-filters-card" aria-label="Transaction filters">
        <TransactionFilters
          value={{
            search: store.filters.description ?? '',
            startDate: store.filters.dateFrom,
            endDate: store.filters.dateTo,
            categoryIds: store.filters.categoryId ? [store.filters.categoryId] : [],
            sortField: store.sort,
            sortDirection: 'desc',
          }}
          categories={categoriesList}
          onChange={handleFiltersChange}
        />
      </section>

      {store.lastConflict && (
        <ConflictRecoveryBanner
          disableDismiss
          onRefresh={() => useTransactionsStore.getState().resolveConflict('refresh')}
        />
      )}

      <div className="tpp-table-region" ref={tableWrapRef} tabIndex={-1}>
        <TransactionTable
          items={tableItems}
          loading={status === 'loading'}
          error={
            status === 'error'
              ? (pageError?.message ?? 'Could not load transactions. Try again.')
              : null
          }
          onRetry={handleRetry}
          onSort={handleSort}
          onEdit={onEdit}
          onDelete={canWrite ? handleDelete : undefined}
          sortKey={store.sort}
          sortDirection="none"
          emptySlot={
            total === 0 && status !== 'loading' && status !== 'error'
              ? renderEmptySlot()
              : undefined
          }
        />
      </div>

      <Pagination
        cursor={{ next: nextCursor, prev: prevCursor }}
        cursorTotal={total}
        cursorStart={1}
        cursorPageSize={PAGE_LIMIT}
        loading={status === 'loading'}
        onChange={handlePagination}
        focusTargetRef={tableWrapRef}
      />

      {deleteTarget && (
        <DeleteTransactionDialog
          transaction={deleteTarget}
          open={deleteTarget !== null}
          onClose={handleDialogClose}
          onDeleted={handleDialogDeleted}
        />
      )}

      <style>{TppStyle}</style>
    </>
  );
}

export interface TransactionsPageProps {
  /** Wired by the parent route to open the New Transaction form. When absent
   *  the CTA is hidden entirely (least-privilege, never disabled). */
  onCreateTransaction?: () => void;
  /** Wired by the parent route to open the Edit Transaction form for an id. */
  onEdit?: (transactionId: string) => void;
}

/** Transaction row DTO as carried by the transactions store; resolves without
 * reaching into services/api/types (outside this module's contract). */
export type TxDTO = NonNullable<ReturnType<typeof selectTransactionById>>;

const FIRST_KEY = '__first__';
const PAGE_LIMIT = 100;

function prevCursorFor(
  state: ReturnType<typeof useTransactionsStore.getState>,
  key: string | null,
): string | null {
  return state.pages.get(key ?? FIRST_KEY)?.prevCursor ?? null;
}

function valueIsFiltered(filters: TransactionFilters): boolean {
  return Boolean(
    (filters.description ?? '') !== '' ||
      filters.dateFrom ||
      filters.dateTo ||
      filters.categoryId,
  );
}

function rowFrom(
  dto: TxDTO,
  accMap: Record<string, string>,
  catMap: Record<string, string>,
): TransactionRow {
  return {
    id: dto.id,
    date: dto.date,
    description: dto.description,
    scheduled: dto.scheduled,
    category: catMap[dto.categoryIds[0]] ?? 'Uncategorized',
    account: accMap[dto.accountId] ?? '',
    amount: dto.amount.amount,
    currency: dto.amount.currency,
    type: dto.type,
  };
}

const TppStyle = `
  .tpp-page {
    max-width: 1280px;
    margin: 0 auto;
    padding: var(--space-6);
    display: flex;
    flex-direction: column;
  }
  .tpp-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    margin-bottom: var(--space-6);
  }
  .tpp-title {
    font-size: 24px;
    font-weight: 600;
    line-height: 1.25;
    margin: 0;
  }
  .tpp-button {
    min-height: 44px;
    padding: 12px 20px;
    border: none;
    border-radius: 8px;
    background: #2563eb;
    color: #ffffff;
    font-size: 16px;
    font-weight: 500;
    cursor: pointer;
    box-shadow: 0 1px 2px rgba(0,0,0,0.05);
  }
  .tpp-button:hover { background: #1d4ed8; }
  .tpp-button:active { transform: translateY(1px); box-shadow: none; }
  .tpp-button:focus-visible { outline: 2px solid #2563eb; outline-offset: 2px; }
  .tpp-filters-card {
    background: #ffffff;
    border: 1px solid rgba(0,0,0,0.08);
    border-radius: 12px;
    box-shadow: 0 1px 2px rgba(0,0,0,0.05);
    padding: var(--space-4);
    margin-bottom: var(--space-6);
  }
  .tpp-table-region { position: relative; outline: none; }
  .tpp-table-region:focus-visible { outline: 2px solid #2563eb; outline-offset: 2px; }
  @media (max-width: 767px) {
    .tpp-header { flex-direction: column; align-items: stretch; }
    .tpp-button { width: 100%; }
  }
`;