import { useEffect, useState, type ReactNode } from 'react';

import { ConfirmDialog } from '../../components/ui/ConfirmDialog';
import { EmptyState } from '../../components/ui/EmptyState';
import { Skeleton } from '../../components/ui/Skeleton';
import { MergeCategoryDialog } from './MergeCategoryDialog';
import { usePermission } from '../../hooks/usePermission';
import { deleteCategory, listCategories } from '../../services/api/endpoints';
import type { CategoryDTO, CategoryType } from '../../services/api/types';

/**
 * CategoriesPage — Module C taxonomy tree (income / expense, one level deep).
 *
 * Groups categories into Income and Expense common-region cards. Each row shows
 * name, a type badge, an in-use badge, and up to three actions per the Module C
 * boundary: Edit always (when permitted), Merge ONLY for in-use categories, and
 * Delete ONLY for free categories (wrapped in a ConfirmDialog stating the
 * consequence). In-use deletion is intentionally absent from the DOM — merge is
 * the supported path (error prevention at the interaction level).
 *
 * Permission is a presentation-only, least-privilege gate: view-only roles
 * render no Create/Edit/Merge/Delete affordances (hidden, never disabled). The
 * backend enforces the same matrix at the resource boundary.
 *
 * Mutations are confirmed by the server before the list is refetched — the
 * client never locally mutates state. Cache invalidation and success/error
 * toasts are delegated to the parent via props (this page does not import the
 * budgets/transactions stores, keeping within its dependency contract).
 */

export interface CategoriesPageToast {
  severity: 'info' | 'success' | 'warning' | 'error';
  title: string;
  body?: string;
}

export interface CategoriesPageProps {
  /** Parent navigates to the category creation form (hidden when absent). */
  onCreateCategory?: () => void;
  /** Parent navigates to the edit form for a category/subcategory. */
  onEdit?: (category: CategoryDTO) => void;
  /** Parent navigates back to the dashboard (used for the read-denied state). */
  onBackToDashboard?: () => void;
  /** Parent-owned toast surface; message body is rendered as a text node. */
  onToast?: (toast: CategoriesPageToast) => void;
  /** Parent invalidates budgets/transactions caches after delete/merge. */
  onInvalidateCaches?: () => void;
}

const PAGE_SIZE = 100;

/** Split a type's categories into top-level, per-parent subcategories, orphans. */
interface CategoryGroup {
  top: CategoryDTO[];
  byParent: Map<string, CategoryDTO[]>;
  orphans: CategoryDTO[];
}

// ---------------------------------------------------------------- Styles

