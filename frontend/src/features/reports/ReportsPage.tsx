import { useCallback, useMemo, useState, type ComponentType, type KeyboardEvent, type ReactElement } from 'react';
import { useNavigate } from 'react-router-dom';
import ProfitLossReport from './ProfitLossReport';
import ExpenseBreakdownReport from './ExpenseBreakdownReport';
import IncomeTrendReport from './IncomeTrendReport';
import CashFlowReport from './CashFlowReport';
import EmptyReportSummary from './EmptyReportSummary';
import { usePermission } from '../../hooks/usePermission';
import { formatDate, isRangeValid, startOfMonth, endOfMonth, todayInLocale, toIsoDate } from '../../utils/date';

/** Stable id for the report hub stylesheet so ensureStyles is idempotent. */
const STYLE_ID = 'fms-reports-page-styles';

function ensureStyles(): void {
  if (document.getElementById(STYLE_ID)) return;
  const style = document.createElement('style');
  style.id = STYLE_ID;
  style.textContent = `
    :root {
      --page-bg:#F9FAFB; --surface:#FFFFFF; --surface-muted:#F3F4F6;
      --border:#E5E7EB; --border-strong:#D1D5DB;
      --text-primary:#111827; --text-secondary:#6B7280; --text-tertiary:#9CA3AF;
      --primary:#2563EB; --primary-hover:#1D4ED8;
      --danger:#DC2626; --danger-bg:#FEF2F2;
      --space-1:4px; --space-2:8px; --space-3:12px; --space-4:16px;
      --space-6:24px; --space-8:32px; --space-12:48px;
      --radius-md:8px; --radius-lg:12px; --radius-full:9999px;
      --shadow-sm:0 1px 2px rgba(0,0,0,0.05);
      --transition-fast:150ms ease-out; --transition-base:200ms ease-out;
    }
    .reports-page {
      max-width:1280px; margin:0 auto; padding:var(--space-4);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      color: var(--text-primary); box-sizing: border-box;
    }
    .reports-page * { box-sizing: border-box; }
    .reports-page :focus-visible { outline: 2px solid var(--primary); outline-offset: 2px; }
    .reports-sr { position: absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0,0,0,0); white-space:nowrap; border:0; }
    .reports-title { font-size:24px; font-weight:700; margin:0 0 var(--space-6); color:var(--text-primary); }
    .reports-insufficient { background:var(--danger-bg); border:1px solid var(--danger); border-radius:var(--radius-md); padding:var(--space-6); }
    .reports-insufficient h2 { margin:0 0 var(--space-2); font-size:18px; font-weight:600; color:var(--danger); }
    .reports-insufficient p { margin:0; font-size:14px; line-height:1.5; color:var(--text-primary); max-width:65ch; }
    .reports-bar {
      display:flex; flex-wrap:wrap; align-items:flex-start; gap: var(--space-4);
      margin-bottom: var(--space-6);
    }
    .reports-presets {
      display:inline-flex; gap:2px; background:var(--surface-muted);
      border-radius:var(--radius-full); padding:3px;
    }
    .reports-preset {
      min-height:44px; padding:var(--space-2) var(--space-3);
      border:none; border-radius:var(--radius-full); background:transparent;
      font-size:14px; font-weight:500; color:var(--text-secondary); cursor:pointer;
      transition: color var(--transition-fast), background var(--transition-fast), box-shadow var(--transition-fast);
    }
    .reports-preset:hover { color:var(--text-primary); }
    .reports-preset[aria-pressed="true"] {
      background:var(--surface); color:var(--text-primary);
      box-shadow:var(--shadow-sm); font-weight:600;
    }
    .reports-pickergroup { display:flex; flex-wrap:wrap; align-items:center; gap:var(--space-3); }
    .reports-field { display:flex; flex-direction:column; gap:var(--space-1); }
    .reports-field label { font-size:14px; font-weight:500; color:var(--text-secondary); }
    .reports-input {
      height:44px; min-width:0; width:160px; padding:0 var(--space-3);
      border:1px solid var(--border-strong); border-radius:var(--radius-md);
      font-size:16px; color:var(--text-primary); background:var(--surface);
      transition: border-color var(--transition-fast);
    }
    .reports-input[aria-invalid="true"] { border-color:var(--danger); }
    .reports-sep { color:var(--text-tertiary); font-size:16px; align-self:flex-end; padding-bottom:var(--space-3); }
    .reports-date-error {
      flex-basis:100%; min-height:20px; display:flex; align-items:center; gap:var(--space-2);
      color:var(--danger); font-size:14px; font-weight:500;
    }
    .reports-date-error[hidden] { display:none; }
    .reports-date-error svg { flex:none; }
    .reports-range-note { flex-basis:100%; margin:0; color:var(--text-secondary); font-size:14px; }
    .reports-export {
      min-height:44px; padding:0 var(--space-4); margin-left:auto;
      display:inline-flex; align-items:center; gap:var(--space-2);
      border:1px solid var(--border-strong); border-radius:var(--radius-md);
      background:var(--surface); color:var(--text-primary); font-size:16px; font-weight:600; cursor:pointer;
      transition: border-color var(--transition-fast), color var(--transition-fast), background var(--transition-fast);
    }
    .reports-export:hover:not(:disabled) { border-color:var(--primary); color:var(--primary); }
    .reports-export:active:not(:disabled) { background:var(--surface-muted); }
    .reports-export:disabled { opacity:0.5; cursor:not-allowed; }
    .reports-tabs {
      display:flex; gap: var(--space-1); width:100%; overflow-x:auto;
      border-bottom:1px solid var(--border); margin-bottom: var(--space-6);
    }
    .reports-tab {
      min-height:44px; padding:var(--space-3) var(--space-4); flex:none;
      border:none; background:transparent; border-radius:var(--radius-md) var(--radius-md) 0 0;
      font-size:16px; font-weight:500; color:var(--text-secondary); cursor:pointer;
      transition: color var(--transition-fast), box-shadow var(--transition-fast);
    }
    .reports-tab:hover:not([aria-selected="true"]) { color:var(--text-primary); }
    .reports-tab[aria-selected="true"] {
      color:var(--primary); font-weight:600;
      box-shadow: inset 0 -3px 0 var(--primary);
    }
    .reports-panel {
      background:var(--surface); border:1px solid var(--border); border-radius:var(--radius-lg);
      padding:var(--space-6); min-height:min(60vh, 480px);
      animation: reports-panel-in var(--transition-base);
    }
    @keyframes reports-panel-in {
      from { opacity:0; transform:translateY(4px); }
      to { opacity:1; transform:translateY(0); }
    }
    @media (prefers-reduced-motion: reduce) {
      .reports-panel { animation:none; }
      .reports-preset, .reports-tab, .reports-input, .reports-export { transition:none; }
    }
    @media (min-width:768px) { .reports-page { padding:var(--space-6); } }
    @media (max-width:767px) {
      .reports-bar { flex-direction:column; align-items:stretch; }
      .reports-pickergroup { flex-direction:column; align-items:stretch; }
      .reports-input { width:100%; }
      .reports-sep { display:none; }
      .reports-export { margin-left:0; width:100%; justify-content:center; min-height:48px; }
      .reports-presets { width:100%; }
      .reports-preset { flex:1; }
      .reports-panel { padding:var(--space-4); }
    }
  `;
  document.head.appendChild(style);
  void style.getBoundingClientRect();
}

