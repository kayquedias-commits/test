import React, { useEffect } from "react";
import { useBudgetStore } from "../../app/stores/budget";
import { formatMinorUnits, subtractMoney } from "../../utils/currency";

/*
 * Budget vs-actual progress card (Blueprint §8.1 / Module D).
 *
 * Presentational + derived-store subscription: it reads the budget-progress projection
 * from `useBudgetStore.progressFor(id)` so actuals recompute the instant the underlying
 * transactions change, and (cold cache only) triggers the authoritative `GET
 * /v1/budgets/{id}/vs-actual` warm-up via `refreshActuals`. The card never writes or
 * mutates anything, so it adds no CSRF surface.
 *
 * Amounts pass through `currency.ts` minor-unit formatters only (never raw string
 * concatenation rendered as HTML), and every user/backend-derived string is rendered
 * as React text children — no `dangerouslySetInnerHTML`, no inline handlers.
 */

type Stage = "within" | "approaching" | "overrun";

interface StatusMeta {
  icon: React.ReactNode;
  text: string;
  at: string;
  fill: string;
  soft: string;
  fg: string;
  strong: string;
}

// Status triplet is always icon + text + color (WCAG 1.4.1 — color is never the sole
// indicator). The -soft backgrounds are ~12% semantic tints over the surface so the
// dark semantic chip text holds >=4.5:1.
const STATUS_META: Record<Stage, StatusMeta> = {
  within: {
    fill: "#059669",
    soft: "rgba(5, 150, 105, 0.12)",
    fg: "#047857",
    strong: "#047857",
    icon: <CheckIcon />,
    text: "On track",
    at: "Within budget",
  },
  approaching: {
    fill: "#d97706",
    soft: "rgba(217, 119, 6, 0.12)",
    fg: "#92400e",
    strong: "#b45309",
    icon: <ApproachIcon />,
    text: "Approaching limit",
    at: "Approaching limit",
  },
  overrun: {
    fill: "#dc2626",
    soft: "rgba(220, 38, 38, 0.12)",
    fg: "#991b1b",
    strong: "#b91c1c",
    icon: <OverrunIcon />,
    text: "Over budget",
    at: "Over budget",
  },
};

const STYLE_ID = "fms-bvacard-styles";

