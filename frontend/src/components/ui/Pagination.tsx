/**
 * Stateless, accessible cursor/offset pagination control (Blueprint §7.2).
 *
 * Cursor mode (high-volume transaction lists, default 100/page) renders a
 * Prev/Next pair plus a human-readable "Showing X–Y of Z" range. Offset mode
 * (bounded collections) renders a page-number cluster capped at 7 buttons
 * (Miller's law) with First/Last affordances and ellipses when the page count
 * exceeds 7. All state lives with the parent store/hook; this component never
 * fetches, mutates, or derives data. Navigation only emits idempotent reads
 * via `onChange`.
 *
 * Security notes: every dynamic value is rendered through JSX interpolation
 * only (no `dangerouslySetInnerHTML`/inline event handlers), incoming props
 * are coerced and bounds-checked at the boundary, raw cursor tokens are never
 * displayed as text, and the rendered page-button count is hard-capped to
 * avoid DOM explosion even from a hostile `total`.
 */
import type { ReactElement, RefObject } from 'react';

export interface CursorPagination {
  next?: string | null;
  prev?: string | null;
}

export interface OffsetPagination {
  page: number;
  total: number;
  pageSize: number;
}

export interface PaginationLabels {
  previous: string;
  next: string;
  first: string;
  last: string;
  range: string;
  pageStatus: string;
}

export interface PaginationProps {
  /** Cursor-mode descriptor. Providing it selects cursor rendering. */
  cursor?: CursorPagination;
  /**
   * Offset-mode descriptor. Providing it selects offset rendering. If both
   * `cursor` and `offset` are supplied (or neither), the control renders
   * nothing — the contract does not define a valid interpretation.
   */
  offset?: OffsetPagination;
  /**
   * Emits an idempotent read request to the parent store/hook:
   * `{ cursor }` for cursor mode, `{ page }` for offset mode.
   */
  onChange: (payload: { cursor?: CursorPagination; page?: number }) => void;
  /** While true, all buttons are disabled and `<nav aria-busy="true">` is set. */
  loading?: boolean;
  /** Total records behind the current cursor (used for the range string). */
  cursorTotal?: number;
  /** 1-based index of the first record in the current cursor window. */
  cursorStart?: number;
  /** Page size used for the cursor range string. Defaults to 100. */
  cursorPageSize?: number;
  /** Locale for number grouping via `Intl.NumberFormat`. Defaults to 'en-US'. */
  locale?: string;
  /** Optional label overrides; defaults are static English literals. */
  labels?: Partial<PaginationLabels>;
  /**
   * Optional ref targeting the results heading/table wrapper (consumer marks
   * it `tabIndex={-1}`). Focus is moved there after a page change to avoid
   * focus loss and jarring jumps.
   */
  focusTargetRef?: RefObject<HTMLElement | null>;
}

const DEFAULT_LABELS: PaginationLabels = {
  previous: 'Previous page',
  next: 'Next page',
  first: 'Go to first page',
  last: 'Go to last page',
  range: 'Showing {start}–{end} of {total}',
  pageStatus: 'Page {page} of {total}',
};

type WindowItem = number | 'start-ellipsis' | 'end-ellipsis';

/**
 * Colocated, injected once as a <style> element (no per-element inline
 * `style=` attributes) so the control holds under a strict `style-src` CSP.
 * Only color/opacity/transform transitions are animated — no layout props.
 */
