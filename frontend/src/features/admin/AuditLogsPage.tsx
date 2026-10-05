/**
 * Audit-trail viewer (Module G).
 *
 * Append-only, read-only view of audit log entries scoped behind the
 * `audit:read` permission (enforced by the route guard / server; this
 * component renders an inline 403 when the guard reports `canReadAudit` false).
 *
 * Security notes (mirrors services/api/endpoints.ts + types.ts contracts):
 * - Every server-derived value (actor, entity, entityId, before/after diffs,
 *   action labels) is rendered via JSX text nodes only. No
 *   `dangerouslySetInnerHTML` / `innerHTML` / `v-html`, and no `eval` /
 *   `new Function` anywhere in the tree.
 * - `before`/`after` snapshots are untrusted unvalidated backend data; the
 *   diff renderer only ever emits escaped text through `String` /
 *   `JSON.stringify`.
 * - `entityId` is displayed as plain text, never as a link, so the page issues
 *   no un-scoped entity fetches (no IDOR surface).
 * - Collapsed-by-default expandable rows keep sensitive deltas out of the
 *   initial DOM (defense-in-depth against shoulder-surfing).
 */
import { Fragment, useMemo, useRef, useState } from 'react';
import { useDebouncedQuery } from '../../hooks/useDebounce';
import { listAuditLogs } from '../../services/api/endpoints';
import type { AuditAction, AuditLogEntryDTO, OffsetPage } from '../../services/api/types';
import { isRangeValid } from '../../utils/date';
import { EmptyState } from '../../components/ui/EmptyState';
import { Pagination } from '../../components/ui/Pagination';
import { Skeleton } from '../../components/ui/Skeleton';

/** Route-guard-resolved props. The guard/RBAC lives in the route wrapper; this
 * page only renders the (non-modal) insufficient-permission state. */
export interface AuditLogsPageProps {
  /** True when the current session holds `audit:read` (gateway-authoritative). */
  canReadAudit: boolean;
  /** Optional navigation for the 403 "Back to dashboard" control. */
  onBackToDashboard?: () => void;
}

/** Action options for the filter select (exact domain vocabulary, Blueprint §5.5). */
const ACTION_OPTIONS: ReadonlyArray<AuditAction> = ['CREATE', 'EDIT', 'DELETE', 'READ'];

/** Glyph + label for each audit action. Text and icon are always rendered
 * together so color is never the only indicator (WCAG 1.4.1). */
const ACTION_META: Record<AuditAction, { glyph: string; label: string }> = {
  CREATE: { glyph: '\u002B', label: 'CREATE' },
  EDIT: { glyph: '\u270E', label: 'EDIT' },
  DELETE: { glyph: '\u{1F5D1}', label: 'DELETE' },
  READ: { glyph: '\u{1F441}', label: 'READ' },
};

/** Decorative 24px inline SVG used by EmptyState; rendered aria-hidden and fed
 * only a static path string (never server data). */
