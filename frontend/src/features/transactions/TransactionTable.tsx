import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Skeleton } from "../../components/ui/Skeleton";
import { formatMinorUnits } from "../../utils/currency";
import { formatDate } from "../../utils/date";
import { usePermission } from "../../hooks/usePermission";

/**
 * Windowed transaction table (Module B, Blueprint §8.1/§8.4).
 *
 * Renders a semantic, virtualized `<table>` that bounds the real DOM to a
 * visible slice of a potentially 100k-row dataset. The visible slice is the
 * single source of truth for the DOM; geometry (fixed row height) is the
 * windowing stride, so CLS stays ≤ 0.1 regardless of dataset size.
 *
 * This is a presentational component: it formats dates/currency via the
 * shared utils, gates Edit/Delete affordances behind `usePermission`, and
 * delegates mutation to `onEdit`/`onDelete`. All user/API-derived strings are
 * rendered through React text interpolation only — never `innerHTML` /
 * `dangerouslySetInnerHTML` (CWE-79). Sorting uses a pure comparator against
 * a fixed, allowlisted set of keys, so no `eval`/dynamic key access on
 * untrusted input exists.
 */

/** One display row as consumed by the table (mirrors the API DTO shape). */
export interface TransactionRow {
  id: string;
  /** ISO-8601 UTC calendar date (YYYY-MM-DD). */
  date: string;
  /** Free-text description, ≤ 255 chars, API-guarded. Rendered as plain text only. */
  description: string;
  /** Display name of the category. */
  category: string;
  /** Display name of the account. */
  account: string;
  /** Non-negative minor-unit (two-decimal) decimal string. Direction is carried by `type`. */
  amount: string;
  /** ISO-4217 currency code of the mounted account. */
  currency: string;
  type: "INCOME" | "EXPENSE";
  /** True when the entry is a future-dated scheduled transaction. */
  scheduled: boolean;
}

export type SortKey = "date" | "description" | "category" | "account" | "amount";
export type SortDirection = "ascending" | "descending" | "none";

export interface TransactionTableProps {
  /** Full dataset; the table renders only a bounded visible slice. */
  items: TransactionRow[];
  /** Fires with the clicked sort key when a sortable header is toggled. */
  onSort?: (key: SortKey) => void;
  onEdit?: (id: string) => void;
  onDelete?: (id: string) => void;
  /** Renders skeleton rows while true. */
  loading?: boolean;
  /** When set, renders a solution-oriented error row with a Retry control. */
  error?: string | null;
  onRetry?: () => void;
  /** Custom empty state; falls back to a friendly guiding message. */
  emptySlot?: React.ReactNode;
  /** Formatting locale for dates/currency. */
  locale?: string;
  sortKey?: SortKey;
  sortDirection?: SortDirection;
  className?: string;
}

const SORT_LABEL: Record<SortKey, string> = {
  date: "Date",
  description: "Description",
  category: "Category",
  account: "Account",
  amount: "Amount",
};

/** Fixed row height in px — the windowing stride AND the CLS guarantee. */
const ROW_HEIGHT = 48;
/** Max real `<tr>` nodes mounted per scroll position (Miller/Hick bound). */
const WINDOW_SIZE = 20;
/** Debounce window for IntersectionObserver-driven window recompute. */
const OBSERVER_DEBOUNCE_MS = 150;

