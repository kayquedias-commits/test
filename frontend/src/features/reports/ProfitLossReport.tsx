/**
 * Read-only profit/loss report view (Module E). Fetches the P/L summary over a
 * date `range` and renders income, expenses, net, and per-category breakdowns.
 * Empty data yields an empty summary (never an error); all amounts flow through
 * `currency.ts` and dates through `date.ts` for consistent formatting. This view
 * renders only text nodes and never sets innerHTML.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ReactElement } from 'react';
import Skeleton from '../../components/ui/Skeleton';
import EmptyReportSummary from './EmptyReportSummary';
import { profitLoss } from '../../services/api/endpoints';
import type { ProfitLossSummary, ProfitLossLine, Money } from '../../services/api/types';
import { formatMinorUnits, parseMinorUnits } from '../../utils/currency';
import { formatDate, isRangeValid } from '../../utils/date';

/** Report period used to query the P/L endpoint (start <= end enforced). */
export interface Range {
  start: string;
  end: string;
}

export interface ProfitLossReportProps {
  /** User-selected, validated date range over which the report is computed. */
  range: Range;
}

const LOCALE = 'en-US';
/** Maximum displayed category rows per section; remainder aggregates into "Other". */
const TOP_N = 6;

const NET_LABEL = { positive: 'Profit', negative: 'Loss', zero: 'Break-even' } as const;
const NET_GLYPH = { positive: '↑', negative: '↓', zero: '=' } as const;

interface ProfitLossState {
  status: 'loading' | 'success' | 'error';
  data: ProfitLossSummary | null;
}

/** True when `money` carries a parseable finite amount (defensive render guard). */
function isFiniteMoney(money: Money | null | undefined): boolean {
  if (!money || typeof money.amount !== 'string') return false;
  try {
    parseMinorUnits(money.amount);
    return true;
  } catch {
    return false;
  }
}

/**
 * Returns the top `max` lines by amount, aggregating any remainder into an
 * "Other" line so a section never exceeds working memory (Miller's Law, N<=7).
 * Rows with a missing name or a non-finite amount are skipped defensively.
 */
function topN(lines: ProfitLossLine[], max: number): ProfitLossLine[] {
  const valid: ProfitLossLine[] = [];
  for (const line of lines) {
    if (!line.categoryName || !isFiniteMoney(line.total)) continue;
    valid.push(line);
  }
  if (valid.length === 0) return [];
  // BigInt comparison keeps ordering exact (no float drift on large amounts).
  const sorted = [...valid].sort((a, b) => {
    try {
      return Number(parseMinorUnits(b.total.amount) - parseMinorUnits(a.total.amount));
    } catch {
      return 0;
    }
  });
  const top = sorted.slice(0, max);
  if (sorted.length <= max) return top;
  const restCurrency = top[0].total.currency;
  let restUnits = 0n;
  for (const line of sorted.slice(max)) {
    try {
      restUnits += parseMinorUnits(line.total.amount);
    } catch {
      /* row already validated; defensive against malformed backend data */
    }
  }
  const sign = restUnits < 0n ? '-' : '';
  const abs = restUnits < 0n ? -restUnits : restUnits;
  const whole = abs / 100n;
  const frac = (abs % 100n).toString().padStart(2, '0');
  top.push({
    categoryId: null,
    categoryName: 'Other',
    type: top[0].type,
    total: { amount: `${sign}${whole.toString()}.${frac}`, currency: restCurrency },
  });
  return top;
}

/** Largest finite amount (in minor units) across `lines`, used to scale bars. */
function sectionMaxUnits(lines: ProfitLossLine[]): bigint {
  let max = 0n;
  for (const line of lines) {
    try {
      const units = parseMinorUnits(line.total.amount);
      if (units > max) max = units;
    } catch {
      /* skip malformed row */
    }
  }
  return max;
}

/** Clamped [0, 1] bar fill ratio so untrusted amounts can never overflow. */
function barRatio(amount: string, maxUnits: bigint): number {
  if (maxUnits <= 0n) return 0;
  try {
    const units = parseMinorUnits(amount);
    if (units <= 0n) return 0;
    return Math.max(0, Math.min(1, Number((units * 100n) / maxUnits) / 100));
  } catch {
    return 0;
  }
}