function EmptyIcon({ d }: { d: string }): JSX.Element {
  return (
    <svg
      viewBox="0 0 24 24"
      width="48"
      height="48"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      aria-hidden="true"
    >
      <path d={d} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Renders one action badge: icon + text, distinct READ treatment. */
function ActionBadge({ action }: { action: AuditAction }): JSX.Element {
  const meta = ACTION_META[action];
  return (
    <span className={`al-badge al-badge--${action.toLowerCase()}`}>
      <span className="al-badge__glyph" aria-hidden="true">
        {meta.glyph}
      </span>
      <span className="al-badge__label">{meta.label}</span>
    </span>
  );
}

/** Renders an untrusted `before`/`after` snapshot as plain text nodes only. */
function diffText(value: unknown): string {
  if (value === undefined || value === null) return '\u2014';
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return String(value);
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

/** Formats an ISO 8601 timestamp as `YYYY-MM-DD HH:MM:SS UTC` (unambiguous
 * temporal evidence for audit review). Empty on malformed input. */
function formatTimestamp(iso: string): string {
  try {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    return `${d.toISOString().replace('T', ' ').replace(/\.\d{3}Z$/, '')} UTC`;
  } catch {
    return '';
  }
}

// -- Colocated styles. Injected once, no inline `style=` on elements, and only
// -- transform/opacity/color/background are animated (CLS-safe, CSP-friendly).
const AUDIT_LOGS_CSS = `
.al-page{
  --color-bg-canvas:#F8FAFC;
  --color-bg-surface:#FFFFFF;
  --color-bg-subtle:#F9FAFB;
  --color-text-primary:#111827;
  --color-text-secondary:#4B5563;
  --color-text-muted:#6B7280;
  --color-border:#E5E7EB;
  --color-border-strong:#9CA3AF;
  --color-danger:#DC2626;
  --color-focus-ring:#4F46E5;
  --color-action-create:#16A34A;
  --color-action-edit:#2563EB;
  --color-action-delete:#DC2626;
  --color-action-read:#6B7280;
  --space-2:8px;--space-3:12px;--space-4:16px;--space-6:24px;--space-8:32px;--space-12:48px;
  --radius-sm:4px;--radius-md:8px;--radius-lg:12px;--radius-full:9999px;
  --shadow-sm:0 1px 2px rgba(0,0,0,0.05);
  --transition-fast:150ms ease-out;
  --transition-base:200ms ease-out;
  --transition-slow:300ms ease-out;

  max-width:1280px; margin:0 auto; padding:var(--space-6);
  color:var(--color-text-primary);
  background:var(--color-bg-canvas);
  font-size:16px; line-height:1.5;
}
.al-page *{ box-sizing:border-box; }
.al-page__sr-only{
  position:absolute; width:1px; height:1px; padding:0; margin:-1px;
  overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; border:0;
}
.al-header{ display:flex; align-items:center; gap:var(--space-3); margin-bottom:var(--space-6); }
.al-header h1{ font-size:24px; font-weight:600; line-height:1.25; margin:0; }
.al-pill{
  display:inline-flex; align-items:center; gap:var(--space-2);
  padding:var(--space-2) var(--space-3);
  background:var(--color-bg-subtle); color:var(--color-text-muted);
  border:1px solid var(--color-border); border-radius:var(--radius-full);
  font-size:12px; font-weight:500;
}
.al-pill__glyph{ line-height:1; }
.al-filter-card{
  background:var(--color-bg-surface); border:1px solid var(--color-border);
  border-radius:var(--radius-lg); box-shadow:var(--shadow-sm);
  padding:var(--space-4); margin-bottom:var(--space-4);
}
.al-filter-grid{
  display:grid; grid-template-columns:1fr; gap:var(--space-3);
}
.al-field{ display:flex; flex-direction:column; gap:var(--space-2); }
.al-field label{
  font-size:14px; font-weight:600; color:var(--color-text-secondary);
}
.al-field input,.al-field select{
  min-height:44px; padding:0 var(--space-3);
  background:var(--color-bg-surface); color:var(--color-text-primary);
  border:1px solid var(--color-border); border-radius:var(--radius-sm);
  font-size:16px; line-height:1.5;
}
.al-field input:focus-visible,.al-field select:focus-visible{
  outline:2px solid var(--color-focus-ring); outline-offset:2px;
}
.al-range{ display:flex; flex-direction:column; gap:var(--space-2); }
.al-field__group-label{ font-size:14px; font-weight:600; color:var(--color-text-secondary); }
.al-range-inputs{ display:flex; flex-direction:column; gap:var(--space-3); }
.al-range-error{
  margin:0; color:var(--color-danger); font-size:14px; line-height:1.5; font-weight:500;
}
.al-chips{ display:flex; flex-wrap:wrap; gap:var(--space-2); margin:0 0 var(--space-4); }
.al-chip{
  display:inline-flex; align-items:center; gap:var(--space-2);
  min-height:44px; padding:0 var(--space-3);
  background:var(--color-bg-surface); color:var(--color-text-primary);
  border:1px solid var(--color-border); border-radius:var(--radius-full);
  font-size:14px; cursor:pointer; user-select:none;
}
.al-chip[aria-pressed="true"]{
  background:var(--color-bg-subtle); border-color:var(--color-border-strong);
}
.al-chip:focus-visible{ outline:2px solid var(--color-focus-ring); outline-offset:2px; }
.al-chip__clear{ color:var(--color-text-muted); }
.al-chip--clearall{ border-color:var(--color-border-strong); font-weight:500; }
.al-table-wrap{
  overflow-x:auto; width:100%;
  background:var(--color-bg-surface); border:1px solid var(--color-border);
  border-radius:var(--radius-lg);
  max-height:70vh; position:relative;
}
.al-table{ width:100%; min-width:720px; border-collapse:collapse; }
.al-caption{
  text-align:left; padding:var(--space-3) var(--space-4);
  font-size:14px; color:var(--color-text-muted);
  caption-side:top;
}
.al-table thead th{
  position:sticky; top:0; z-index:1;
  background:var(--color-bg-surface);
  text-align:left; font-size:14px; font-weight:600; color:var(--color-text-secondary);
  padding:var(--space-3) var(--space-4); white-space:nowrap;
  border-bottom:1px solid var(--color-border);
}
.al-table tbody td{
  padding:var(--space-3) var(--space-4); vertical-align:middle;
  border-bottom:1px solid var(--color-border);
}
.al-table tbody tr{ min-height:48px; }
.al-table tbody tr:hover{ background:var(--color-bg-subtle); }
.al-cell-time{ white-space:nowrap; font-variant-numeric:tabular-nums; }
.al-row-button{
  display:inline-flex; align-items:center; justify-content:center;
  min-width:44px; min-height:44px; padding:0;
  background:transparent; border:1px solid transparent; border-radius:var(--radius-sm);
  color:var(--color-text-secondary); cursor:pointer;
}
.al-row-button:focus-visible{ outline:2px solid var(--color-focus-ring); outline-offset:2px; }
.al-row-button__chev{
  display:inline-flex; transition:transform var(--transition-base);
}
.al-row-button__chev.is-open{ transform:rotate(90deg); }
.al-badge{
  display:inline-flex; align-items:center; gap:var(--space-2);
  padding:var(--space-2) var(--space-3);
  border-radius:var(--radius-full); font-size:12px; font-weight:500;
  white-space:nowrap;
}
.al-badge__glyph{ line-height:1; }
.al-badge--create{ color:var(--color-action-create); background:#DCFCE7; }
.al-badge--edit{ color:var(--color-action-edit); background:#DBEAFE; }
.al-badge--delete{ color:var(--color-action-delete); background:#FEE2E2; }
.al-badge--read{ color:var(--color-action-read); background:#F3F4F6; }
.al-diff-row td{ padding:var(--space-3) var(--space-4); background:var(--color-bg-subtle); }
.al-diff{
  display:flex; flex-direction:column; gap:var(--space-3);
  animation:al-diff-in 200ms ease-out;
}
.al-diff-col{
  background:var(--color-bg-surface); border:1px solid var(--color-border);
  border-left:3px solid var(--color-border-strong); border-radius:var(--radius-md);
  padding:var(--space-3);
}
.al-diff-label{ display:block; font-size:14px; font-weight:600; color:var(--color-text-secondary); margin-bottom:var(--space-2); }
.al-diff-pre{
  margin:0; font-family:ui-monospace,SFMono-Regular,Menlo,monospace;
  font-size:14px; line-height:1.75; color:var(--color-text-primary);
  white-space:pre-wrap; overflow-wrap:anywhere; max-width:65ch;
}
.al-skeleton{ padding:var(--space-4); background:var(--color-bg-surface); border:1px solid var(--color-border); border-radius:var(--radius-lg); }
.al-skeleton .fms-skeleton{ margin-bottom:var(--space-3); }
.al-filtering{
  display:flex; align-items:center; gap:var(--space-2);
  margin:0 0 var(--space-3); color:var(--color-text-muted); font-size:14px;
}
.al-filtering__dot{
  width:8px; height:8px; border-radius:50%;
  border:2px solid transparent; border-top-color:var(--color-text-muted);
  animation:al-spin 700ms linear infinite;
}
@keyframes al-spin{ to{ transform:rotate(360deg); } }
@keyframes al-diff-in{ from{ opacity:0; transform:translateY(4px); } to{ opacity:1; transform:translateY(0); } }
@media (min-width:768px){
  .al-filter-grid{ grid-template-columns:2fr 1fr 1.5fr 2fr; }
  .al-range-inputs{ flex-direction:row; }
  .al-range-inputs .al-field{ flex:1; }
  .al-diff{ flex-direction:row; }
  .al-diff-col{ flex:1; }
}
@media (prefers-reduced-motion: reduce){
  .al-row-button__chev{ transition-duration:0.01ms; }
  .al-diff{ animation:none; }
  .al-filtering__dot{ animation-duration:0.01ms; }
}
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-auditlogs-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = AUDIT_LOGS_CSS;
    document.head.appendChild(el);
  }
}

/**
 * Audit-trail viewer: permission-scoped filter bar, paginated append-only
 * table with expandable before/after diffs, and distinct loading/empty/error
 * states. All audit-derived values render as escaped text nodes.
 */
export function AuditLogsPage({
  canReadAudit,
  onBackToDashboard,
}: AuditLogsPageProps): JSX.Element {
  // Controlled filter state; refreshed through the debounced query's deps.
  const [entityInput, setEntityInput] = useState('');
  const [actionInput, setActionInput] = useState<'' | AuditAction>('');
  const [actorInput, setActorInput] = useState('');
  const [fromDate, setFromDate] = useState('');
  const [toDate, setToDate] = useState('');
  const [rangeTouched, setRangeTouched] = useState(false);
  const [page, setPage] = useState(1);
  const [reloadKey, setReloadKey] = useState(0);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const tableWrapRef = useRef<HTMLDivElement>(null);

  // Inline-validated on blur (error prevention): start <= end. Equal bounds are
  // a valid single-instant window (isRangeValid contract).
  const rangeValid =
    fromDate === '' || toDate === '' ? true : isRangeValid(fromDate, toDate);
  const rangeError = rangeTouched && !rangeValid;

  // Debounced (>=150ms) + abortable fetch. Passing the raw filter states as
  // deps lets useDebouncedQuery own the debounce-and-cancel window.
  const { data, pending, error } = useDebouncedQuery<OffsetPage<AuditLogEntryDTO>>(
    (signal) =>
      listAuditLogs(
        {
          entity: entityInput.trim() || undefined,
          actorId: actorInput.trim() || undefined,
          from: rangeValid && rangeTouched ? fromDate || undefined : undefined,
          to: rangeValid && rangeTouched ? toDate || undefined : undefined,
        },
        signal,
      ),
    [entityInput, actorInput, rangeValid, rangeTouched, fromDate, toDate, page, reloadKey],
    150,
  );

  // GAP note: `listAuditLogs` does not accept an `action` filter, so the action
  // filter is applied client-side to the single server-returned page (a
  // correctness caveat until the endpoint is extended).
  const rows = useMemo<AuditLogEntryDTO[]>(() => {
    if (!data) return [];
    const items = data.items;
    return actionInput ? items.filter((r) => r.action === actionInput) : items;
  }, [data, actionInput]);

  const chips = useMemo(() => {
    const list: Array<{ key: string; label: string; clear: () => void }> = [];
    if (entityInput.trim()) {
      list.push({ key: 'entity', label: entityInput, clear: () => setEntityInput('') });
    }
    if (actionInput) {
      list.push({ key: 'action', label: actionInput, clear: () => setActionInput('') });
    }
    if (actorInput.trim()) {
      list.push({ key: 'actor', label: actorInput, clear: () => setActorInput('') });
    }
    if (fromDate) {
      list.push({ key: 'from', label: `From ${fromDate}`, clear: () => setFromDate('') });
    }
    if (toDate) {
      list.push({ key: 'to', label: `To ${toDate}`, clear: () => setToDate('') });
    }
    return list;
  }, [entityInput, actionInput, actorInput, fromDate, toDate]);

  const clearAllFilters = () => {
    setEntityInput('');
    setActionInput('');
    setActorInput('');
    setFromDate('');
    setToDate('');
    setRangeTouched(false);
    setPage(1);
  };

  // ---- Permission guard: inline, non-modal 403 (preserves history/back). ----
  if (!canReadAudit) {
    return (
      <main className="al-page">
        <header className="al-header">
          <h1>Audit Logs</h1>
        </header>
        <EmptyState
          tone="info"
          title="You don't have permission to view audit logs"
          description="Only users with the audit read permission can access the audit trail. Return to the dashboard to continue."
          icon={<EmptyIcon d="M12 15v2m-7 5h14a2 2 0 002-2V9a2 2 0 00-2-2H5a2 2 0 00-2 2v11a2 2 0 002 2z" />}
          actionLabel={onBackToDashboard ? 'Back to dashboard' : undefined}
          onAction={onBackToDashboard}
        />
      </main>
    );
  }

  // ---- Loading (initial) state. ----
  const showInitialSkeleton = pending && data === null && !error;
  // ---- Error state (distinct from empty). ----
  const showError = !pending && error !== null && data === null;
  // ---- Empty state. ----
  const showEmpty = !pending && data !== null && !error && rows.length === 0;

  return (
    <main className="al-page">
      <header className="al-header">
        <h1>Audit Logs</h1>
        <span className="al-pill">
          <span className="al-pill__glyph" aria-hidden="true">
            {'\u{1F512}'}
          </span>
          <span>Read-only record</span>
        </span>
      </header>

      <section className="al-filter-card" aria-label="Filter audit logs">
        <div className="al-filter-grid">
          <div className="al-field">
            <label htmlFor="al-entity">Entity type</label>
            <input
              id="al-entity"
              type="text"
              value={entityInput}
              onChange={(e) => setEntityInput(e.target.value)}
              placeholder="e.g. transaction"
            />
          </div>
          <div className="al-field">
            <label htmlFor="al-action">Action</label>
            <select
              id="al-action"
              value={actionInput}
              onChange={(e) => setActionInput(e.target.value as '' | AuditAction)}
            >
              <option value="">Action</option>
              {ACTION_OPTIONS.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </div>
          <div className="al-field">
            <label htmlFor="al-actor">Actor</label>
            <input
              id="al-actor"
              type="text"
              value={actorInput}
              onChange={(e) => setActorInput(e.target.value)}
              placeholder="User or user id"
            />
          </div>
          <div className="al-range" role="group" aria-labelledby="al-range-label">
            <span className="al-field__group-label" id="al-range-label">
              Date range
            </span>
            <div className="al-range-inputs">
              <div className="al-field">
                <label htmlFor="al-from">From</label>
                <input
                  id="al-from"
                  type="date"
                  value={fromDate}
                  onChange={(e) => setFromDate(e.target.value)}
                  onBlur={() => setRangeTouched(true)}
                  aria-describedby={rangeError ? 'al-range-error' : undefined}
                />
              </div>
              <div className="al-field">
                <label htmlFor="al-to">To</label>
                <input
                  id="al-to"
                  type="date"
                  value={toDate}
                  onChange={(e) => setToDate(e.target.value)}
                  onBlur={() => setRangeTouched(true)}
                  aria-describedby={rangeError ? 'al-range-error' : undefined}
                />
              </div>
            </div>
            {rangeError && (
              <p className="al-range-error" id="al-range-error" role="alert">
                End date is before start date — adjust the end date or clear the range.
              </p>
            )}
          </div>
        </div>
      </section>

      {chips.length > 0 && (
        <div className="al-chips" aria-label="Active filters">
          {chips.map((c) => (
            <button
              key={c.key}
              type="button"
              className="al-chip"
              aria-pressed="true"
              onClick={() => {
                c.clear();
                setPage(1);
              }}
            >
              <span>{c.label}</span>
              <span className="al-chip__clear" aria-hidden="true">
                {'\u2715'}
              </span>
              <span className="al-page__sr-only">{`Clear ${c.key} filter`}</span>
            </button>
          ))}
          <button type="button" className="al-chip al-chip--clearall" onClick={clearAllFilters}>
            <span>Clear all</span>
          </button>
        </div>
      )}

      {pending && data !== null && (
        <p className="al-filtering">
          <span className="al-filtering__dot" aria-hidden="true" />
          <span>Filtering…</span>
        </p>
      )}

      {showInitialSkeleton && (
        <div className="al-skeleton" role="status" aria-label="Loading audit log">
          {[0, 1, 2, 3, 4].map((i) => (
            <Skeleton key={i} variant="table-row" announceOnLoad={false} />
          ))}
        </div>
      )}

      {showError && (
        <EmptyState
          tone="info"
          title="Failed to load audit records"
          description="We couldn't load the audit trail. If this persists, contact your system administrator."
          icon={
            <EmptyIcon d="M12 9v4m0 4h.01M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z" />
          }
          actionLabel="Retry"
          onAction={() => setReloadKey((k) => k + 1)}
          role="alert"
        />
      )}

      {showEmpty && (
        <EmptyState
          tone="neutral"
          title="No audit activity in this range"
          description={
            chips.length > 0
              ? 'No events match your current filters. Adjust the filters to widen the range.'
              : 'Audit logging records all changes to your financial data. New activity will appear here.'
          }
          icon={
            <EmptyIcon d="M3 10h18M5 6h14M5 6a2 2 0 00-2 2v10a2 2 0 002 2h14a2 2 0 002-2V8a2 2 0 00-2-2M12 10v6m0 0l-2-2m2 2l2-2" />
          }
          actionLabel={chips.length > 0 ? 'Clear filters' : undefined}
          onAction={chips.length > 0 ? clearAllFilters : undefined}
          role="status"
        />
      )}

      {data !== null && !error && rows.length > 0 && (
        <>
          <div
            className="al-table-wrap"
            tabIndex={-1}
            ref={tableWrapRef}
            data-testid="audit-logs-table"
          >
            <table className="al-table">
              <caption className="al-caption">Audit log — read-only record</caption>
              <thead>
                <tr>
                  <th scope="col">Timestamp</th>
                  <th scope="col">Actor</th>
                  <th scope="col">Action</th>
                  <th scope="col">Entity</th>
                  <th scope="col">Entity ID</th>
                  <th scope="col">
                    <span className="al-page__sr-only">Details</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((entry, idx) => {
                  const open = expandedId === entry.id;
                  return (
                    <Fragment key={entry.id}>
                      <tr>
                        <td className="al-cell-time">{formatTimestamp(entry.timestamp)}</td>
                        <td>{entry.actorId}</td>
                        <td>
                          <ActionBadge action={entry.action} />
                        </td>
                        <td>{entry.entity}</td>
                        <td>{entry.entityId}</td>
                        <td>
                          <button
                            type="button"
                            className="al-row-button"
                            aria-expanded={open}
                            aria-controls={`al-diff-${entry.id}`}
                            aria-label={`Show details for ${entry.entity} ${entry.entityId}, row ${idx + 1}`}
                            onClick={() => setExpandedId(open ? null : entry.id)}
                          >
                            <span
                              className={`al-row-button__chev${open ? ' is-open' : ''}`}
                              aria-hidden="true"
                            >
                              {'\u203A'}
                            </span>
                          </button>
                        </td>
                      </tr>
                      {open && (
                        <tr className="al-diff-row">
                          <td colSpan={5}>
                            <div
                              className="al-diff"
                              id={`al-diff-${entry.id}`}
                              role="region"
                              aria-label={`Before and after values for ${entry.entity} ${entry.entityId}`}
                            >
                              <div className="al-diff-col">
                                <span className="al-diff-label">Before:</span>
                                <pre className="al-diff-pre">{diffText(entry.before)}</pre>
                              </div>
                              <div className="al-diff-col">
                                <span className="al-diff-label">After:</span>
                                <pre className="al-diff-pre">{diffText(entry.after)}</pre>
                              </div>
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>

          <Pagination
            offset={{ page, total: data.total, pageSize: data.page_size }}
            onChange={({ page: nextPage }) => {
              if (nextPage) {
                setPage(nextPage);
                setExpandedId(null);
              }
            }}
            loading={pending}
            focusTargetRef={tableWrapRef}
          />
        </>
      )}
    </main>
  );
}

export default AuditLogsPage;