// Single, deduplicated style block. Static string with zero user interpolation,
// so a strict CSP needs no inline `style-src 'unsafe-inline'`. Sign coloring
// and the Scheduled badge use design tokens (not data-derived inline styles).
let csvStyleInjected = false;
const TX_TABLE_STYLE_ID = "fms-transaction-table-styles";
const TX_TABLE_CSS = `
:root {
  --table-row-height: ${ROW_HEIGHT}px;
  --tx-surface: #ffffff;
  --tx-surface-muted: #f9fafb;
  --tx-surface-header: #f3f4f6;
  --tx-border-subtle: #e5e7eb;
  --tx-text-primary: #111827;
  --tx-text-secondary: #4b5563;
  --tx-text-muted: #6b7280;
  --tx-income: #15803d;
  --tx-expense: #dc2626;
  --tx-scheduled: #7c3aed;
  --tx-focus-ring: #2563eb;
}
.fms-txtable{border-collapse:collapse;width:100%;max-width:1280px;}
.fms-txtable__scroll{overflow-x:auto;}
.fms-txtable__viewport{overflow-y:auto;max-height:calc(var(--table-row-height) * 24);}
.fms-txtable__view{min-width:640px;width:100%;}
.fms-txtable__caption{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);clip-path:inset(50%);white-space:nowrap;border:0;}
.fms-txtable__head{position:sticky;top:0;z-index:1;background:var(--tx-surface-header);box-shadow:0 1px 2px rgba(0,0,0,0.05);}
.fms-txtable__head th{
  padding:0;background:var(--tx-surface-header);text-align:left;
  vertical-align:middle;font-weight:500;font-size:14px;color:var(--tx-text-secondary);
  border-bottom:1px solid var(--tx-border-subtle);
}
.fms-txtable__head th.fms-txtable__th--amount,.fms-txtable__cell--amount{text-align:right;}
.fms-txtable__head th.fms-txtable__th--actions{text-align:right;border-bottom:none;}
.fms-sort-btn{
  appearance:none;border:0;background:transparent;cursor:pointer;
  display:inline-flex;align-items:center;justify-content:flex-start;gap:4px;
  min-height:44px;min-width:44px;padding:0 8px;width:100%;box-sizing:border-box;
  font:inherit;font-weight:500;color:inherit;border-radius:var(--radius-sm,4px);
}
.fms-sort-btn:hover{background:var(--tx-surface-muted);}
.fms-sort-btn:focus-visible{outline:2px solid var(--tx-focus-ring);outline-offset:2px;}
.fms-sort-btn .fms-sort-icon{font-size:12px;line-height:1;color:var(--tx-text-muted);opacity:0.5;}
.fms-sort-btn.is-active .fms-sort-icon{opacity:1;color:var(--tx-text-primary);transition:opacity 150ms ease-out;}
.fms-sort-icon--desc{transform:rotate(180deg);transition:transform 150ms ease-out;}
.fms-txtable tbody tr.fms-tx-row{height:var(--table-row-height);}
.fms-txtable tbody tr.fms-tx-row:focus-visible{outline:2px solid var(--tx-focus-ring);outline-offset:-2px;}
.fms-txtable tbody tr.fms-tx-row:hover{background:var(--tx-surface-muted);transition:background-color 150ms ease-out;}
.fms-txtable tbody tr.fms-tx-row.is-focused{background:var(--tx-surface-muted);}
.fms-txtable__spacer{background:transparent;padding:0;}
.fms-txtable__cell{padding:0;vertical-align:middle;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;padding-left:16px;padding-right:16px;border-bottom:1px solid var(--tx-border-subtle);}
.fms-txtable__cell--date{width:64px;font-size:14px;color:var(--tx-text-secondary);font-variant-numeric:tabular-nums;}
.fms-txtable__cell--desc{min-width:180px;font-size:16px;font-weight:500;color:var(--tx-text-primary);}
.fms-txtable__cell--cat,.fms-txtable__cell--acc{width:120px;font-size:14px;color:var(--tx-text-secondary);}
.fms-txtable__cell--amount{width:120px;font-size:16px;font-weight:600;font-variant-numeric:tabular-nums;white-space:nowrap;}
.fms-txtable__actions{width:64px;text-align:right;white-space:nowrap;}
.fms-txtable__actions:empty{display:none;}
.fms-tx-action{
  appearance:none;border:0;background:transparent;cursor:pointer;
  min-width:44px;min-height:44px;width:44px;height:44px;box-sizing:border-box;
  color:var(--tx-text-muted);border-radius:var(--radius-sm,4px);
}
.fms-tx-action:hover{color:var(--tx-text-primary);background:var(--tx-surface-muted);}
.fms-tx-action:focus-visible{outline:2px solid var(--tx-focus-ring);outline-offset:2px;}
.fms-scheduled-badge{
  display:inline-flex;align-items:center;gap:6px;
  background:rgba(124,58,237,0.08);color:var(--tx-scheduled);
  border-radius:var(--radius-full,9999px);padding:2px 8px;
  font-size:12px;font-weight:500;white-space:nowrap;margin-left:8px;vertical-align:middle;
}
.fms-scheduled-badge svg{flex:none;}
.fms-amount--income{color:var(--tx-income);}
.fms-amount--expense{color:var(--tx-expense);}
.fms-txtable__state{text-align:center;padding:48px 24px;color:var(--tx-text-muted);}
.fms-txtable__state-title{display:block;font-size:16px;font-weight:500;color:var(--tx-text-primary);margin-bottom:8px;}
.fms-txtable__retry,.fms-txtable__cta{
  appearance:none;border:0;cursor:pointer;border-radius:var(--radius-sm,4px);
  min-height:44px;min-width:44px;padding:0 16px;margin-top:12px;
  background:var(--tx-focus-ring);color:#ffffff;font-size:14px;font-weight:500;
}
.fms-txtable__retry:focus-visible,.fms-txtable__cta:focus-visible{outline:2px solid var(--tx-focus-ring);outline-offset:2px;}
.fms-txtable__skeleton-row{height:var(--table-row-height);padding:0;}
@media (max-width:1023px){
  .fms-txtable__cell{padding-left:12px;padding-right:12px;}
}
@media (prefers-reduced-motion:reduce){
  .fms-sort-icon--desc,.fms-sort-btn .fms-sort-icon,.fms-txtable tbody tr.fms-tx-row{transition:none;}
}
`;

