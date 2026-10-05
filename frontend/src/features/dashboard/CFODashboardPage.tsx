/**
 * CFODashboardPage — role-tailored executive dashboard (read-only).
 *
 * Loads the permission-scoped `/v1/dashboard/kpis` read model via React Query
 * (`refetchOnWindowFocus`) and renders up to MAX_KPI_CARDS KpiCards, each with a
 * single drill-down action that preserves breadcrumb context (`/dashboard` →
 * `/transactions?filter=<kpi>`). Composes KpiCard, EmptyDashboardState, and
 * KpiCard's internal Skeleton; per-role route variants are a future seam, so the
 * RBAC gate is passed in from the router via the `canView` prop.
 *
 * Security posture: all API-derived values render as React text nodes (auto-
 * escaped); every drill-down href is built only through URLSearchParams and is
 * always a relative path; trend icons are hardcoded inline SVG components, never
 * API-derived strings; the page issues only a GET and exposes no write
 * endpoints. Trend arrows are intentionally omitted because the DTO carries no
 * trend field — surfaced as a backend seam in KpiCard.
 */
import React from 'react';
import { useQuery } from '@tanstack/react-query';
import { KpiCard } from './KpiCard';
import { EmptyDashboardState } from './EmptyDashboardState';
import { listDashboardKpis } from '../../services/api/endpoints';
import type { DashboardKPI, Money } from '../../services/api/types';
import { formatMinorUnits } from '../../utils/currency';

/** Miller's Law ceiling on concurrently presented KPI cards. */
const MAX_KPI_CARDS = 9;

const STYLE_ID = 'fms-cfo-dashboard-styles';

function isForbiddenError(error: unknown): boolean {
  return (
    !!error &&
    typeof error === 'object' &&
    (error as { status?: unknown }).status === 403
  );
}

/** Always a relative path so KpiCard's safe-href guard accepts it; filter is
 *  percent-encoded via URLSearchParams (never string-concatenated from the DTO). */
function buildDrilldownHref(kpiId: string): string {
  const sp = new URLSearchParams({ filter: kpiId });
  return `/transactions?${sp.toString()}`;
}

function formatKpiValue(money: Money): string {
  try {
    return formatMinorUnits(money.amount, money.currency);
  } catch {
    // Fall back to the raw amount text so a formatting edge case never blanks
    // a KPI or throws during render.
    return money.amount;
  }
}

const FMS_CFO_CSS = `
.fms-cfo{max-width:1280px;margin:0 auto;padding:var(--space-6,24px) var(--space-4,16px)}
@media(min-width:768px){.fms-cfo{padding:var(--space-6,24px) var(--space-6,24px)}}
@media(min-width:1024px){.fms-cfo{padding:var(--space-6,24px) var(--space-8,32px)}}
.fms-cfo *:focus-visible{outline:2px solid var(--color-focus-ring,#4f46e5);outline-offset:2px}
.fms-cfo-sr{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
.fms-cfo__crumb{font-size:var(--text-sm,14px);color:var(--color-text-muted,#6b7280);margin:0 0 var(--space-2,8px)}
.fms-cfo__crumb ol{list-style:none;margin:0;padding:0;display:flex;align-items:center;gap:var(--space-2,8px);flex-wrap:wrap}
.fms-cfo__crumb a{min-height:44px;display:inline-flex;align-items:center;color:var(--color-primary,#2563eb);text-decoration:none}
.fms-cfo__crumb a:hover{text-decoration:underline;text-underline-offset:4px}
.fms-cfo__crumb--current{font-weight:var(--font-semibold,600);color:var(--color-text,#111827)}
.fms-cfo__title{margin:0 0 var(--space-6,24px);font-size:var(--text-2xl,24px);font-weight:var(--font-semibold,600);line-height:var(--leading-tight,1.25)}
.fms-cfo__grid{list-style:none;margin:0;padding:0;display:grid;gap:var(--space-4,16px);animation:fms-cfo-enter 300ms ease-out}
.fms-cfo__grid > li{display:flex}
@keyframes fms-cfo-enter{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
.fms-cfo__state{grid-column:1/-1;text-align:center;padding:var(--space-16,64px) var(--space-6,24px);max-width:42ch;margin:0 auto}
.fms-cfo__state h2{margin:0 0 var(--space-2,8px);font-size:var(--text-xl,20px);font-weight:var(--font-semibold,600)}
.fms-cfo__state p{margin:0 0 var(--space-4,16px);font-size:var(--text-base,16px);color:var(--color-text-muted,#6b7280);line-height:var(--leading-normal,1.5)}
.fms-cfo__retry{min-height:44px;padding:var(--space-2,8px) var(--space-6,24px);font-size:var(--text-sm,14px);font-weight:var(--font-semibold,600);color:#fff;background:var(--color-primary,#2563eb);border:none;border-radius:var(--radius-md,8px);cursor:pointer}
.fms-cfo__updating{margin:0 0 var(--space-4,16px);font-size:var(--text-xs,12px);color:var(--color-text-muted,#6b7280)}
@media(min-width:768px){.fms-cfo__grid{grid-template-columns:repeat(2,1fr)}}
@media(min-width:1024px){.fms-cfo__grid{grid-template-columns:repeat(3,1fr)}}
@media(prefers-reduced-motion:reduce){.fms-cfo__grid{animation:none}}
`;

