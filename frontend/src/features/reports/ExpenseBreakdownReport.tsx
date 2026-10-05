/**
 * Expense breakdown report by category and period (Module E).
 *
 * Read-only. Renders expense totals per category against the selected range
 * (income excluded), a share-% bar visualization, and a total-expense footer.
 * Empty datasets render an empty summary; failed reads render an error with a
 * retry action. Never an error for an empty result.
 */
import { useEffect, useMemo, useState, type ReactElement } from 'react';
import { expensesByCategory } from '../../services/api/endpoints';
import type {
  CategoryTotal,
  ExpensesByCategorySummary,
  Money,
} from '../../services/api/types';
import { Skeleton } from '../../components/ui/Skeleton';
import { EmptyReportSummary } from './EmptyReportSummary';
import { addMoney, formatMinorUnits, parseMinorUnits } from '../../utils/currency';

export interface ExpenseBreakdownRange {
  start: string;
  end: string;
}

export interface ExpenseBreakdownReportProps {
  range: ExpenseBreakdownRange;
}

const LOAD_ERROR_MESSAGE = "Couldn't load expenses.";

const DEFAULT_LOCALE = 'en-US';
const DEFAULT_CURRENCY = 'USD';

/**
 * A validated row for the presentational table. `tenths` holds the category's
 * share expressed in 0.1% units so the displayed percentages (rounded via
 * largest remainder over the 0.1% grid) always sum to <= 100.
 */
interface BreakdownRow {
  categoryId: string;
  categoryName: string;
  amount: string;
  currency: string;
  tenths: number;
}

interface Breakdown {
  rows: BreakdownRow[];
  total: string;
  currency: string;
  /** Whether there is a positive total from which shares can be derived. */
  hasShare: boolean;
}

function isRangeValid(start: string, end: string): boolean {
  if (!start || !end) return false;
  const startTime = Date.parse(start);
  const endTime = Date.parse(end);
  if (Number.isNaN(startTime) || Number.isNaN(endTime)) return false;
  return startTime <= endTime;
}

/** Parses a minor-units decimal string, returning null instead of throwing. */
function parseAmount(amount: unknown): bigint | null {
  if (typeof amount !== 'string') return null;
  try {
    return parseMinorUnits(amount);
  } catch {
    return null;
  }
}

/**
 * Computes the per-row percentage as integer tenths of a percent using
 * largest-remainder rounding, so the summed displayed shares stay <= 100 even
 * after rounding. Malformed or non-expense rows are dropped at the boundary.
 */
function buildBreakdown(summary: ExpensesByCategorySummary): Breakdown {
  const currency = summary.totalExpenses.currency || DEFAULT_CURRENCY;
  const parsed: {
    item: CategoryTotal;
    categoryId: string;
    categoryName: string;
    minor: bigint;
  }[] = [];

  for (const item of summary.items) {
    const minor = parseAmount(item.total.amount);
    const categoryName = typeof item.categoryName === 'string' ? item.categoryName : '';
    if (minor === null || minor < 0n || !categoryName) continue;
    parsed.push({ item, categoryId: item.categoryId, categoryName, minor });
  }

  const sumMinor = parsed.reduce((acc, row) => acc + row.minor, 0n);
  const hasShare = sumMinor > 0n;

  let rows: BreakdownRow[] = parsed.map((row, index) => ({
    categoryId: row.categoryId,
    categoryName: row.categoryName,
    amount: row.item.total.amount,
    currency,
    tenths: hasShare ? Number((row.minor * 1000n) / sumMinor) : 0,
    minor: row.minor,
  }));

  if (hasShare) {
    const allotted = rows.reduce((acc, row) => acc + row.tenths, 0);
    const remainder = 1000 - allotted;
    // Distribute the leftover 0.1% units to the rows with the largest
    // fractional remainders, preserving a strict <= 100% total.
    const byFraction = rows
      .map((row, index) => ({
        row,
        index,
        fraction: Number((row.minor * 1000n) % sumMinor) / Number(sumMinor),
      }))
      .sort((a, b) => b.fraction - a.fraction || a.index - b.index);
    for (let i = 0; i < remainder; i += 1) {
      byFraction[i].row.tenths += 1;
    }
  }

  rows.sort((a, b) => {
    if (b.minor !== a.minor) return b.minor < a.minor ? -1 : 1;
    return a.categoryName.localeCompare(b.categoryName);
  });

  // Recompute the footer from the displayed rows so it always matches the
  // on-screen list.
  const total = rows
    .map((row) => ({ amount: row.amount, currency: row.currency }))
    .reduce(
      (acc, money) => addMoney(acc, money),
      { amount: '0', currency } satisfies Money,
    ).amount;

  return {
    rows: rows.map(({ minor: _minor, ...rest }) => rest),
    total,
    currency,
    hasShare,
  };
}