function injectStyles(): void {
  if (typeof document === "undefined") return;
  if (document.getElementById(STYLE_ID)) return;
  const style = document.createElement("style");
  style.id = STYLE_ID;
  style.textContent = `
  .bva {
    display: flex;
    flex-direction: column;
    gap: var(--space-3, 12px);
    padding: var(--space-4, 16px);
    background: var(--color-surface, #ffffff);
    border: 1px solid var(--color-border, #e2e8f0);
    border-radius: var(--radius-lg, 12px);
    box-shadow: var(--shadow-sm, 0 1px 2px rgba(0,0,0,0.05));
    max-width: 480px;
  }
  .bva__head {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  .bva__label {
    font-size: var(--text-sm, 14px);
    font-weight: var(--font-medium, 500);
    letter-spacing: 0.05em;
    text-transform: uppercase;
    color: var(--color-text-muted, #64748b);
  }
  .bva__chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1, 4px);
    padding: var(--space-1, 4px) var(--space-3, 12px);
    border-radius: var(--radius-full, 9999px);
    background: var(--bva-soft, rgba(5, 150, 105, 0.12));
    color: var(--bva-fg, #047857);
    font-size: var(--text-xs, 12px);
    font-weight: var(--font-semibold, 600);
    letter-spacing: 0.05em;
    text-transform: uppercase;
    transition: background-color var(--transition-fast, 150ms ease-out),
      color var(--transition-fast, 150ms ease-out);
  }
  .bva__chip svg {
    flex: none;
    color: var(--bva-strong, #047857);
  }
  .bva__amounts {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-2, 8px);
    margin-top: var(--space-3, 12px);
  }
  .bva__actual {
    font-size: var(--text-2xl, 24px);
    font-weight: var(--font-semibold, 600);
    line-height: 1.25;
    color: var(--color-text-primary, #1a202c);
    font-variant-numeric: tabular-nums;
  }
  .bva__alloc {
    font-size: var(--text-sm, 14px);
    font-weight: var(--font-normal, 400);
    color: var(--color-text-muted, #64748b);
    font-variant-numeric: tabular-nums;
  }
  .bva__progress {
    display: flex;
    align-items: center;
    gap: var(--space-3, 12px);
    margin-top: var(--space-2, 8px);
  }
  .bva__bar {
    flex: 1 1 auto;
    min-width: 0;
  }
  .bva__track {
    position: relative;
    height: 8px;
    border-radius: var(--radius-full, 9999px);
    background: var(--color-border, #e2e8f0);
    overflow: hidden;
  }
  .bva__fill {
    position: absolute;
    inset: 0;
    transform-origin: left center;
    transform: scaleX(var(--bva-pct, 0));
    border-radius: var(--radius-full, 9999px);
    background: var(--bva-fill, #059669);
    transition: transform var(--transition-base, 200ms ease-out),
      background-color var(--transition-fast, 150ms ease-out);
  }
  .bva__pct {
    flex: none;
    font-size: var(--text-sm, 14px);
    font-weight: var(--font-medium, 500);
    color: var(--bva-fill, #059669);
    font-variant-numeric: tabular-nums;
  }
  .bva__caption {
    font-size: var(--text-xs, 12px);
    color: var(--color-text-muted, #64748b);
  }
  .bva__retry {
    min-height: 44px;
    padding: var(--space-2, 8px) var(--space-4, 16px);
    border: 1px solid var(--color-border, #e2e8f0);
    border-radius: var(--radius-md, 8px);
    background: var(--color-surface, #ffffff);
    color: var(--color-text-primary, #1a202c);
    font-size: var(--text-sm, 14px);
    font-weight: var(--font-medium, 500);
    cursor: pointer;
    transition: background-color var(--transition-fast, 150ms ease-out);
  }
  .bva__retry:hover { background: #f1f5f9; }
  .bva__retry:focus-visible {
    outline: 2px solid var(--bva-fill, #059669);
    outline-offset: 2px;
  }
  .bva__visually-hidden {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
    white-space: nowrap;
    border: 0;
  }
  /* Skeleton placeholder reserves the exact final layout (no CLS). */
  .bva__sk {
    border-radius: var(--radius-sm, 4px);
    background: #eef2f7;
    position: relative;
    overflow: hidden;
  }
  .bva__sk::after {
    content: "";
    position: absolute;
    inset: 0;
    transform: translateX(-100%);
    background: linear-gradient(90deg, transparent, rgba(255,255,255,0.6), transparent);
    animation: bva__shimmer 1.4s ease-in-out infinite;
  }
  .bva__sk-head { width: 40%; height: 14px; }
  .bva__sk-actual { width: 45%; height: 24px; margin-top: var(--space-3, 12px); }
  .bva__sk-track { height: 8px; margin-top: var(--space-2, 8px); }

  @keyframes bva__shimmer {
    100% { transform: translateX(100%); }
  }

  @media (prefers-reduced-motion: reduce) {
    .bva__fill { transition: none; }
    .bva__chip { transition: none; }
    .bva__sk::after { animation: none; }
  }

  @media (max-width: 767px) {
    .bva { padding: var(--space-3, 12px); }
    .bva__progress { flex-direction: column; align-items: stretch; }
    .bva__pct { text-align: right; }
  }
  `;
  document.head.appendChild(style);
}
injectStyles();