function injectCsvStyle(): void {
  if (typeof document !== "undefined" && !csvStyleInjected) {
    csvStyleInjected = true;
    const existing = document.getElementById(TX_TABLE_STYLE_ID);
    if (!existing) {
      const style = document.createElement("style");
      style.id = TX_TABLE_STYLE_ID;
      style.textContent = TX_TABLE_CSS;
      document.head.appendChild(style);
    }
  }
}

/**
 * Pure comparator over the fixed, allowlisted sort keys. Operates on plain
 * strings / BigInt only and never touches user-controlled property names,
 * so no `eval`, `new Function`, or prototype-polluting dynamic key access.
 */
function compareRows(
  a: TransactionRow,
  b: TransactionRow,
  key: SortKey,
  direction: "ascending" | "descending",
): number {
  let cmp: number;
  switch (key) {
    case "date":
      // ISO YYYY-MM-DD sorts lexicographically as chronological.
      cmp = a.date < b.date ? -1 : a.date > b.date ? 1 : 0;
      break;
    case "amount": {
      const av = parseMinorAbs(a.amount);
      const bv = parseMinorAbs(b.amount);
      cmp = av < bv ? -1 : av > bv ? 1 : 0;
      break;
    }
    case "description":
    case "category":
    case "account":
      cmp = a[key].localeCompare(b[key]);
      break;
    default:
      cmp = 0;
  }
  return direction === "descending" ? -cmp : cmp;
}

/** Absolute minor units for sorting — amount is non-negative per domain invariant. */
function parseMinorAbs(amount: string): bigint {
  const cleaned = amount.startsWith("-") ? amount.slice(1) : amount;
  const [whole, frac = ""] = cleaned.split(".");
  try {
    const wholeBig = BigInt(whole || "0");
    const fracBig = frac.length === 1 ? BigInt(`${frac}0`) : BigInt(frac || "0");
    return wholeBig * 100n + fracBig;
  } catch {
    return 0n;
  }
}

/** Sign glyph derived from the transaction type — never from color alone. */
function amountSign(type: TransactionRow["type"]): string {
  return type === "INCOME" ? "+" : "\u2212";
}