/** 'positive' | 'negative' | 'zero' from the parsed net amount; net=0 stays neutral. */
function netSign(net: Money | null | undefined): 'positive' | 'negative' | 'zero' {
  if (!isFiniteMoney(net)) return 'zero';
  try {
    const units = parseMinorUnits(net!.amount);
    if (units === 0n) return 'zero';
    return units < 0n ? 'negative' : 'positive';
  } catch {
    return 'zero';
  }
}

/**
 * Local fetch hook for the P/L summary. Validates `range` (never requests an
 * invalid start>end range), aborts stale in-flight requests on range change /
 * unmount, and exposes a manual refetch for the error-state Retry control.
 */
function useProfitLoss(range: Range): { state: ProfitLossState; refetch: () => void } {
  const [state, setState] = useState<ProfitLossState>({ status: 'loading', data: null });
  const [tick, setTick] = useState(0);

  const valid = useMemo(
    () => isRangeValid(range.start, range.end),
    [range.start, range.end],
  );

  useEffect(() => {
    if (!valid) {
      // Invalid range: produce an empty summary without firing a request and
      // never send malformed start/end params to the gateway.
      setState({ status: 'success', data: null });
      return;
    }
    setState((prev) => ({ status: 'loading', data: null }));
    const controller = new AbortController();
    let active = true;
    profitLoss({ start: range.start, end: range.end }, controller.signal)
      .then((data) => {
        if (active) setState({ status: 'success', data });
      })
      .catch(() => {
        if (active && !controller.signal.aborted) setState({ status: 'error', data: null });
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [valid, range.start, range.end, tick]);

  const refetch = useCallback(() => setTick((t) => t + 1), []);
  return { state, refetch };
}

const PL_STYLE_ID = 'fms-profitloss-styles';

// Deduplicated colocated stylesheet. Colors are local tokens (no reliance on a
// global theme) and bars animate via transform-only scaleX with a fixed 8px
// track to hold layout (CLS <= 0.1).
const FMS_PROFITLOSS_CSS = `
:root {
  --fms-pl-neutral-50:#f9fafb; --fms-pl-neutral-100:#f3f4f6;
  --fms-pl-neutral-200:#e5e7eb; --fms-pl-neutral-300:#d1d5db;
  --fms-pl-neutral-500:#6b7280; --fms-pl-neutral-900:#111827;
  --fms-pl-success:#15803d; --fms-pl-error:#dc2626;
  --fms-pl-primary:#2563eb; --fms-pl-expense:#f87171;
  --fms-space-1:4px; --fms-space-2:8px; --fms-space-3:12px;
  --fms-space-4:16px; --fms-space-6:24px; --fms-space-8:32px;
  --fms-text-sm:14px; --fms-text-base:16px; --fms-text-lg:18px;
  --fms-text-2xl:24px; --fms-text-3xl:30px;
  --fms-font-medium:500; --fms-font-semibold:600; --fms-font-bold:700;
  --fms-radius-md:8px; --fms-radius-full:9999px;
  --fms-shadow-sm:0 1px 2px rgba(0,0,0,0.05);
  --fms-transition-slow:300ms ease-out;
}
.pl-report{ max-width:1280px; margin:0 auto; padding:var(--fms-space-4); }
@media (min-width:768px){ .pl-report{ padding:var(--fms-space-6); } }
.pl-report__title{ font-size:var(--fms-text-2xl); font-weight:var(--fms-font-semibold); line-height:1.25; margin:0 0 var(--fms-space-1); }
.pl-report__range{ font-size:var(--fms-text-sm); color:var(--fms-pl-neutral-500); margin:0 0 var(--fms-space-6); }
.pl-report__table{ width:100%; border-collapse:collapse; background:#fff; border:1px solid var(--fms-pl-neutral-200); border-radius:var(--fms-radius-md); box-shadow:var(--fms-shadow-sm); margin-bottom:var(--fms-space-8); overflow:hidden; }
.pl-report__row th{ text-align:left; font-size:var(--fms-text-sm); color:var(--fms-pl-neutral-500); font-weight:var(--fms-font-medium); padding:var(--fms-space-4); }
.pl-report__row td{ text-align:right; font-size:var(--fms-text-2xl); font-weight:var(--fms-font-semibold); color:var(--fms-pl-neutral-900); font-variant-numeric:tabular-nums; padding:var(--fms-space-4); }
.pl-report__row--net th, .pl-report__row--net td{ border-top:1px solid var(--fms-pl-neutral-300); background:var(--fms-pl-neutral-50); }
.pl-report__row--net td{ font-size:var(--fms-text-3xl); font-weight:var(--fms-font-bold); }
.pl-report__net-arrow{ display:inline-block; margin-right:var(--fms-space-1); }
.pl-report__net-tag{ margin-left:var(--fms-space-2); font-size:var(--fms-text-sm); font-weight:var(--fms-font-medium); }
.pl-report__net-tag--positive, .pl-report__net-value--positive{ color:var(--fms-pl-success); }
.pl-report__net-tag--negative, .pl-report__net-value--negative{ color:var(--fms-pl-error); }
.pl-report__net-tag--zero, .pl-report__net-value--zero{ color:var(--fms-pl-neutral-500); }
.pl-report__breakdown{ margin-bottom:var(--fms-space-8); }
.pl-report__breakdown h2{ font-size:var(--fms-text-lg); font-weight:var(--fms-font-semibold); margin:0 0 var(--fms-space-3); }
.pl-report__breakdown-table{ width:100%; border-collapse:collapse; }
.pl-report__breakdown-table th[scope="col"]{ text-align:left; font-size:var(--fms-text-sm); color:var(--fms-pl-neutral-500); font-weight:var(--fms-font-medium); padding:var(--fms-space-2); }
.pl-report__bar-col{ width:40%; }
.pl-report__amount-col{ text-align:right; }
.pl-report__breakdown-table th[scope="row"]{ text-align:left; font-size:var(--fms-text-base); color:var(--fms-pl-neutral-900); font-weight:var(--fms-font-medium); padding:var(--fms-space-2); }
.pl-report__bar-cell{ padding:var(--fms-space-2); }
.pl-report__bar{ display:block; width:100%; height:var(--fms-space-2); border-radius:var(--fms-radius-full); background:var(--fms-pl-neutral-100); overflow:hidden; }
.pl-report__bar-fill{ display:block; height:100%; border-radius:var(--fms-radius-full); transform-origin:left; transition:transform var(--fms-transition-slow); }
.pl-report__bar-fill--income{ background:var(--fms-pl-primary); }
.pl-report__bar-fill--expense{ background:var(--fms-pl-expense); }
@media (prefers-reduced-motion:reduce){ .pl-report__bar-fill{ transition:none; } }
.pl-report__amount{ text-align:right; font-size:var(--fms-text-base); font-weight:var(--fms-font-medium); font-variant-numeric:tabular-nums; color:var(--fms-pl-neutral-900); padding:var(--fms-space-2); }
.pl-report__error{ border:1px solid var(--fms-pl-neutral-300); border-radius:var(--fms-radius-md); padding:var(--fms-space-4); margin-bottom:var(--fms-space-8); max-width:65ch; }
.pl-report__error-title{ font-size:var(--fms-text-base); font-weight:var(--fms-font-semibold); margin:0 0 var(--fms-space-2); }
.pl-report__error-body{ font-size:var(--fms-text-base); color:var(--fms-pl-neutral-500); margin:0 0 var(--fms-space-4); }
.pl-report__retry{ display:inline-flex; align-items:center; justify-content:center; min-height:44px; padding:var(--fms-space-3) var(--fms-space-4); border:none; border-radius:var(--fms-radius-md); background:var(--fms-pl-primary); color:#fff; font-size:var(--fms-text-sm); font-weight:var(--fms-font-semibold); cursor:pointer; }
.pl-report__retry:hover{ background:#1d4ed8; }
.pl-report__retry:focus-visible{ outline:3px solid var(--fms-pl-primary); outline-offset:2px; }
.pl-report__sr{ position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0 0 0 0); clip-path:inset(50%); white-space:nowrap; border:0; }
.pl-report__skeleton-cards{ display:grid; grid-template-columns:1fr; gap:var(--fms-space-4); margin-bottom:var(--fms-space-8); }
@media (min-width:768px){ .pl-report__skeleton-cards{ grid-template-columns:repeat(3,1fr); } }
.pl-report__skeleton-rows{ display:flex; flex-direction:column; gap:var(--fms-space-3); }
`;

if (typeof document !== 'undefined') {
  if (!document.getElementById(PL_STYLE_ID)) {
    const el = document.createElement('style');
    el.id = PL_STYLE_ID;
    el.textContent = FMS_PROFITLOSS_CSS;
    document.head.appendChild(el);
  }
}

/** Renders the loading skeleton (three summary cards + breakdown rows). */
function ProfitLossSkeleton(): ReactElement {
  return (
    <div className="pl-report__skeleton">
      <div className="pl-report__skeleton-cards">
        <Skeleton variant="card" announceOnLoad={false} />
        <Skeleton variant="card" announceOnLoad={false} />
        <Skeleton variant="card" announceOnLoad={false} />
      </div>
      <div className="pl-report__skeleton-rows">
        <Skeleton variant="table-row" announceOnLoad={false} height={24} />
        <Skeleton variant="table-row" announceOnLoad={false} height={24} />
        <Skeleton variant="table-row" announceOnLoad={false} height={24} />
      </div>
    </div>
  );
}

/** Renders the fetch-error panel with a solution-oriented message + Retry. */
function ProfitLossError({ onRetry }: { onRetry: () => void }): ReactElement {
  return (
    <div className="pl-report__error" role="alert">
      <p className="pl-report__error-title">Unable to load this report</p>
      <p className="pl-report__error-body">
        The details could not be retrieved. Please try again.
      </p>
      <button type="button" className="pl-report__retry" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

/**
 * Renders the profit/loss view: a semantic summary table (income, expenses,
 * net with profit/loss/break-even treatment), per-category breakdowns with
 * decorative bars, an empty-summary state for empty ranges, and skeleton /
 * error states.
 */
export function ProfitLossReport({ range }: ProfitLossReportProps): ReactElement {
  const { state, refetch } = useProfitLoss(range);
  const [barsReady, setBarsReady] = useState(false);

  useEffect(() => {
    if (state.status !== 'success') return;
    const raf = requestAnimationFrame(() => setBarsReady(true));
    return () => cancelAnimationFrame(raf);
  }, [state.status]);

  const data = state.data;
  const incomeLines = useMemo(
    () => (data ? topN(data.items.filter((l) => l.type === 'INCOME'), TOP_N) : []),
    [data],
  );
  const expenseLines = useMemo(
    () => (data ? topN(data.items.filter((l) => l.type === 'EXPENSE'), TOP_N) : []),
    [data],
  );
  const incomeMax = useMemo(() => sectionMaxUnits(incomeLines), [incomeLines]);
  const expenseMax = useMemo(() => sectionMaxUnits(expenseLines), [expenseLines]);

  const isEmpty =
    !data ||
    !isFiniteMoney(data.income) ||
    !isFiniteMoney(data.expense) ||
    !isFiniteMoney(data.net) ||
    (incomeLines.length === 0 && expenseLines.length === 0);

  const sign = data ? netSign(data.net) : 'zero';
  const rangeText =
    formatDate(range.start, LOCALE) && formatDate(range.end, LOCALE)
      ? `${formatDate(range.start, LOCALE)} to ${formatDate(range.end, LOCALE)}`
      : `${range.start} to ${range.end}`;

  const liveText =
    state.status === 'loading'
      ? 'Loading report'
      : state.status === 'error'
        ? 'Unable to load report'
        : '';

  return (
    <section className="pl-report" aria-labelledby="pl-report-title">
      <h1 className="pl-report__title" id="pl-report-title">
        Profit &amp; Loss
      </h1>
      <p className="pl-report__range">{rangeText}</p>
      <span className="pl-report__sr" role="status">
        {liveText}
      </span>

      {state.status === 'loading' && <ProfitLossSkeleton />}

      {state.status === 'error' && <ProfitLossError onRetry={refetch} />}

      {state.status === 'success' && isEmpty && (
        <EmptyReportSummary
          title="No transactions in this date range"
          hint="Add transactions or adjust the date range."
        />
      )}

      {state.status === 'success' && !isEmpty && data && (
        <>
          <table className="pl-report__table">
            <caption className="pl-report__sr">Profit and Loss summary for {rangeText}</caption>
            <tbody>
              <tr className="pl-report__row">
                <th scope="row">Income</th>
                <td className="pl-report__amount">
                  {formatMinorUnits(data.income.amount, data.income.currency)}
                </td>
              </tr>
              <tr className="pl-report__row">
                <th scope="row">Expenses</th>
                <td className="pl-report__amount">
                  {formatMinorUnits(data.expense.amount, data.expense.currency)}
                </td>
              </tr>
              <tr className="pl-report__row pl-report__row--net">
                <th scope="row">
                  <span
                    className={`pl-report__net-arrow pl-report__net-tag--${sign}`}
                    aria-hidden="true"
                  >
                    {NET_GLYPH[sign]}
                  </span>
                  Net
                  <span className={`pl-report__net-tag pl-report__net-tag--${sign}`}>
                    {NET_LABEL[sign]}
                  </span>
                </th>
                <td className={`pl-report__amount pl-report__net-value--${sign}`}>
                  {formatMinorUnits(data.net.amount, data.net.currency)}
                </td>
              </tr>
            </tbody>
          </table>

          {incomeLines.length > 0 && (
            <section className="pl-report__breakdown" aria-labelledby="pl-income-h">
              <h2 id="pl-income-h">Income by Category</h2>
              <table className="pl-report__breakdown-table">
                <caption className="pl-report__sr">Income by category for {rangeText}</caption>
                <thead>
                  <tr>
                    <th scope="col">Category</th>
                    <th scope="col" className="pl-report__bar-col">
                      Share
                    </th>
                    <th scope="col" className="pl-report__amount-col">
                      Amount
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {incomeLines.map((line) => (
                    <tr key={line.categoryId ?? 'other-income'}>
                      <th scope="row">{line.categoryName}</th>
                      <td className="pl-report__bar-cell">
                        <span className="pl-report__bar" aria-hidden="true">
                          <span
                            className="pl-report__bar-fill pl-report__bar-fill--income"
                            style={{
                              transform: barsReady
                                ? `scaleX(${barRatio(line.total.amount, incomeMax)})`
                                : 'scaleX(0)',
                            }}
                          />
                        </span>
                      </td>
                      <td className="pl-report__amount">
                        {formatMinorUnits(line.total.amount, line.total.currency)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>
          )}

          {expenseLines.length > 0 && (
            <section className="pl-report__breakdown" aria-labelledby="pl-expense-h">
              <h2 id="pl-expense-h">Expenses by Category</h2>
              <table className="pl-report__breakdown-table">
                <caption className="pl-report__sr">Expenses by category for {rangeText}</caption>
                <thead>
                  <tr>
                    <th scope="col">Category</th>
                    <th scope="col" className="pl-report__bar-col">
                      Share
                    </th>
                    <th scope="col" className="pl-report__amount-col">
                      Amount
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {expenseLines.map((line) => (
                    <tr key={line.categoryId ?? 'other-expense'}>
                      <th scope="row">{line.categoryName}</th>
                      <td className="pl-report__bar-cell">
                        <span className="pl-report__bar" aria-hidden="true">
                          <span
                            className="pl-report__bar-fill pl-report__bar-fill--expense"
                            style={{
                              transform: barsReady
                                ? `scaleX(${barRatio(line.total.amount, expenseMax)})`
                                : 'scaleX(0)',
                            }}
                          />
                        </span>
                      </td>
                      <td className="pl-report__amount">
                        {formatMinorUnits(line.total.amount, line.total.currency)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>
          )}
        </>
      )}
    </section>
  );
}

export default ProfitLossReport;