const FMS_CATEGORIES_CSS = `
.cp-page{ max-width:1280px; margin:0 auto; padding:24px 24px 64px; }
.cp-header{
  display:flex; align-items:center; justify-content:space-between;
  gap:16px; margin:0 0 24px;
}
.cp-header h1{ margin:0; font-size:24px; line-height:1.25; font-weight:600; color:#111827; }
.cp-button{
  display:inline-flex; align-items:center; justify-content:center;
  min-height:44px; padding:8px 16px; border:0; border-radius:8px;
  background:var(--color-primary,#2563eb); color:#ffffff;
  font-size:14px; font-weight:500; line-height:1.5; cursor:pointer;
  transition:background-color 150ms ease-out;
}
.cp-button:hover{ background:var(--color-primary-hover,#1d4ed8); }
.cp-button:focus-visible{ outline:2px solid #2563eb; outline-offset:2px; }

.cp-cards{
  display:grid; grid-template-columns:1fr; gap:24px;
  align-items:start;
}
.cp-card{
  background:var(--color-surface,#ffffff); border:1px solid var(--color-border,#e5e7eb);
  border-radius:12px; box-shadow:var(--shadow-sm,0 1px 2px rgba(0,0,0,.05));
  overflow:hidden;
}
.cp-card__header{
  display:flex; align-items:baseline; gap:12px;
  padding:16px; border-bottom:1px solid var(--color-border,#e5e7eb);
}
.cp-card__header h2{ margin:0; font-size:18px; line-height:1.25; font-weight:600; color:#111827; }
.cp-count{ font-size:14px; font-weight:400; line-height:1.5; color:var(--color-text-muted,#6b7280); }

.cp-list{ list-style:none; margin:0; padding:8px 0; }
.cp-sublist{ list-style:none; margin:0; padding:0; }
.cp-row{
  display:flex; align-items:center; gap:12px;
  padding:12px 16px; min-height:44px;
  transition:background-color 150ms ease-out;
}
.cp-row:hover{ background:var(--color-surface-muted,#f9fafb); }
.cp-row:focus-within{ background:var(--color-surface-muted,#f9fafb); }
.cp-row--sub{
  padding-left:24px; border-left:2px solid var(--color-border,#e5e7eb);
  margin-left:16px;
}
.cp-name{
  font-size:16px; font-weight:500; line-height:1.5; color:#111827;
  overflow-wrap:anywhere;
}

.cp-badge{
  display:inline-flex; align-items:center; gap:6px;
  font-size:12px; font-weight:500; line-height:1.5;
  padding:2px 8px; border-radius:9999px; white-space:nowrap;
}
.cp-badge--income{ color:#047857; background:#ecfdf5; }
.cp-badge--expense{ color:#b45309; background:#fffbeb; }
.cp-badge--inuse{ color:#4b5563; background:#f3f4f6; }
.cp-dot{ width:8px; height:8px; border-radius:9999px; background:#3b82f6; flex:none; }

.cp-row__actions{
  margin-left:auto; display:flex; align-items:center; gap:8px; flex:none;
}
.cp-action{
  display:inline-flex; align-items:center; justify-content:center;
  min-width:44px; min-height:44px; padding:0; border:0; border-radius:8px;
  color:#6b7280; background:transparent; cursor:pointer;
  transition:background-color 150ms ease-out, color 150ms ease-out;
}
.cp-action:hover{ background:#f3f4f6; }
.cp-action--merge{ color:#2563eb; background:#eff6ff; }
.cp-action--merge:hover{ background:#dbeafe; }
.cp-action--danger{ color:#dc2626; background:transparent; }
.cp-action--danger:hover{ background:#fee2e2; }
.cp-action:focus-visible{ outline:2px solid #2563eb; outline-offset:2px; }

/* Skeleton group card mirrors the real layout to avoid CLS. */
.cp-card--skeleton .cp-list{ padding:16px; display:flex; flex-direction:column; gap:12px; }
.cp-skel-row{ display:flex; align-items:center; gap:12px; width:100%; }

.cp-sr{
  position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden;
  clip:rect(0 0 0 0); white-space:nowrap; border:0;
}
.cp-empty{ border:0; border-radius:0; background:transparent; }

@media (min-width:768px){
  .cp-cards{ grid-template-columns:repeat(2, minmax(0,1fr)); gap:24px; }
}
@media (max-width:767px){
  .cp-page{ padding:16px 16px 64px; }
  .cp-row--sub{ padding-left:16px; margin-left:12px; }
  .cp-header{ flex-direction:column; align-items:flex-start; gap:12px; }
}
@media (prefers-reduced-motion: reduce){
  .cp-row, .cp-action, .cp-button{ transition:none; }
}
`;

const FMS_CATEGORIES_STYLE_ID = 'fms-categories-page-styles';

function styleElements(): ReactNode {
  if (
    typeof document === 'undefined' ||
    document.getElementById(FMS_CATEGORIES_STYLE_ID)
  ) {
    return null;
  }
  return (
    <style id={FMS_CATEGORIES_STYLE_ID} data-fms>
      {FMS_CATEGORIES_CSS}
    </style>
  );
}

// ---------------------------------------------------------------- Helpers