type TabId = 'pl' | 'expense' | 'income' | 'cashflow';
type PresetId = 'month' | 'quarter' | 'year';
type ReportRange = { start: string; end: string };

interface TabDef {
  id: TabId;
  label: string;
  Comp: ComponentType<{ range: ReportRange }>;
}

const TABS: ReadonlyArray<TabDef> = [
  { id: 'pl', label: 'Profit & Loss', Comp: ProfitLossReport },
  { id: 'expense', label: 'Expense Breakdown', Comp: ExpenseBreakdownReport },
  { id: 'income', label: 'Income Trend', Comp: IncomeTrendReport },
  { id: 'cashflow', label: 'Cash Flow', Comp: CashFlowReport },
];

const PRESETS: ReadonlyArray<{ id: PresetId; label: string }> = [
  { id: 'month', label: 'This month' },
  { id: 'quarter', label: 'Quarter' },
  { id: 'year', label: 'Year' },
];

const LOCALE = 'en-US';
const TIME_ZONE = Intl.DateTimeFormat().resolvedOptions().timeZone;

/** Build the ISO range for a quick preset relative to the given ISO "today". */
function computePresetRange(id: PresetId, todayIso: string): ReportRange {
  const d = new Date(`${todayIso}T12:00:00Z`);
  const year = d.getUTCFullYear();
  if (id === 'month') {
    return { start: toIsoDate(startOfMonth(d)), end: toIsoDate(endOfMonth(d)) };
  }
  if (id === 'quarter') {
    const quarterStartMonth = Math.floor(d.getUTCMonth() / 3) * 3;
    const start = new Date(Date.UTC(year, quarterStartMonth, 1));
    const end = new Date(Date.UTC(year, quarterStartMonth + 3, 1) - 1);
    return { start: toIsoDate(start), end: toIsoDate(end) };
  }
  const start = new Date(Date.UTC(year, 0, 1));
  const end = new Date(Date.UTC(year + 1, 0, 1) - 1);
  return { start: toIsoDate(start), end: toIsoDate(end) };
}

