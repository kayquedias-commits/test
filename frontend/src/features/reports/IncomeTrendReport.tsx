/**
 * Income trend report (Module E): renders the income-trend series for the
 * selected date range as a period table always accompanied by an accessible
 * SVG chart. The component contributes a subheading and renders inside the
 * page's single <h1>; it never renders its own page-level heading.
 */
import { useEffect, useMemo, useState } from 'react';
import type { CSSProperties, ReactElement } from 'react';
import { incomeTrend } from '../../services/api/endpoints';
import type { IncomeTrendSummary, TrendPoint } from '../../services/api/types';
import { Skeleton } from '../../components/ui/Skeleton';
import EmptyReportSummary from './EmptyReportSummary';
import { formatMinorUnits, isZero, subtractMoney } from '../../utils/currency';
import { formatDate, isRangeValid } from '../../utils/date';

export interface IncomeTrendRange {
  start: string;
  end: string;
}

export interface IncomeTrendReportProps {
  range: IncomeTrendRange;
}

/** Solution-oriented copy; never echoes server or stack-trace payloads. */
const LOAD_ERROR_MESSAGE = "Couldn't load income trend.";

/** Miller's Law: render at most this many period rows; longer series scrolls within the card. */
const MAX_ROWS = 9;

interface Row {
  period: string;
  amount: string;
  currency: string;
  deltaMinor: bigint | null;
  deltaPct: number | null;
}

/** First period has no predecessor; a zero prior baseline also leaves the percentage undefined. */
function buildRows(items: TrendPoint[]): Row[] {
  return items.map((point, i) => {
    const current = point.amount.amount;
    const prev = i > 0 ? items[i - 1].amount.amount : undefined;
    if (prev === undefined) {
      return {
        period: point.period,
        amount: current,
        currency: point.amount.currency,
        deltaMinor: null,
        deltaPct: null,
      };
    }
    // BigInt minor-unit arithmetic only — never binary floats (delta is money).
    const deltaMinor = BigInt(subtractMoney(current, prev));
    let deltaPct: number | null = null;
    if (!isZero(prev)) {
      deltaPct = (Number(subtractMoney(current, prev)) / Number(prev)) * 100;
    }
    return {
      period: point.period,
      amount: current,
      currency: point.amount.currency,
      deltaMinor,
      deltaPct,
    };
  });
}

interface TrendState {
  summary: IncomeTrendSummary | null;
  status: 'loading' | 'error' | 'ready';
}

/**
 * Fetches the income-trend series for the range. An invalid range (start after
 * end) is surfaced as an error state rather than a broken chart.
 */
function useIncomeTrend(range: IncomeTrendRange) {
  const [state, setState] = useState<TrendState>({ summary: null, status: 'loading' });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    if (!isRangeValid(range.start, range.end)) {
      setState({ summary: null, status: 'error' });
      return () => controller.abort();
    }
    setState({ summary: null, status: 'loading' });
    incomeTrend({ start: range.start, end: range.end }, controller.signal)
      .then((summary) => setState({ summary, status: 'ready' }))
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return;
        setState({ summary: null, status: 'error' });
      });
    return () => controller.abort();
  }, [range.start, range.end, attempt]);

  return { ...state, retry: () => setAttempt((n) => n + 1) };
}

export function IncomeTrendReport({ range }: IncomeTrendReportProps): ReactElement {
  const { summary, status, retry } = useIncomeTrend(range);
  const rows = useMemo(() => (summary ? buildRows(summary.items) : []), [summary]);

  const locale = 'en-US';
  const currency = rows[0]?.currency ?? 'USD';

  return (
    <section className="report-card" aria-labelledby="income-trend-title">
      <h2 id="income-trend-title">Income Trend</h2>
      <p className="report-card__subtitle">
        {formatDate(range.start, locale)} – {formatDate(range.end, locale)}
      </p>

      {status === 'loading' && <IncomeTrendSkeleton />}
      {status === 'error' && <IncomeTrendError onRetry={retry} />}
      {status === 'ready' && summary && summary.items.length === 0 && (
        <EmptyReportSummary
          title="No income in this period"
          hint="Widen the date range or add income transactions to see a trend."
        />
      )}
      {status === 'ready' && summary && summary.items.length > 0 && (
        <IncomeTrendView rows={rows} range={range} currency={currency} locale={locale} />
      )}
    </section>
  );
}

/** Loading skeleton mirrors the populated layout exactly to avoid CLS; presentational only. */
function IncomeTrendSkeleton(): ReactElement {
  return (
    <div aria-hidden="true">
      <Skeleton variant="text" width="35%" height={24} announceOnLoad={false} />
      <div style={{ marginTop: 'var(--space-6)' }}>
        <Skeleton variant="table-row" lines={5} announceOnLoad={false} />
      </div>
    </div>
  );
}