const FMS_PAGINATION_CSS = `
.fms-pagination{
  display:block; box-sizing:border-box; width:100%; max-width:1280px;
  margin-top:var(--fms-pagination-margin-top,16px); padding:var(--fms-pagination-padding-y,8px) 0;
  font-size:var(--fms-text-sm,14px); line-height:var(--fms-leading-normal,1.5);
}
.fms-pagination__row{
  display:flex; align-items:center; flex-wrap:wrap; justify-content:center; gap:var(--fms-pagination-gap-sm,4px);
}
.fms-pagination__btn{
  display:inline-flex; align-items:center; justify-content:center; gap:var(--fms-pagination-gap-sm,4px);
  min-width:var(--fms-pagination-btn-min-w,44px); min-height:var(--fms-pagination-btn-h,44px);
  padding:0 var(--fms-pagination-btn-pad-x,12px);
  background-color:var(--color-surface,#ffffff); color:var(--color-text,#1f2937);
  border:1px solid var(--color-border,#d1d5db); border-radius:var(--fms-radius-md,8px);
  font-size:var(--fms-text-sm,14px); line-height:var(--fms-leading-normal,1.5);
  font-weight:var(--fms-font-medium,500); cursor:pointer; user-select:none; white-space:nowrap;
  transition:background-color var(--fms-transition-fast,150ms ease-out),border-color var(--fms-transition-fast,150ms ease-out),color var(--fms-transition-fast,150ms ease-out);
}
.fms-pagination__btn:hover:not(:disabled){
  background-color:var(--color-surface-hover,#f3f4f6); border-color:var(--color-border-strong,#9ca3af);
}
.fms-pagination__btn:focus-visible{
  outline:2px solid var(--color-focus-ring,#4f46e5); outline-offset:2px;
}
.fms-pagination__btn:disabled{ opacity:0.5; cursor:not-allowed; pointer-events:none; }
.fms-pagination__chev{ flex:none; font-size:var(--fms-text-base,16px); line-height:1; }
.fms-pagination__edge-label{ display:none; }
.fms-pagination__nums{ display:none; align-items:center; gap:var(--fms-pagination-gap-sm,4px); }
.fms-pagination__current{
  display:inline-flex; align-items:center; justify-content:center;
  min-width:var(--fms-pagination-btn-min-w,44px); min-height:var(--fms-pagination-btn-h,44px);
  padding:0 var(--fms-pagination-btn-pad-x,12px);
  background-color:var(--color-primary,#4f46e5); color:var(--color-primary-contrast,#ffffff);
  border:1px solid var(--color-primary,#4f46e5); border-radius:var(--fms-radius-md,8px);
  font-size:var(--fms-text-sm,14px); line-height:var(--fms-leading-normal,1.5);
  font-weight:var(--fms-font-semibold,600);
  box-shadow:0 1px 2px rgba(0,0,0,0.08);
}
.fms-pagination__ellipsis{
  display:inline-flex; align-items:center; justify-content:center;
  min-width:var(--fms-pagination-btn-min-w,44px); min-height:var(--fms-pagination-btn-h,44px);
  color:var(--color-text-muted,#6b7280); font-weight:var(--fms-font-normal,400);
}
.fms-pagination__info{
  margin-left:var(--fms-pagination-gap-lg,8px); padding:0 var(--fms-pagination-btn-pad-x,12px);
  color:var(--color-text-muted,#6b7280); font-weight:var(--fms-font-normal,400);
}
.fms-pagination__placeholder{
  display:inline-flex; align-items:center; justify-content:center;
  min-width:var(--fms-pagination-btn-min-w,44px); min-height:var(--fms-pagination-btn-h,44px);
  visibility:hidden;
}
.fms-pagination__spinner{
  display:inline-block; width:16px; height:16px; margin-right:var(--fms-pagination-gap-sm,4px);
  border:2px solid transparent; border-top-color:var(--color-text-muted,#6b7280);
  border-radius:50%; animation:fms-pagination-spin 700ms linear infinite;
}
.fms-pagination__sr{
  position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden;
  clip:rect(0 0 0 0); white-space:nowrap; border:0;
}
@keyframes fms-pagination-spin{ to{ transform:rotate(360deg); } }
@media (min-width:768px){
  .fms-pagination__row{ justify-content:flex-end; }
  .fms-pagination__edge-label{ display:inline; }
  .fms-pagination__nums{ display:inline-flex; }
}
@media (prefers-reduced-motion: reduce){
  .fms-pagination__btn,.fms-pagination__current{ transition-duration:0.01ms; }
  .fms-pagination__spinner{ animation-duration:0.01ms; }
}
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-pagination-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = FMS_PAGINATION_CSS;
    document.head.appendChild(el);
  }
}

/** Coerce a value to a finite, non-negative integer, or `null` when invalid. */
function toCount(value: unknown): number | null {
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(n)) return null;
  const rounded = Math.round(n);
  if (!Number.isInteger(n) || rounded !== n) return null;
  return rounded;
}

function clampPage(page: number, totalPages: number): number {
  return Math.max(1, Math.min(page, totalPages));
}

/**
 * Builds the page-number window for offset mode. For `totalPages <= 7` the
 * full range is returned; otherwise a bounded window (current ± 1) with
 * ellipsis markers is produced — never iterating over an unbounded `total`.
 */
function buildOffsetWindow(page: number, totalPages: number): WindowItem[] {
  if (totalPages <= 7) {
    const items: WindowItem[] = [];
    for (let i = 1; i <= totalPages; i += 1) items.push(i);
    return items;
  }
  const set = new Set<number>([page, page - 1, page + 1]);
  const nums = Array.from(set)
    .filter((n) => n >= 2 && n <= totalPages - 1)
    .sort((a, b) => a - b);
  const items: WindowItem[] = [];
  if (nums.length > 0 && nums[0] > 2) items.push('start-ellipsis');
  for (const n of nums) items.push(n);
  if (nums.length > 0 && nums[nums.length - 1] < totalPages - 1) {
    items.push('end-ellipsis');
  }
  return items;
}

/** Formats a non-negative integer with locale-aware grouping. */
function formatNumber(value: number, locale: string): string {
  return new Intl.NumberFormat(locale).format(value);
}

type Edge = 'prev' | 'next';

export function Pagination({
  cursor,
  offset,
  onChange,
  loading = false,
  cursorTotal,
  cursorStart = 1,
  cursorPageSize = 100,
  locale = 'en-US',
  labels,
  focusTargetRef,
}: PaginationProps): ReactElement | null {
  const resolvedLabels = { ...DEFAULT_LABELS, ...labels };

  const offsetRawPage = toCount(offset?.page);
  const offsetTotal = toCount(offset?.total);
  const offsetPageSize = toCount(offset?.pageSize);

  // Render nothing for an ambiguous/invalid declaration, or an empty set.
  const invalidOffset =
    offset !== undefined &&
    (offsetRawPage === null ||
      offsetTotal === null ||
      offsetPageSize === null ||
      offsetTotal <= 0 ||
      offsetPageSize <= 0);
  const hasBoth = cursor !== undefined && offset !== undefined;
  if (hasBoth || (!cursor && !offset) || invalidOffset) return null;

  const cursorPrev =
    cursor?.prev !== undefined && cursor?.prev !== null ? String(cursor.prev) : null;
  const cursorNext =
    cursor?.next !== undefined && cursor?.next !== null ? String(cursor.next) : null;

  const moveFocus = () => {
    if (focusTargetRef?.current) {
      // Consumer marks the results heading/table wrapper tabIndex={-1}.
      focusTargetRef.current.focus({ preventScroll: true });
    }
  };

  const handleCursorEdge = (edge: Edge) => {
    if (loading) return;
    const token = edge === 'prev' ? cursorPrev : cursorNext;
    if (!token) return;
    onChange({ cursor: { [edge]: token } });
    moveFocus();
  };

  if (offsetTotal !== null && offsetPageSize !== null && offsetRawPage !== null) {
    const totalPages = Math.max(1, Math.ceil(offsetTotal / offsetPageSize));
    const page = clampPage(offsetRawPage, totalPages);
    const windowItems = buildOffsetWindow(page, totalPages);
    const showMacroAccess = totalPages > 7;

    const handlePage = (target: number) => {
      if (loading || target === page) return;
      onChange({ page: target });
      moveFocus();
    };

    const pageStatus = resolvedLabels.pageStatus
      .replace('{page}', formatNumber(page, locale))
      .replace('{total}', formatNumber(totalPages, locale));

    return (
      <nav
        aria-label="Pagination"
        aria-busy={loading ? true : undefined}
        className="fms-pagination"
      >
        <div className="fms-pagination__row">
          <button
            type="button"
            className="fms-pagination__btn"
            aria-label={resolvedLabels.previous}
            disabled={page <= 1 || loading}
            onClick={() => handlePage(Math.max(1, page - 1))}
          >
            <span className="fms-pagination__chev" aria-hidden="true">
              {'\u2039'}
            </span>
            <span className="fms-pagination__edge-label">{resolvedLabels.previous}</span>
          </button>

          {showMacroAccess && (
            <button
              type="button"
              className="fms-pagination__btn fms-pagination__num"
              aria-label={resolvedLabels.first}
              disabled={page === 1 || loading}
              onClick={() => handlePage(1)}
            >
              {formatNumber(1, locale)}
            </button>
          )}

          <div className="fms-pagination__nums" role="group" aria-label="Pages">
            {windowItems.map((item) => {
              if (item === 'start-ellipsis' || item === 'end-ellipsis') {
                return (
                  <span key={item} className="fms-pagination__ellipsis" aria-hidden="true">
                    {'\u2026'}
                  </span>
                );
              }
              if (item === page) {
                return (
                  <span
                    key={`page-${item}`}
                    className="fms-pagination__current"
                    aria-current="page"
                    aria-label={`Page ${formatNumber(item, locale)} of ${formatNumber(totalPages, locale)}`}
                  >
                    {formatNumber(item, locale)}
                  </span>
                );
              }
              return (
                <button
                  key={`page-${item}`}
                  type="button"
                  className="fms-pagination__btn fms-pagination__num"
                  aria-label={`Page ${formatNumber(item, locale)}`}
                  disabled={loading}
                  onClick={() => handlePage(item)}
                >
                  {formatNumber(item, locale)}
                </button>
              );
            })}
          </div>

          {showMacroAccess && (
            <button
              type="button"
              className="fms-pagination__btn fms-pagination__num"
              aria-label={resolvedLabels.last}
              disabled={page === totalPages || loading}
              onClick={() => handlePage(totalPages)}
            >
              {formatNumber(totalPages, locale)}
            </button>
          )}

          <button
            type="button"
            className="fms-pagination__btn"
            aria-label={resolvedLabels.next}
            disabled={page >= totalPages || loading}
            onClick={() => handlePage(Math.min(totalPages, page + 1))}
          >
            <span className="fms-pagination__edge-label">{resolvedLabels.next}</span>
            <span className="fms-pagination__chev" aria-hidden="true">
              {'\u203A'}
            </span>
          </button>

          <span className="fms-pagination__info">
            {loading && <span className="fms-pagination__spinner" aria-hidden="true" />}
            {pageStatus}
          </span>
        </div>
        <span className="fms-pagination__sr" role="status">
          {pageStatus}
        </span>
      </nav>
    );
  }

  // Cursor mode.
  const start = Math.max(1, toCount(cursorStart) ?? 1);
  const count = toCount(cursorTotal);
  const rangeText =
    count !== null && count >= start
      ? resolvedLabels.range
          .replace('{start}', formatNumber(start, locale))
          .replace(
            '{end}',
            formatNumber(
              Math.min(start + Math.max(0, toCount(cursorPageSize) ?? 100) - 1, count),
              locale,
            ),
          )
          .replace('{total}', formatNumber(count, locale))
      : null;
  const statusText = rangeText ?? (loading ? 'Loading' : 'Loaded page');

  return (
    <nav
      aria-label="Pagination"
      aria-busy={loading ? true : undefined}
      className="fms-pagination"
    >
      <div className="fms-pagination__row">
        {cursorPrev ? (
          <button
            type="button"
            className="fms-pagination__btn"
            aria-label={resolvedLabels.previous}
            disabled={loading}
            onClick={() => handleCursorEdge('prev')}
          >
            <span className="fms-pagination__chev" aria-hidden="true">
              {'\u2039'}
            </span>
            <span className="fms-pagination__edge-label">{resolvedLabels.previous}</span>
          </button>
        ) : (
          <span className="fms-pagination__placeholder" aria-hidden="true" />
        )}

        <span className="fms-pagination__info">
          {loading && <span className="fms-pagination__spinner" aria-hidden="true" />}
          {rangeText}
        </span>

        {cursorNext ? (
          <button
            type="button"
            className="fms-pagination__btn"
            aria-label={resolvedLabels.next}
            disabled={loading}
            onClick={() => handleCursorEdge('next')}
          >
            <span className="fms-pagination__edge-label">{resolvedLabels.next}</span>
            <span className="fms-pagination__chev" aria-hidden="true">
              {'\u203A'}
            </span>
          </button>
        ) : (
          <span className="fms-pagination__placeholder" aria-hidden="true" />
        )}
      </div>
      <span className="fms-pagination__sr" role="status">
        {statusText}
      </span>
    </nav>
  );
}

export default Pagination;