function CheckIcon(): React.ReactElement {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d="M3 8.5 L6.5 12 L13 4.5"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function ApproachIcon(): React.ReactElement {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path d="M8 2 L15 14 L1 14 Z" fill="currentColor" />
      <path d="M8 6 V10" stroke="#fff" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

function OverrunIcon(): React.ReactElement {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="8" cy="8" r="7" fill="currentColor" />
      <path d="M8 4.5 V9" stroke="#fff" strokeWidth="1.6" strokeLinecap="round" />
      <circle cx="8" cy="11.5" r="1" fill="#fff" />
    </svg>
  );
}

function Chip({ meta, testId }: { meta: StatusMeta; testId?: string }): React.ReactElement {
  return (
    <span
      className="bva__chip"
      data-testid={testId}
      style={{
        ["--bva-soft" as string]: meta.soft,
        ["--bva-fg" as string]: meta.fg,
        ["--bva-strong" as string]: meta.strong,
      }}
    >
      {meta.icon}
      {meta.text}
    </span>
  );
}

interface BudgetVsActualCardProps {
  /** Structural id only — all other data comes from the budget-progress store. */
  budget: { id: string };
  /** Optional card heading; falls back to "Budget". Used for the progressbar aria-label. */
  label?: string;
  className?: string;
}

export function BudgetVsActualCard({ budget, label = "Budget", className }: BudgetVsActualCardProps): React.ReactElement | null {
  const progress = useBudgetStore((s) => s.progressFor(budget.id));

  // Warm the authoritative vs-actual read on mount when the cache is cold. Strictly a GET
  // (no CSRF token); the store keeps last-known-good derived data on failure.
  useEffect(() => {
    void useBudgetStore.getState().refreshActuals(budget.id);
  }, [budget.id]);

  const heading = label;

  // Unloaded: show an exact-layout skeleton, never a blank or misleading bar.
  if (!progress.loaded) {
    return (
      <div className={`bva ${className ?? ""}`} data-testid="bva-card">
        <div className="bva__head">
          <div className="bva__label">{heading}</div>
        </div>
        <div className="bva__sk bva__sk-actual" />
        <div className="bva__track bva__sk bva__sk-track" />
        <span className="bva__visually-hidden" role="status">Loading budget progress</span>
      </div>
    );
  }

  // No allocation: never divide; show an explicit neutral state instead of a bar.
  if (progress.state === "no-allocation") {
    return (
      <div className={`bva ${className ?? ""}`} data-testid="bva-card">
        <div className="bva__head">
          <div className="bva__label">{heading}</div>
        </div>
        <div className="bva__amounts">
          <span className="bva__actual">—</span>
        </div>
        <div className="bva__progress">
          <div
            className="bva__bar bva__track"
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={0}
            aria-label={`Budget progress for ${heading}`}
            data-testid="bva-progress"
            style={{ ["--bva-pct" as string]: "0" } as React.CSSProperties}
          >
            <div className="bva__fill" style={{ ["--bva-fill" as string]: STATUS_META.within.fill } as React.CSSProperties} />
          </div>
          <div className="bva__pct" aria-live="polite">0%</div>
        </div>
        <div className="bva__caption" data-testid="bva-no-allocation">No budget set</div>
      </div>
    );
  }

  const meta = STATUS_META[progress.state as Stage];
  const spent = progress.spent;
  const allocated = progress.allocated;

  // Division-by-zero is guarded in the store (pct null => no-allocation above). pct may exceed
  // 100 on overrun; aria/fill clamp to [0,100] while the label keeps the true value.
  const pct = progress.pct ?? 0;
  const ariaPct = Math.min(100, Math.max(0, pct));
  const pctText = `${Math.round(pct)}%`;

  const spentText = spent ? formatMinorUnits(spent.amount, spent.currency) : "—";
  const allocText = allocated ? formatMinorUnits(allocated.amount, allocated.currency) : "—";

  // Overrun only: spent > allocated, so the delta is positive minor units.
  const overBy =
    progress.state === "overrun" && spent && allocated
      ? formatMinorUnits(subtractMoney(spent.amount, allocated.amount), spent.currency)
      : null;

  const atText =
    progress.state === "overrun"
      ? overBy
        ? `Over budget by ${overBy}`
        : "Over budget"
      : progress.state === "approaching"
        ? `${Math.round(pct)}% of budget used`
        : progress.transactionCount > 0
          ? `${Math.round(pct)}% of budget used`
          : "No spending yet";

  return (
    <div className={`bva ${className ?? ""}`} data-testid="bva-card">
      <div className="bva__head">
        <div className="bva__label">{heading}</div>
        <Chip meta={meta} testId="bva-chip" />
      </div>

      <div className="bva__amounts">
        <span className="bva__actual" data-testid="bva-spent">{spentText}</span>
        <span className="bva__alloc" data-testid="bva-allocated">of {allocText}</span>
      </div>

      <div className="bva__progress">
        <div
          className="bva__bar bva__track"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(ariaPct)}
          aria-label={`Budget progress for ${heading}`}
          data-testid="bva-progress"
          style={{ ["--bva-pct" as string]: String(ariaPct / 100) } as React.CSSProperties}
        >
          <div
            className="bva__fill"
            style={{ ["--bva-fill" as string]: meta.fill } as React.CSSProperties}
          />
        </div>
        <div className="bva__pct" aria-live="polite" data-testid="bva-pct">{pctText}</div>
      </div>

      {progress.transactionCount === 0 && (
        <div className="bva__caption" data-testid="bva-empty">No spending yet</div>
      )}

      {progress.refreshFailed && (
        <div className="bva__caption">
          Couldn&apos;t refresh from server.
          <button
            type="button"
            className="bva__retry"
            data-testid="bva-retry"
            onClick={() => void useBudgetStore.getState().refreshActuals(budget.id)}
          >
            Retry
          </button>
        </div>
      )}

      <span className="bva__visually-hidden" aria-live="polite" data-testid="bva-at-text">{atText}</span>
    </div>
  );
}

export default BudgetVsActualCard;