function IncomeTrendError({ onRetry }: { onRetry: () => void }): ReactElement {
  return (
    <div role="alert" className="report-card__error">
      <p className="report-card__error-message">{LOAD_ERROR_MESSAGE}</p>
      <button type="button" className="report-card__retry" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

interface ViewProps {
  rows: Row[];
  range: IncomeTrendRange;
  currency: string;
  locale: string;
}

function IncomeTrendView({ rows, range, currency, locale }: ViewProps): ReactElement {
  const visibleRows = rows.slice(0, MAX_ROWS);
  return (
    <div>
      <IncomeTrendChart rows={rows} range={range} currency={currency} locale={locale} />
      <div className="report-card__table-scroll">
        <table className="report-table">
          <caption className="report-table__caption">
            Income by period from {formatDate(range.start, locale)} to{' '}
            {formatDate(range.end, locale)}. The summary table below accompanies the chart.
          </caption>
          <thead>
            <tr>
              <th scope="col">Period</th>
              <th scope="col" className="report-table__num">
                Income
              </th>
              <th scope="col" className="report-table__num">
                Change vs previous
              </th>
            </tr>
          </thead>
          <tbody>
            {visibleRows.map((row, i) => (
              <tr key={`${row.period}-${i}`} className="report-table__row">
                <th scope="row" className="report-table__period">
                  {formatDate(row.period, locale)}
                </th>
                <td className="report-table__num">
                  {formatMinorUnits(row.amount, currency, locale)}
                </td>
                <td className="report-table__num">
                  <DeltaCell row={row} currency={currency} locale={locale} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** Delta renders arrow + signed text + semantic color (arrow/text are the indicators). */
function DeltaCell({
  row,
  currency,
  locale,
}: {
  row: Row;
  currency: string;
  locale: string;
}): ReactElement {
  if (row.deltaMinor === null) {
    return (
      <span className="report-table__delta" aria-label="No previous period">
        —
      </span>
    );
  }
  const increasing = row.deltaMinor > 0n;
  const amount = formatMinorUnits(row.deltaMinor.toString(), currency, locale);
  const cls = increasing
    ? 'report-table__delta report-table__delta--up'
    : 'report-table__delta report-table__delta--down';

  // Percentage is undefined when the previous period was a zero baseline.
  if (row.deltaPct === null) {
    const direction = increasing ? 'Increase' : 'Decrease';
    return (
      <span className={cls} aria-label={`${direction} versus previous period`}>
        <span aria-hidden="true" className="report-table__arrow">
          {increasing ? '▲' : '▼'}
        </span>
        <span className="report-table__delta-text">{amount}</span>
      </span>
    );
  }

  if (row.deltaMinor === 0n) {
    return (
      <span className="report-table__delta report-table__delta--flat" aria-label="No change versus previous period">
        {amount} (0.0%)
      </span>
    );
  }

  const arrow = increasing ? '▲' : '▼';
  const pct = `${increasing ? '+' : '−'}${Math.abs(row.deltaPct).toFixed(1)}%`;
  const direction = increasing ? 'Increase' : 'Decrease';
  return (
    <span className={cls} aria-label={`${direction} of ${Math.abs(row.deltaPct).toFixed(1)}% versus previous period`}>
      <span aria-hidden="true" className="report-table__arrow">
        {arrow}
      </span>
      <span className="report-table__delta-text">
        {amount} ({pct})
      </span>
    </span>
  );
}

const chartStyle: CSSProperties = {
  width: '100%',
  height: 160,
  display: 'block',
  overflow: 'hidden',
};

/** Declarative SVG only (no innerHTML/string builds, no data: URIs, no inline handlers). */
function IncomeTrendChart({
  rows,
  range,
  currency,
  locale,
}: {
  rows: Row[];
  range: IncomeTrendRange;
  currency: string;
  locale: string;
}): ReactElement {
  const W = 720;
  const H = 160;
  const pad = 12;

  const values = rows.map((r) => Number(r.amount));
  const maxV = Math.max(...values, 0);
  const minV = Math.min(...values, 0);
  const span = maxV - minV || 1;

  const points = rows.map((r, i) => {
    const x = rows.length > 1 ? pad + (i / (rows.length - 1)) * (W - 2 * pad) : W / 2;
    const y = H - pad - ((Number(r.amount) - minV) / span) * (H - 2 * pad);
    return [x, y];
  });

  const linePoints = points.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ');

  return (
    <svg
      className="report-chart"
      style={chartStyle}
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={`Income trend by period, ${formatDate(range.start, locale)} to ${formatDate(range.end, locale)}. The data table below accompanies this chart.`}
      tabIndex={0}
      focusable="false"
    >
      <title>Income trend by period</title>
      <desc>
        Income trend in {currency} by period for {formatDate(range.start, locale)} to{' '}
        {formatDate(range.end, locale)}. A data table appears below this chart.
      </desc>
      <g aria-hidden="true">
        <line
          x1={pad}
          y1={H - pad}
          x2={W - pad}
          y2={H - pad}
          stroke="var(--color-border-subtle)"
          strokeWidth={1}
        />
        <line
          x1={pad}
          y1={pad}
          x2={W - pad}
          y2={pad}
          stroke="var(--color-border-faint)"
          strokeWidth={1}
        />
      </g>
      <polyline
        points={linePoints}
        fill="none"
        stroke="var(--color-primary)"
        strokeWidth={2}
        strokeLinecap="round"
        strokeLinejoin="round"
        style={{
          opacity: 1,
          transform: 'translateY(0)',
          transition: 'opacity 200ms ease-out',
        }}
      />
      <g aria-hidden="true">
        {points.map(([x, y], i) => (
          <circle
            key={i}
            cx={x}
            cy={y}
            r={4}
            fill="var(--color-surface-1)"
            stroke="var(--color-primary)"
            strokeWidth={2}
          />
        ))}
      </g>
    </svg>
  );
}