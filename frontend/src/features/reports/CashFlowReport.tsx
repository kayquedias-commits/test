/**
 * Cash-flow statement view (Module E).
 *
 * Read-only report that fetches the period cash-flow summary and renders
 * per-period inflows/outflows/net movement with a running balance, section
 * totals, an emphasized overall net, and the shared empty/loading/error
 * states. All monetary values are formatted and arithmetically combined
 * through currency.ts (BigInt-backed) — never binary floats.
 */
import { useEffect, useMemo, useState } from 'react';
import type { ReactElement } from 'react';
import { cashFlow } from '../../services/api/endpoints';
import type { CashFlowSummary, CashFlowLine } from '../../services/api/types';
import { Skeleton } from '../../components/ui/Skeleton';
import EmptyReportSummary from './EmptyReportSummary';
import {
  formatMinorUnits,
  addMoney,
  subtractMoney,
} from '../../utils/currency';

export interface CashFlowRange {
  start: string;
  end: string;
}

export interface CashFlowReportProps {
  range: CashFlowRange;
}

interface PeriodRow {
  period: string;
  inflows: string;
  outflows: string;
  net: string;
  runningBalance: string;
}

interface CashFlowTotals {
  totalInflows: string;
  totalOutflows: string;
  netTotal: string;
  closingBalance: string;
}

/** Keep the period table scannable (Miller's Law); longer series scrolls. */
const MAX_ROWS = 9;
const LOAD_ERROR_MESSAGE = "Couldn't load cash flow.";

/** ISO date strings compare correctly; guard against non-dates. */
function isRangeValid(start: string, end: string): boolean {
  if (!start || !end || Number.isNaN(Date.parse(start)) || Number.isNaN(Date.parse(end))) {
    return false;
  }
  return start.localeCompare(end) <= 0;
}

/** Folds a list of amounts with the sign-aware BigInt addition helper. */
function sumAmounts(items: CashFlowLine[], selector: (line: CashFlowLine) => string): string {
  return items.reduce<string>((acc, line) => addMoney(acc, selector(line)), '0');
}

/**
 * Groups the flat transaction lines into chronological per-period rows,
 * computing each period's inflows/outflows/net and a running balance that
 * starts from the opening balance. Returns the rows plus aggregate totals.
 */
export function buildPeriodRows(
  items: CashFlowLine[],
  openingBalance: string,
): { rows: PeriodRow[]; totals: CashFlowTotals } {
  const totalInflows = sumAmounts(items, (l) => (l.type === 'INCOME' ? l.amount : '0'));
  const totalOutflows = sumAmounts(items, (l) => (l.type === 'EXPENSE' ? l.amount : '0'));
  const netTotal = subtractMoney(totalInflows, totalOutflows);
  const closingBalance = addMoney(openingBalance, netTotal);

  const byPeriod = new Map<string, CashFlowLine[]>();
  for (const line of items) {
    const period = line.date.slice(0, 7);
    const group = byPeriod.get(period);
    if (group) {
      group.push(line);
    } else {
      byPeriod.set(period, [line]);
    }
  }

  const periods = [...byPeriod.keys()].sort((a, b) => a.localeCompare(b));
  const rows: PeriodRow[] = [];
  let runningBalance = openingBalance;
  for (const period of periods) {
    const periodItems = byPeriod.get(period)!;
    const inflows = sumAmounts(periodItems, (l) => (l.type === 'INCOME' ? l.amount : '0'));
    const outflows = sumAmounts(periodItems, (l) => (l.type === 'EXPENSE' ? l.amount : '0'));
    const net = subtractMoney(inflows, outflows);
    runningBalance = addMoney(runningBalance, net);
    rows.push({ period, inflows, outflows, net, runningBalance });
  }

  return {
    rows,
    totals: { totalInflows, totalOutflows, netTotal, closingBalance },
  };
}

type ReportStatus = 'loading' | 'error' | 'ready';

interface CashFlowState {
  summary: CashFlowSummary | null;
  status: ReportStatus;
}

/** Loads the cash-flow summary for the given range; increments on retry. */
function useCashFlow(range: CashFlowRange): {
  state: CashFlowState;
  retry: () => void;
} {
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<CashFlowState>({ summary: null, status: 'loading' });

  useEffect(() => {
    if (!isRangeValid(range.start, range.end)) {
      setState({ summary: null, status: 'error' });
      return;
    }

    const controller = new AbortController();
    setState({ summary: null, status: 'loading' });
    cashFlow({ start: range.start, end: range.end }, controller.signal)
      .then((summary) => setState({ summary, status: 'ready' }))
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') {
          return;
        }
        setState({ summary: null, status: 'error' });
      });

    return () => controller.abort();
  }, [range.start, range.end, attempt]);

  return { state, retry: () => setAttempt((n) => n + 1) };
}