/** Generate a fresh idempotency key so a retried delete can never fire twice. */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `cp-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

interface ErrorLike {
  name?: string;
  message?: string;
}

/** Map a thrown value to a stable, solution-oriented user message. */
function describeError(err: unknown): string {
  const e = err as ErrorLike | null;
  const name = e?.name ?? '';
  if (name === 'ConflictError') return 'This category was modified by another user.';
  if (name === 'ForbiddenError') return "You don't have permission to delete categories.";
  if (name === 'NotFoundError') return 'This category no longer exists.';
  return e?.message ?? 'The category was not deleted. Please retry.';
}

/** Split a type's categories into top-level, per-parent subcategories, orphans. */
function groupCategories(items: CategoryDTO[]): CategoryGroup {
  const top: CategoryDTO[] = [];
  const byParent = new Map<string, CategoryDTO[]>();
  const orphans: CategoryDTO[] = [];
  const topIds = new Set<string>();

  for (const c of items) {
    if (c.parentId === null) {
      top.push(c);
      topIds.add(c.id);
    }
  }
  for (const c of items) {
    if (c.parentId === null) continue;
    if (topIds.has(c.parentId)) {
      const list = byParent.get(c.parentId) ?? [];
      list.push(c);
      byParent.set(c.parentId, list);
    } else {
      orphans.push(c);
    }
  }
  return { top, byParent, orphans };
}

// ---------------------------------------------------------------- Component

export function CategoriesPage({
  onCreateCategory,
  onEdit,
  onBackToDashboard,
  onToast,
  onInvalidateCaches,
}: CategoriesPageProps) {
  const { can } = usePermission();
  const canWrite = can('category:write');
  const canRead = can('category:read');

  const [categories, setCategories] = useState<CategoryDTO[]>([]);
  const [loadState, setLoadState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);

  const [deleteTarget, setDeleteTarget] = useState<CategoryDTO | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [mergeSource, setMergeSource] = useState<CategoryDTO | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;

    async function load(): Promise<void> {
      setLoadState('loading');
      setLoadError(null);
      try {
        const page = await listCategories({ pageSize: PAGE_SIZE }, controller.signal);
        if (cancelled) return;
        setCategories(page.items ?? []);
        setLoadState('ready');
      } catch (err) {
        if (cancelled) return;
        if ((err as ErrorLike | null)?.name === 'AbortError') return;
        setLoadError(describeError(err));
        setLoadState('error');
      }
    }
    void load();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [retryNonce]);

  const handleDelete = async (): Promise<void> => {
    const target = deleteTarget;
    if (!target || deletingId !== null) return;
    setDeletingId(target.id);
    try {
      await deleteCategory(target.id, { idempotencyKey: newIdempotencyKey() });
      onInvalidateCaches?.();
      onToast?.({
        severity: 'success',
        title: 'Category deleted',
        body: `${target.name} was deleted.`,
      });
      setDeleteTarget(null);
      setRetryNonce((n) => n + 1);
    } catch (err) {
      const name = (err as ErrorLike | null)?.name;
      if (name === 'ConflictError') {
        onToast?.({
          severity: 'error',
          title: 'Could not delete category',
          body: `${target.name} is now in use. Merge it instead.`,
        });
        setRetryNonce((n) => n + 1);
      } else if (name !== 'AbortError') {
        onToast?.({ severity: 'error', title: 'Could not delete category', body: describeError(err) });
      }
    } finally {
      setDeletingId(null);
    }
  };

  const handleMerged = (): void => {
    const name = mergeSource?.name;
    onInvalidateCaches?.();
    onToast?.({ severity: 'success', title: 'Category merged', body: `${name ?? 'Category'} was merged.` });
    setMergeSource(null);
    setRetryNonce((n) => n + 1);
  };

  const income = categories.filter((c) => c.type === 'INCOME');
  const expense = categories.filter((c) => c.type === 'EXPENSE');

  const renderRow = (category: CategoryDTO, isSub: boolean): ReactNode => {
    const typeLabel = category.type === 'INCOME' ? 'Income' : 'Expense';
    const typeClass = category.type === 'INCOME' ? 'income' : 'expense';
    return (
      <li key={category.id} className={isSub ? 'cp-row cp-row--sub' : 'cp-row'}>
        <span className="cp-name">{category.name}</span>
        <span className={`cp-badge cp-badge--${typeClass}`}>{typeLabel}</span>
        {category.inUse && (
          <span className="cp-badge cp-badge--inuse">
            <span className="cp-dot" aria-hidden="true" />
            In use
          </span>
        )}
        <span className="cp-row__actions">
          {canWrite && onEdit && (
            <button
              type="button"
              className="cp-action"
              aria-label={`Edit ${category.name}`}
              title="Edit category"
              onClick={() => onEdit(category)}
            >
              <svg
                viewBox="0 0 24 24"
                width="18"
                height="18"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z" />
              </svg>
            </button>
          )}
          {canWrite && category.inUse ? (
            <button
              type="button"
              className="cp-action cp-action--merge"
              aria-label={`Merge ${category.name}`}
              title="Merge into another category"
              onClick={() => setMergeSource(category)}
            >
              <svg
                viewBox="0 0 24 24"
                width="18"
                height="18"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M8 3v4a2 2 0 0 0 2 2h4a2 2 0 0 1 2 2v10" />
                <path d="M16 3v4a2 2 0 0 1-2 2h-4a2 2 0 0 0-2 2v10" />
                <path d="M8 3H4" />
                <path d="M20 3h-4" />
              </svg>
            </button>
          ) : canWrite ? (
            <button
              type="button"
              className="cp-action cp-action--danger"
              aria-label={`Delete ${category.name}`}
              title="Delete category"
              onClick={() => setDeleteTarget(category)}
            >
              <svg
                viewBox="0 0 24 24"
                width="18"
                height="18"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M3 6h18" />
                <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
                <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
              </svg>
            </button>
          ) : null}
        </span>
      </li>
    );
  };

  /** Render a group card (list or per-group empty state). */
  const renderGroup = (type: CategoryType): ReactNode => {
    const items = type === 'INCOME' ? income : expense;
    const groupTitle = type === 'INCOME' ? 'Income' : 'Expense';
    const { top, byParent, orphans } = groupCategories(items);

    return (
      <section className="cp-card" aria-label={groupTitle}>
        {top.length === 0 && orphans.length === 0 ? (
          <EmptyState
            className="cp-empty"
            tone="info"
            title={type === 'INCOME' ? 'No income categories yet' : 'No expense categories yet'}
            description="Add your first category to start tracking against it."
            actionLabel={canWrite && onCreateCategory ? 'Create your first category' : undefined}
            onAction={canWrite && onCreateCategory ? onCreateCategory : undefined}
            role="status"
          />
        ) : (
          <>
            <div className="cp-card__header">
              <h2>{groupTitle}</h2>
              <span className="cp-count">
                {items.length} category{items.length === 1 ? '' : 's'}
              </span>
            </div>
            <ul className="cp-list">
              {top.map((c) => (
                <li key={c.id} className="cp-group-item">
                  {renderRow(c, false)}
                  {(byParent.get(c.id) ?? []).length > 0 && (
                    <ul className="cp-sublist">
                      {(byParent.get(c.id) ?? []).map((sub) => renderRow(sub, true))}
                    </ul>
                  )}
                </li>
              ))}
              {orphans.map((c) => renderRow(c, false))}
            </ul>
          </>
        )}
      </section>
    );
  };

  /** Loading skeleton mirrors the final two-card layout (single shared region). */
  const renderSkeleton = (): ReactNode => (
    <div role="status">
      <span className="cp-sr">Loading categories</span>
      <div aria-hidden="true">
        <div className="cp-cards">
          <section className="cp-card cp-card--skeleton">
            <div className="cp-card__header">
              <Skeleton variant="text" lines={1} width="40%" height={18} announceOnLoad={false} />
            </div>
            <div className="cp-list">
              {[0, 1, 2, 3].map((i) => (
                <div key={i} className="cp-skel-row">
                  <Skeleton variant="rect" width="55%" height={16} announceOnLoad={false} />
                  <Skeleton variant="rect" width={48} height={16} announceOnLoad={false} />
                  <Skeleton variant="rect" width={88} height={16} announceOnLoad={false} />
                </div>
              ))}
            </div>
          </section>
          <section className="cp-card cp-card--skeleton">
            <div className="cp-card__header">
              <Skeleton variant="text" lines={1} width="40%" height={18} announceOnLoad={false} />
            </div>
            <div className="cp-list">
              {[0, 1, 2, 3].map((i) => (
                <div key={i} className="cp-skel-row">
                  <Skeleton variant="rect" width="55%" height={16} announceOnLoad={false} />
                  <Skeleton variant="rect" width={48} height={16} announceOnLoad={false} />
                  <Skeleton variant="rect" width={88} height={16} announceOnLoad={false} />
                </div>
              ))}
            </div>
          </section>
        </div>
      </div>
    </div>
  );

  let body: ReactNode;
  if (!canRead) {
    body = (
      <EmptyState
        tone="info"
        title="Insufficient permissions"
        description="You need read access to view categories."
        actionLabel={onBackToDashboard ? 'Back to dashboard' : undefined}
        onAction={onBackToDashboard}
        role="alert"
      />
    );
  } else if (loadState === 'loading') {
    body = renderSkeleton();
  } else if (loadState === 'error') {
    body = (
      <EmptyState
        tone="info"
        title="Failed to load categories"
        description={
          loadError
            ? `We couldn't load categories: ${loadError}`
            : "We couldn't load categories. Check your connection and retry."
        }
        actionLabel="Retry"
        onAction={() => setRetryNonce((n) => n + 1)}
        role="alert"
      />
    );
  } else {
    body = (
      <div className="cp-cards">
        {renderGroup('INCOME')}
        {renderGroup('EXPENSE')}
      </div>
    );
  }

  return (
    <main className="cp-page">
      {styleElements()}
      <header className="cp-header">
        <h1>Categories</h1>
        {canWrite && onCreateCategory && (
          <button type="button" className="cp-button" onClick={onCreateCategory}>
            New category
          </button>
        )}
      </header>

      {body}

      <ConfirmDialog
        open={deleteTarget !== null}
        title="Delete category"
        consequence={
          deleteTarget
            ? `Delete "${deleteTarget.name}"? This category is not currently used. Deleting it is permanent and cannot be undone.`
            : ''
        }
        confirmLabel="Delete"
        cancelLabel="Cancel"
        danger
        loading={deletingId !== null}
        onConfirm={() => void handleDelete()}
        onCancel={() => {
          if (deletingId === null) setDeleteTarget(null);
        }}
      />

      <MergeCategoryDialog
        source={mergeSource ?? (categories[0] as CategoryDTO)}
        open={mergeSource !== null}
        onClose={() => setMergeSource(null)}
        onMerged={handleMerged}
      />
    </main>
  );
}

export default CategoriesPage;