function useExpensesByCategory(range: ExpenseBreakdownRange): {
  state: 'loading' | 'error' | 'ready';
  summary: ExpensesByCategorySummary | null;
  retry: () => void;
} {
  const [attempt, setAttempt] = useState(0);
  const [summary, setSummary] = useState<ExpensesByCategorySummary | null>(null);
  const [state, setState] = useState<'loading' | 'error' | 'ready'>('loading');

  useEffect(() => {
    if (!isRangeValid(range.start, range.end)) {
      setSummary(null);
      setState('error');
      return;
    }

    const controller = new AbortController();
    setSummary(null);
    setState('loading');

    expensesByCategory({ start: range.start, end: range.end }, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        setSummary(result);
        setState('ready');
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        if (error instanceof DOMException && error.name === 'AbortError') return;
        setSummary(null);
        setState('error');
      });

    return () => controller.abort();
  }, [range.start, range.end, attempt]);

  const retry = () => setAttempt((n) => n + 1);

  return { state, summary, retry };
}

/**
 * Placeholder that mirrors the table geometry so the initial paint reserves
 * layout space and avoids CLS. Decorative only.
 */
function ExpenseBreakdownSkeleton(): ReactElement {
  return (
    <div aria-hidden="true">
      <Skeleton variant="text" width="35%" height={24} lines={1} />
      <Skeleton variant="table-row" lines={5} />
    </div>
  );
}

function ExpenseBreakdownError({ onRetry }: { onRetry: () => void }): ReactElement {
  return (
    <div role="alert" className="report-card__error">
      <p className="report-card__error-message">{LOAD_ERROR_MESSAGE}</p>
      <button type="button" className="report-card__retry" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

function ShareCell({ share, hasShare }: { share: number; hasShare: boolean }): ReactElement {
  // The bar is decorative reinforcement; the share % text in the adjacent cell
  // carries the information, so the bar is hidden from assistive tech. The bar
  // uses transform: scaleX, never animating width, to keep reflow at zero.
  const fillStyle: React.CSSProperties = {
    display: 'block',
    height: 8,
    borderRadius: 'var(--radius-full)',
    background: 'var(--color-expense-bar, #475569)',
    transform: `scaleX(${hasShare ? Math.min(Math.max(share, 0), 1) : 0})`,
    transformOrigin: 'left center',
    maxWidth: '100%',
  };
  const trackStyle: React.CSSProperties = {
    display: 'block',
    height: 8,
    borderRadius: 'var(--radius-full)',
    background: 'var(--color-expense-track, #F1F5F9)',
    width: '100%',
  };

  return (
    <td aria-hidden="true">
      <div role="presentation" style={trackStyle}>
        <span style={fillStyle} />
      </div>
    </td>
  );
}

function ExpenseBreakdownView({
  breakdown,
  summary,
  locale,
}: {
  breakdown: Breakdown;
  summary: ExpensesByCategorySummary;
  locale: string;
}): ReactElement {
  const totalAmount = formatMinorUnits(breakdown.total, breakdown.currency, locale);
  // Shares are tenths of a percent, not the server pct field, so `sum <= 100`
  // holds independently of the API. An empty share renders an em-dash.
  const percent = (tenths: number) => (tenths / 10).toFixed(1) + '%';

  return (
    <div className="report-table__scroll">
      <table className="report-table">
        <caption className="report-table__caption">
          Expenses by category from {summary.periodStart} to {summary.periodEnd}.
        </caption>
        <thead>
          <tr>
            <th scope="col">Category</th>
            <th scope="col">Share</th>
            <th scope="col" className="report-table__num">
              Share %
            </th>
            <th scope="col" className="report-table__num">
              Amount
            </th>
          </tr>
        </thead>
        <tbody>
          {breakdown.rows.map((row) => (
            <tr key={row.categoryId}>
              <th scope="row" className="report-table__period">
                {row.categoryName}
              </th>
              <ShareCell share={row.tenths / 1000} hasShare={breakdown.hasShare} />
              <td className="report-table__num">
                {breakdown.hasShare ? percent(row.tenths) : '\u2014'}
              </td>
              <td className="report-table__num">
                {formatMinorUnits(row.amount, row.currency, locale)}
              </td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr className="report-table__total">
            <th scope="row">Total expenses</th>
            <td aria-hidden="true" />
            <td aria-hidden="true" />
            <td className="report-table__num report-table__total-value">{totalAmount}</td>
          </tr>
        </tfoot>
      </table>
    </div>
  );
}

export function ExpenseBreakdownReport({
  range,
}: ExpenseBreakdownReportProps): ReactElement {
  const { state, summary, retry } = useExpensesByCategory(range);
  const breakdown = useMemo(() => (summary ? buildBreakdown(summary) : null), [summary]);

  return (
    <section className="report-card" aria-labelledby="expense-breakdown-title">
      <h2 id="expense-breakdown-title">Expense Breakdown</h2>
      <p className="report-card__subtitle">
        {range.start} – {range.end}
      </p>

      {state === 'loading' && <ExpenseBreakdownSkeleton />}
      {state === 'error' && <ExpenseBreakdownError onRetry={retry} />}
      {state === 'ready' && breakdown && breakdown.rows.length === 0 && (
        <EmptyReportSummary
          title="No expenses in this period"
          hint="Widen the date range or add expense transactions to see a breakdown."
        />
      )}
      {state === 'ready' && breakdown && breakdown.rows.length > 0 && (
        <ExpenseBreakdownView
          breakdown={breakdown}
          summary={summary!}
          locale={DEFAULT_LOCALE}
        />
      )}
    </section>
  );
}