function DownloadIcon(): ReactElement {
  return (
    <svg aria-hidden="true" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
      <polyline points="7 10 12 15 17 10" />
      <line x1="12" y1="15" x2="12" y2="3" />
    </svg>
  );
}

function WarnIcon(): ReactElement {
  return (
    <svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
  );
}

/**
 * Report hub (Module E). Composes the four report views behind an accessible tab
 * bar and a date-range picker, and gates the export affordance on permission.
 */
function ReportsPage(): ReactElement {
  ensureStyles();
  const navigate = useNavigate();
  const { can } = usePermission();
  const canReadReports = can('report:read');
  const canExport = can('export:create');

  const initial = useMemo(() => computePresetRange('month', todayInLocale(LOCALE, TIME_ZONE)), []);
  const [preset, setPreset] = useState<PresetId | null>('month');
  const [start, setStart] = useState(initial.start);
  const [end, setEnd] = useState(initial.end);
  const [activeTab, setActiveTab] = useState<TabId>('pl');
  const [mounted, setMounted] = useState<ReadonlySet<TabId>>(() => new Set(['pl']));

  const rangeValid = useMemo(() => isRangeValid(start, end), [start, end]);

  const applyPreset = useCallback(
    (id: PresetId) => {
      const next = computePresetRange(id, todayInLocale(LOCALE, TIME_ZONE));
      setPreset(id);
      setStart(next.start);
      setEnd(next.end);
    },
    []
  );

  const handleStartChange = (value: string): void => {
    setStart(value);
    setPreset(null);
  };

  const handleEndChange = (value: string): void => {
    setEnd(value);
    setPreset(null);
  };

  const activateTab = (id: TabId): void => {
    setActiveTab(id);
    setMounted((prev) => (prev.has(id) ? prev : new Set(prev).add(id)));
  };

  const handleTabsKeyDown = (e: KeyboardEvent<HTMLDivElement>): void => {
    const index = TABS.findIndex((t) => t.id === activeTab);
    let nextIndex = index;
    if (e.key === 'ArrowRight') nextIndex = (index + 1) % TABS.length;
    else if (e.key === 'ArrowLeft') nextIndex = (index - 1 + TABS.length) % TABS.length;
    else if (e.key === 'Home') nextIndex = 0;
    else if (e.key === 'End') nextIndex = TABS.length - 1;
    else return;
    e.preventDefault();
    const next = TABS[nextIndex];
    activateTab(next.id);
  };

  const handleExport = (): void => {
    if (!rangeValid || !canExport) return;
    const params = new URLSearchParams({ start, end });
    navigate(`/exports?${params.toString()}`);
  };

  if (!canReadReports) {
    return (
      <main className="reports-page">
        <h1 className="reports-title">Reports</h1>
        <div role="alert" className="reports-insufficient">
          <h2>Insufficient permissions</h2>
          <p>You do not have permission to view reports. Contact an administrator if you believe this is an error.</p>
        </div>
      </main>
    );
  }

  return (
    <main className="reports-page">
      <h1 className="reports-title">Reports</h1>

      <div className="reports-bar">
        <div role="group" aria-label="Quick date ranges" className="reports-presets">
          {PRESETS.map((p) => (
            <button
              key={p.id}
              type="button"
              className="reports-preset"
              aria-pressed={preset === p.id}
              onClick={() => applyPreset(p.id)}
            >
              {p.label}
            </button>
          ))}
        </div>

        <div className="reports-pickergroup">
          <div className="reports-field">
            <label htmlFor="reports-start">Start date</label>
            <input
              id="reports-start"
              type="date"
              className="reports-input"
              value={start}
              aria-invalid={!rangeValid}
              aria-describedby={!rangeValid ? 'reports-date-error' : undefined}
              onChange={(e) => handleStartChange(e.target.value)}
            />
          </div>
          <span className="reports-sep" aria-hidden="true">→</span>
          <div className="reports-field">
            <label htmlFor="reports-end">End date</label>
            <input
              id="reports-end"
              type="date"
              className="reports-input"
              value={end}
              aria-invalid={!rangeValid}
              aria-describedby={!rangeValid ? 'reports-date-error' : undefined}
              onChange={(e) => handleEndChange(e.target.value)}
            />
          </div>
        </div>

        {(() => {
          if (!rangeValid) {
            return (
              <p id="reports-date-error" role="alert" className="reports-date-error">
                <WarnIcon />
                Start date must be on or before end date.
              </p>
            );
          }
          return (
            <p id="reports-date-error" className="reports-date-error" hidden>
              <WarnIcon />
              Start date must be on or before end date.
            </p>
          );
        })()}

        {rangeValid && (
          <p className="reports-range-note">
            Showing: {formatDate(start, LOCALE)} – {formatDate(end, LOCALE)}
          </p>
        )}

        {canExport && (
          <button
            type="button"
            className="reports-export"
            disabled={!rangeValid}
            onClick={handleExport}
          >
            <DownloadIcon />
            Export
          </button>
        )}
      </div>

      <div
        role="tablist"
        aria-label="Report types"
        className="reports-tabs"
        onKeyDown={handleTabsKeyDown}
      >
        {TABS.map((tab) => (
          <button
            key={tab.id}
            type="button"
            role="tab"
            id={`tab-${tab.id}`}
            className="reports-tab"
            aria-selected={activeTab === tab.id}
            aria-controls={`panel-${tab.id}`}
            tabIndex={activeTab === tab.id ? 0 : -1}
            onClick={() => activateTab(tab.id)}
          >
            {tab.label}
          </button>
        ))}
      </div>

      {TABS.map(
        (tab) =>
          mounted.has(tab.id) && (
            <div
              key={tab.id}
              role="tabpanel"
              id={`panel-${tab.id}`}
              aria-labelledby={`tab-${tab.id}`}
              tabIndex={0}
              hidden={activeTab !== tab.id}
              className="reports-panel"
            >
              {rangeValid ? (
                <tab.Comp range={{ start, end }} />
              ) : (
                <EmptyReportSummary
                  title="Invalid date range"
                  hint="Start date must be on or before end date. Adjust the range to view the report."
                />
              )}
            </div>
          )
      )}
    </main>
  );
}

export default ReportsPage;