function ensureStyles(): void {
  // Colocated <style> so the page needs no inline style attrs (CSP-friendly);
  // injected once per mount, idempotently.
  if (document.getElementById(STYLE_ID)) {
    return;
  }
  const style = document.createElement('style');
  style.id = STYLE_ID;
  style.textContent = FMS_CFO_CSS;
  document.head.appendChild(style);
}

function cashIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <rect x="2" y="6" width="20" height="12" rx="2" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

function revenueIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M3 17l6-6 4 4 8-8" />
      <path d="M14 7h7v7" />
    </svg>
  );
}

function marginIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 20V10" />
      <path d="M6 20V4" />
      <path d="M18 20v-4" />
    </svg>
  );
}

function expenseRatioIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 3" />
    </svg>
  );
}

function burnRateIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 3v18" />
      <path d="M5 21h14" />
    </svg>
  );
}

function defaultIcon(): React.ReactElement {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="9" />
    </svg>
  );
}

/** Icon allowlist keyed by KPI id slug. Ids are matched only against these
 *  hardcoded entries; anything unknown falls back to the default. No id string
 *  from the API is ever interpreted as markup or a path. */
const KPI_ICONS: Record<string, () => React.ReactElement> = {
  cash: cashIcon,
  revenue: revenueIcon,
  margin: marginIcon,
  'expense-ratio': expenseRatioIcon,
  'burn-rate': burnRateIcon,
  default: defaultIcon,
};

export interface CFODashboardPageProps {
  /** Route-guard-resolved RBAC gate; false renders an inline 403 panel (never
   *  a blank or partially loaded grid). */
  canView: boolean;
  /** Stable tenant/user scope appended to the query key to prevent
   *  cross-session React Query cache leakage. */
  scopeKey?: string;
}

export function CFODashboardPage({ canView, scopeKey }: CFODashboardPageProps) {
  const { data, isPending, isFetching, isError, error, refetch } = useQuery({
    queryKey: scopeKey ? ['dashboard-kpis', scopeKey] : ['dashboard-kpis'],
    queryFn: () => listDashboardKpis(),
    refetchOnWindowFocus: true,
  });

  ensureStyles();

  const kpiData = data ?? [];

  return (
    <main className="fms-cfo">
      {!canView ? (
        <section className="fms-cfo__state" role="alert">
          <h2>Insufficient permissions</h2>
          <p>You don&apos;t have access to the executive dashboard.</p>
        </section>
      ) : (
        <>
          <nav aria-label="Breadcrumb" className="fms-cfo__crumb">
            <ol>
              <li>
                <span aria-current="page" className="fms-cfo__crumb--current">
                  Dashboard
                </span>
              </li>
              <li aria-hidden="true">/</li>
              <li>
                <a href="/transactions">Transactions</a>
              </li>
            </ol>
          </nav>
          <h1 className="fms-cfo__title">Executive Financial Overview</h1>

          {data !== undefined && isFetching && (
            <p className="fms-cfo__updating" role="status">
              Updating figures
            </p>
          )}

          {isPending && (
            <>
              <p className="fms-cfo-sr" role="status">
                Loading dashboard KPIs
              </p>
              <ul
                className="fms-cfo__grid"
                role="list"
                aria-label="Loading key performance indicators"
              >
                {Array.from({ length: MAX_KPI_CARDS }, (_, index) => (
                  <li key={index}>
                    <KpiCard
                      loading
                      announceOnLoad={false}
                      label=""
                      ariaLabel={`Loading KPI ${index + 1}`}
                    />
                  </li>
                ))}
              </ul>
            </>
          )}

          {isError && data === undefined && (
            <section className="fms-cfo__state" role="alert">
              <h2>Couldn&apos;t load your KPIs</h2>
              <p>
                {isForbiddenError(error)
                  ? "You don't have permission to view executive KPIs."
                  : error instanceof Error
                    ? error.message
                    : "We couldn't load your key performance indicators. Please try again."}
              </p>
              {!isForbiddenError(error) && (
                <button
                  type="button"
                  className="fms-cfo__retry"
                  onClick={() => refetch()}
                >
                  Try again
                </button>
              )}
            </section>
          )}

          {!isPending && !isError && kpiData.length === 0 && (
            <EmptyDashboardState />
          )}

          {!isPending && !isError && kpiData.length > 0 && (
            <ul
              className="fms-cfo__grid"
              role="list"
              aria-label="Executive key performance indicators"
            >
              {kpiData.slice(0, MAX_KPI_CARDS).map((kpi: DashboardKPI) => {
                const icon =
                  KPI_ICONS[kpi.id] ?? KPI_ICONS.default ?? defaultIcon;
                return (
                  <li key={kpi.id}>
                    <KpiCard
                      label={kpi.label}
                      value={formatKpiValue(kpi.value)}
                      sublabel={kpi.hint}
                      icon={icon()}
                      primaryAction={{
                        label: 'View transactions',
                        href: buildDrilldownHref(kpi.id),
                      }}
                    />
                  </li>
                );
              })}
            </ul>
          )}
        </>
      )}
    </main>
  );
}

export default CFODashboardPage;