function ArrowIcon(): JSX.Element {
  return <span aria-hidden="true" className="fms-sort-icon">▲</span>;
}

function ClockIcon(): JSX.Element {
  return (
    <svg
      aria-hidden="true"
      width="14"
      height="14"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 2" />
    </svg>
  );
}

function EditIcon(): JSX.Element {
  return (
    <svg
      aria-hidden="true"
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M12 20h9" />
      <path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z" />
    </svg>
  );
}

function DeleteIcon(): JSX.Element {
  return (
    <svg
      aria-hidden="true"
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M3 6h18" />
      <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" />
      <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
    </svg>
  );
}

/**
 * Renders an accessible, windowed transaction table row (presentational).
 */
function TransactionTableRow({
  row,
  locale,
  offset,
  isFocused,
  canMutate,
  onEdit,
  onDelete,
}: {
  row: TransactionRow;
  locale: string;
  /** Zero-based offset within the currently windowed slice (keyboard nav). */
  offset: number;
  isFocused: boolean;
  canMutate: boolean;
  onEdit?: (id: string) => void;
  onDelete?: (id: string) => void;
}): JSX.Element {
  const formatted = formatMinorUnits(row.amount, row.currency, locale);
  const signedAmount = `${amountSign(row.type)}${formatted}`;
  const dateText = formatDate(row.date, locale);

  return (
    <tr
      className={`fms-tx-row${isFocused ? " is-focused" : ""}`}
      tabIndex={0}
      aria-rowindex={offset + 1}
      data-row-offset={offset}
      title={row.description}
    >
      <td className="fms-txtable__cell fms-txtable__cell--date">{dateText}</td>
      <td
        className="fms-txtable__cell fms-txtable__cell--desc"
        aria-label={row.description}
      >
        {row.description}
        {row.scheduled && (
          <span className="fms-scheduled-badge">
            <ClockIcon />
            Scheduled
          </span>
        )}
      </td>
      <td className="fms-txtable__cell fms-txtable__cell--cat">{row.category}</td>
      <td className="fms-txtable__cell fms-txtable__cell--acc">{row.account}</td>
      <td
        className={`fms-txtable__cell fms-txtable__cell--amount ${
          row.type === "INCOME" ? "fms-amount--income" : "fms-amount--expense"
        }`}
        aria-label={`${row.type === "INCOME" ? "Income" : "Expense"} ${formatted}`}
      >
        {signedAmount}
      </td>
      {canMutate && (
        <td className="fms-txtable__actions">
          {onEdit && (
            <button
              type="button"
              className="fms-tx-action"
              aria-label="Edit transaction"
              onClick={() => onEdit(row.id)}
            >
              <EditIcon />
            </button>
          )}
          {onDelete && (
            <button
              type="button"
              className="fms-tx-action"
              aria-label="Delete transaction"
              onClick={() => onDelete(row.id)}
            >
              <DeleteIcon />
            </button>
          )}
        </td>
      )}
    </tr>
  );
}

/**
 * Windowed transaction table. Presentational; bounds the real DOM to a
 * visible slice of `items` so a 100k-row dataset stays fluid and CLS-safe.
 */