function CashFlowSkeleton(): ReactElement {
  return (
    <div aria-hidden="true">
      <Skeleton variant="text" width="35%" height={24} />
      <Skeleton variant="table-row" lines={5} />
    </div>
  );
}

function CashFlowError({ onRetry }: { onRetry: () => void }): ReactElement {
  return (
    <div role="alert" className="report-card__error">
      <p className="report-card__error-message">{LOAD_ERROR_MESSAGE}</p>
      <button type="button" className="report-card__retry" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

interface CashFlowViewProps {
  range: CashFlowRange;
  rows: PeriodRow[];
  totals: CashFlowTotals;
  currency: string;
  locale: string;
}

function formatSigned(amount: string, currency: string, locale: string, sign = '+'): string {
  return `${sign}${formatMinorUnits(amount, currency, locale)}`;
}

function CashFlowView({
  range,
  rows,
  totals,
  currency,
  locale,
}: CashFlowViewProps): ReactElement {
  const visibleRows = rows.slice(0, MAX_ROWS);

  return (
    <div className="report-table__scroll">
      <table className="report-table">
        <caption className="report-table__caption">
          Cash-flow activity from {range.start} to {range.end}. The running
          balance reflects the opening balance plus net movement in each period.
        </caption>
        <thead>
          <tr>
            <th scope="col">Period</th>
            <th scope="col" className="report-table__num">Inflows</th>
            <th scope="col" className="report-table__num">Outflows</th>
            <th scope="col" className="report-table__num">Net</th>
            <th scope="col" className="report-table__num">Running Balance</th>
          </tr>
        </thead>
        <tbody>
          {visibleRows.map((row) => (
            <tr key={row.period}>
              <th scope="row" className="report-table__period">{row.period}</th>
              <td className="report-table__num">{formatSigned(row.inflows, currency, locale)}</td>
              <td className="report-table__num">{formatSigned(row.outflows, currency, locale, '−')}</td>
              <td className="report-table__num">{formatSigned(row.net, currency, locale)}</td>
              <td className="report-table__num report-table__context">
                {formatSigned(row.runningBalance, currency, locale)}
              </td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr className="report-table__total">
            <th scope="row">Total</th>
            <td className="report-table__num report-table__total-value">
              {formatSigned(totals.totalInflows, currency, locale)}
            </td>
            <td className="report-table__num report-table__total-value">
              {formatSigned(totals.totalOutflows, currency, locale, '−')}
            </td>
            <td className="report-table__num report-table__net">
              {formatSigned(totals.netTotal, currency, locale)}
            </td>
            <td className="report-table__num report-table__context">
              {formatSigned(totals.closingBalance, currency, locale)}
            </td>
          </tr>
        </tfoot>
      </table>
    </div>
  );
}

/** Cash-flow report by period with running balance and emphasized net. */
export function CashFlowReport({ range }: CashFlowReportProps): ReactElement {
  const { state, retry } = useCashFlow(range);

  const { rows, totals } = useMemo(
    () =>
      state.summary && state.summary.items.length > 0
        ? buildPeriodRows(state.summary.items, state.summary.openingBalance.amount)
        : { rows: [] as PeriodRow[], totals: null as CashFlowTotals | null },
    [state.summary],
  );

  return (
    <section className="report-card" aria-labelledby="cash-flow-title">
      <h2 id="cash-flow-title">Cash Flow</h2>
      <p className="report-card__subtitle">
        {range.start} – {range.end}
      </p>

      {state.status === 'loading' && <CashFlowSkeleton />}

      {state.status === 'error' && <CashFlowError onRetry={retry} />}

      {state.status === 'ready' && state.summary && state.summary.items.length === 0 && (
        <EmptyReportSummary
          title="No cash-flow activity in this period"
          hint="Widen the date range or add transactions to see cash-flow activity."
        />
      )}

      {state.status === 'ready' &&
        state.summary &&
        state.summary.items.length > 0 &&
        rows.length > 0 &&
        totals && (
          <CashFlowView
            range={range}
            rows={rows}
            totals={totals}
            currency={state.summary.openingBalance.currency}
            locale="en-US"
          />
        )}
    </section>
  );
}