export function TransactionTable({
  items,
  onSort,
  onEdit,
  onDelete,
  loading = false,
  error = null,
  onRetry,
  emptySlot,
  locale = "en-US",
  sortKey,
  sortDirection = "none",
  className = "",
}: TransactionTableProps): JSX.Element {
  injectCsvStyle();

  const { can } = usePermission();
  // Fail-closed on unresolved role; edit/delete affordances are hidden, not
  // disabled, for view-only / unauthenticated sessions (least privilege).
  const canMutate = can("transaction:write");

  const scrollRef = useRef<HTMLDivElement | null>(null);
  const sentinelRef = useRef<HTMLTableRowElement | null>(null);
  const debounceRef = useRef<number | undefined>(undefined);
  const [windowStart, setWindowStart] = useState(0);
  const [focusedOffset, setFocusedOffset] = useState<number | null>(null);

  const total = items.length;
  const windowEnd = Math.min(windowStart + WINDOW_SIZE, total);
  const visible = useMemo(
    () => items.slice(windowStart, windowEnd),
    [items, windowStart, windowEnd],
  );

  // Reset the window whenever the dataset identity changes so the user always
  // lands at the top of a newly loaded / refetched slice.
  useEffect(() => {
    setWindowStart(0);
    setFocusedOffset(null);
    if (scrollRef.current) scrollRef.current.scrollTop = 0;
  }, [items]);

  useEffect(() => {
    const sentinel = sentinelRef.current;
    const scroll = scrollRef.current;
    if (!sentinel || !scroll || loading || items.length === 0) return;

    const io = new IntersectionObserver(
      (entries) => {
        const isIntersecting = entries.some((e) => e.isIntersecting);
        window.clearTimeout(debounceRef.current);
        debounceRef.current = window.setTimeout(() => {
          setWindowStart((start) => {
            if (isIntersecting) {
              return Math.min(start + WINDOW_SIZE, Math.max(total - WINDOW_SIZE, 0));
            }
            const top = scroll.scrollTop;
            const next = Math.floor(top / ROW_HEIGHT);
            return Math.min(next, Math.max(total - WINDOW_SIZE, 0));
          });
        }, OBSERVER_DEBOUNCE_MS);
      },
      { root: scroll },
    );
    io.observe(sentinel);
    return () => {
      window.clearTimeout(debounceRef.current);
      io.disconnect();
    };
  }, [loading, total, items]);

  const handleSort = useCallback(
    (key: SortKey) => {
      onSort?.(key);
    },
    [onSort],
  );

  const moveFocus = useCallback(
    (delta: 1 | -1, toBoundary: boolean) => {
      if (!scrollRef.current || visible.length === 0) return;
      const rows = Array.from(
        scrollRef.current.querySelectorAll<HTMLElement>("tr.fms-tx-row"),
      );
      const current = focusedOffset ?? 0;
      const target =
        toBoundary
          ? delta === 1
            ? visible.length - 1
            : 0
          : Math.min(Math.max(current + delta, 0), visible.length - 1);
      const targetRow = rows[target];
      if (targetRow) {
        targetRow.focus({ preventScroll: true });
        setFocusedOffset(target);
      }
    },
    [focusedOffset, visible.length],
  );

  const handleBodyKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLTableSectionElement>) => {
      const active = document.activeElement as HTMLElement | null;
      const row = active && active.closest("tr.fms-tx-row");
      const offsetAttr = row?.getAttribute("data-row-offset");
      const offset = offsetAttr === null ? -1 : Number(offsetAttr);
      if (offset < 0) return;

      switch (event.key) {
        case "ArrowDown":
          event.preventDefault();
          moveFocus(1, false);
          break;
        case "ArrowUp":
          event.preventDefault();
          moveFocus(-1, false);
          break;
        case "Home":
          event.preventDefault();
          moveFocus(-1, true);
          break;
        case "End":
          event.preventDefault();
          moveFocus(1, true);
          break;
        case "Enter": {
          // Primary action: open the focused row for editing when permitted.
          if (canMutate && onEdit) {
            event.preventDefault();
            const row = visible[offset];
            if (row) onEdit(row.id);
          }
          break;
        }
        default:
          break;
      }
    },
    [moveFocus, canMutate, onEdit, visible],
  );

  const renderHeader = (): JSX.Element => {
    const sortableKeys: SortKey[] = [
      "date",
      "description",
      "category",
      "account",
      "amount",
    ];
    return (
      <thead className="fms-txtable__head">
        <tr>
          {sortableKeys.map((key) => {
            const active = sortKey === key && sortDirection !== "none";
            const toolbarAriaSort = active ? sortDirection : "none";
            const label = SORT_LABEL[key];
            const buttonLabel =
              active && sortDirection === "ascending"
                ? `Sort by ${label} ascending`
                : active && sortDirection === "descending"
                  ? `Sort by ${label} descending`
                  : `Sort by ${label}`;
            return (
              <th scope="col" key={key} aria-sort={toolbarAriaSort}>
                <button
                  type="button"
                  className={`fms-sort-btn${active ? " is-active" : ""}`}
                  aria-label={buttonLabel}
                  onClick={() => handleSort(key)}
                >
                  {label}
                  <span
                    className={
                      active && sortDirection === "descending"
                        ? "fms-sort-icon fms-sort-icon--desc"
                        : "fms-sort-icon"
                    }
                  >
                    <ArrowIcon />
                  </span>
                </button>
              </th>
            );
          })}
          {canMutate && (
            <th scope="col" aria-sort="none" className="fms-txtable__th--actions">
              <span className="fms-txtable__caption">Actions</span>
            </th>
          )}
        </tr>
      </thead>
    );
  };

  const renderBody = (): JSX.Element => {
    const colSpan = canMutate ? 6 : 5;

    if (loading) {
      return (
        <tbody aria-busy="true" aria-live="polite">
          {Array.from({ length: 5 }).map((_, i) => (
            <tr key={i} className="fms-txtable__skeleton-row" aria-hidden="true">
              <td colSpan={colSpan} className="fms-txtable__cell fms-txtable__skeleton-row">
                <Skeleton
                  variant="table-row"
                  announceOnLoad={i === 0}
                  ariaLabel="Loading transactions"
                />
              </td>
            </tr>
          ))}
        </tbody>
      );
    }

    if (error) {
      return (
        <tbody>
          <tr>
            <td colSpan={colSpan} className="fms-txtable__state">
              <span className="fms-txtable__state-title">
                Unable to load transactions
              </span>
              {error}
              {onRetry && (
                <button
                  type="button"
                  className="fms-txtable__retry"
                  onClick={onRetry}
                >
                  Retry
                </button>
              )}
            </td>
          </tr>
        </tbody>
      );
    }

    if (total === 0) {
      return (
        <tbody>
          <tr>
            <td colSpan={colSpan} className="fms-txtable__state">
              {emptySlot ?? (
                <>
                  <span className="fms-txtable__state-title">
                    All caught up!
                  </span>
                  Add your first transaction to see it here.
                </>
              )}
            </td>
          </tr>
        </tbody>
      );
    }

    const topSpacerHeight = windowStart * ROW_HEIGHT;
    const bottomSpacerHeight = Math.max(total - windowEnd, 0) * ROW_HEIGHT;

    return (
      <tbody onKeyDown={handleBodyKeyDown}>
        <tr className="fms-txtable__spacer" aria-hidden="true">
          <td colSpan={colSpan} style={{ height: topSpacerHeight }} />
        </tr>
        {visible.map((row, i) => (
          <TransactionTableRow
            key={row.id}
            row={row}
            offset={i}
            isFocused={focusedOffset === i}
            locale={locale}
            canMutate={canMutate}
            onEdit={onEdit}
            onDelete={onDelete}
          />
        ))}
        <tr ref={sentinelRef} className="fms-txtable__spacer" aria-hidden="true">
          <td colSpan={colSpan} style={{ height: bottomSpacerHeight }} />
        </tr>
      </tbody>
    );
  };

  return (
    <div
      className={`fms-txtable__scroll${className ? ` ${className}` : ""}`}
      role="region"
      aria-label="Transaction history"
      aria-busy={loading}
    >
      <div className="fms-txtable__viewport" ref={scrollRef}>
        <table
          role="grid"
          aria-rowcount={total}
          className="fms-txtable fms-txtable__view"
        >
          <caption className="fms-txtable__caption">
            Transaction history table. Use column headers to sort.
          </caption>
          {renderHeader()}
          {renderBody()}
        </table>
      </div>
    </div>
  );
}

export default TransactionTable;