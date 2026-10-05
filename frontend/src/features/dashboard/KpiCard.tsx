/**
 * Presentational KPI card for the CFO dashboard grid (Blueprint §6/§8.1).
 * Owns only the loaded/loading states; value formatting is delegated to the
 * caller so the card stays format-agnostic. Renders exactly one interactive
 * affordance (Hick's Law) with a 44px touch target (Fitts' Law); when both
 * `href` and `onClick` are supplied, `href` wins and `onClick` is ignored
 * (documented precedence). Card container and value are inert — only the
 * explicit action is interactive, keeping the dashboard's attack surface
 * minimal (Blueprint §8.1 / Security).
 */
import React, { useEffect, useId } from 'react';
import type { MouseEvent, ReactNode } from 'react';
import Skeleton from '../../components/ui/Skeleton';

export type KpiTrend = 'up' | 'down' | 'flat';

export interface KpiPrimaryAction {
  /** Visible action label (rendered as text only). */
  label: string;
  /** Internal route/SAME-origin URL. Only safe hrefs are rendered as an <a>. */
  href?: string;
  /** Invoked only when no safe `href` is provided. */
  onClick?: (e: MouseEvent<HTMLElement>) => void;
}

export interface KpiCardProps {
  /** Short KPI caption; rendered as visible heading text. */
  label: string;
  /** Formatted figure. Real DOM text so SR users hear it. */
  value: string;
  /** Secondary caption beneath the value (e.g. "vs last month"). */
  sublabel?: string;
  /** Direction indicator — always shown with arrow + text + color, never color alone. */
  trend?: KpiTrend;
  /** Direction text paired with the arrow (e.g. "+5.2%"). */
  trendDelta?: string;
  /** Decorative inline SVG/component. Never a raw HTML/SVG string (XSS). */
  icon?: ReactNode;
  /** At most one affordance is rendered when present; zero when absent. */
  primaryAction?: KpiPrimaryAction;
  /** When true renders a CLS-safe skeleton and zero affordances. */
  loading?: boolean;
  /** Heading semantics for the label so SR users can navigate the grid by KPI. */
  level?: 1 | 2 | 3 | 4 | 5 | 6;
  /** Accessible name override for the whole card (icon-only cases). */
  ariaLabel?: string;
  className?: string;
}

const STYLE_ID = 'fms-kpicard-styles';

const FMS_KPICARD_CSS = `
.fms-kpicard {
  display:flex; flex-direction:column; gap:var(--space-3,12px);
  background:var(--color-surface,#ffffff);
  border:1px solid var(--color-border,#e2e8f0);
  border-radius:var(--radius-lg,12px);
  padding:var(--space-6,24px);
  box-shadow:var(--shadow-sm,0 1px 2px rgba(0,0,0,0.05));
  width:100%; height:100%; min-height:180px;
  box-sizing:border-box;
  transition:transform var(--transition-fast,150ms) ease-out;
}
.fms-kpicard:hover { transform:translateY(-2px); }
.fms-kpicard__header { display:flex; align-items:center; gap:var(--space-2,8px); }
.fms-kpicard__icon { flex:none; width:20px; height:20px; color:var(--color-text-tertiary,#6b7280); }
.fms-kpicard__icon svg { width:20px; height:20px; display:block; }
/* Caption-styled heading; keeps heading semantics for SR grid navigation. */
.fms-kpicard__label {
  margin:0; font-size:var(--text-sm,14px); font-weight:var(--font-medium,500);
  color:var(--color-text-secondary,#475569); text-transform:uppercase; letter-spacing:0.05em;
}
/* Hero figure — tabular numerals avoid width shift as values change; wraps long
   values at narrow viewports without horizontal overflow or CLS. */
.fms-kpicard__value {
  margin:0; font-size:var(--text-2xl,24px); font-weight:var(--font-bold,700);
  line-height:var(--leading-tight,1.25); color:var(--color-text,#1a2027);
  font-variant-numeric:tabular-nums; overflow-wrap:anywhere;
}
.fms-kpicard__sublabel {
  margin:0; font-size:var(--text-xs,12px); color:var(--color-text-secondary,#475569);
  line-height:var(--leading-normal,1.5);
}
.fms-kpicard__trend {
  display:inline-flex; align-items:center; gap:var(--space-1,4px);
  font-size:var(--text-sm,14px); font-weight:var(--font-semibold,600);
}
.fms-kpicard__trend-arrow { flex:none; font-weight:var(--font-bold,700); }
/* Direction is conveyed by arrow + delta text + color together (WCAG 1.4.1). */
.fms-kpicard__trend--up   { color:var(--color-success,#1a7f37); }
.fms-kpicard__trend--down { color:var(--color-error,#d1242f); }
.fms-kpicard__trend--flat { color:var(--color-text-secondary,#475569); }
.fms-kpicard__action {
  align-self:flex-start; margin-top:auto;
  display:inline-flex; align-items:center; justify-content:center; gap:var(--space-2,8px);
  min-height:44px; padding:var(--space-3,12px) var(--space-4,16px); box-sizing:border-box;
  border:none; border-radius:var(--radius-md,8px); background:var(--color-primary,#4f46e5);
  color:#ffffff; font-size:var(--text-sm,14px); font-weight:var(--font-semibold,600);
  line-height:var(--leading-normal,1.5); cursor:pointer; text-decoration:none; white-space:nowrap;
  transition:background-color var(--transition-fast,150ms) ease-out, transform var(--transition-fast,150ms) ease-out;
}
.fms-kpicard__action:hover { background:var(--color-primary-hover,#4338ca); }
.fms-kpicard__action:active { transform:translateY(1px); }
.fms-kpicard__action:focus-visible { outline:2px solid var(--color-focus-ring,#4f46e5); outline-offset:2px; }
.fms-kpicard__skeleton { flex:1; min-height:0; }
@media (max-width:767px) {
  .fms-kpicard { padding:var(--space-4,16px); }
}
`;

if (typeof document !== 'undefined') {
  if (!document.getElementById(STYLE_ID)) {
    const style = document.createElement('style');
    style.id = STYLE_ID;
    style.textContent = FMS_KPICARD_CSS;
    document.head.appendChild(style);
  }
}

// allowlist against script-capable schemes before rendering the card's only
// interactive surface — rejects `javascript:`, `data:`, `vbscript:`.
const ALLOWED_HREF_PROTOCOLS = ['http:', 'https:'];

/** Rejects unsafe `href` schemes; relative (/), hash (#), and query (?) are always accepted. */
function isSafeInternalHref(href: string): boolean {
  if (/^[/#?]/.test(href)) return true;
  const colon = href.indexOf(':');
  if (colon === -1) return true;
  const scheme = href.slice(0, colon).toLowerCase();
  return ALLOWED_HREF_PROTOCOLS.indexOf(scheme) !== -1;
}

const TREND_META: Record<KpiTrend, { arrow: string; cls: string; dir: string }> = {
  up: { arrow: '↑', cls: 'fms-kpicard__trend--up', dir: 'trend up' },
  down: { arrow: '↓', cls: 'fms-kpicard__trend--down', dir: 'trend down' },
  flat: { arrow: '→', cls: 'fms-kpicard__trend--flat', dir: 'trend flat' },
};

function classNames(parts: Array<string | false | undefined>): string {
  return parts.filter(Boolean).join(' ');
}

/**
 * KPI card. Presentational: owns loaded/loading states only. The parent
 * (CFODashboardPage) is responsible for the grid (1/2/3 columns), the ≤7–9
 * card cap (Miller's Law), a single shared value formatter (Gestalt
 * similarity), and never passing an undefined value (error/empty live there).
 */
export function KpiCard({
  label,
  value,
  sublabel,
  trend,
  trendDelta,
  icon,
  primaryAction,
  loading = false,
  level = 3,
  ariaLabel,
  className = '',
}: KpiCardProps): React.JSX.Element {
  const titleId = useId();
  const HeadingTag = `h${level}` as 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
  const caption = 'fms-kpicard__label';

  // Guard against raw-string icons: strings rendered as children would be plain
  // text (not an injection sink), but warn to catch markup passed by mistake.
  useEffect(() => {
    if (process.env.NODE_ENV !== 'development') return;
    if (icon !== undefined && typeof icon === 'string') {
      console.warn(
        'KpiCard icon must be a ReactNode; raw strings are not rendered to avoid XSS.',
      );
    }
  }, [icon]);

  const shellAttrs = ariaLabel
    ? { 'aria-label': ariaLabel }
    : { 'aria-labelledby': titleId };

  // Loading state: reserves the exact loaded dimensions via a fixed min-height
  // card shell with a fill-area skeleton, so the swap to content causes zero
  // reflow (CLS ≤ 0.1). Zero affordances are rendered while loading.
  if (loading) {
    return (
      <article
        className={classNames(['fms-kpicard', className])}
        {...shellAttrs}
        aria-busy="true"
        data-testid="kpicard"
      >
        <span id={titleId} className={caption}>
          {label}
        </span>
        <div className="fms-kpicard__skeleton">
          <Skeleton variant="card" announceOnLoad={false} />
        </div>
      </article>
    );
  }

  // Exactly one affordance: safe href renders an <a> (onClick ignored); with no
  // safe href, an onClick renders a <button>; with neither, zero affordances.
  const actionLabel = primaryAction?.label;
  const safeHref =
    primaryAction?.href && isSafeInternalHref(primaryAction.href)
      ? primaryAction.href
      : undefined;
  const actionNode =
    actionLabel && safeHref ? (
      <a
        className="fms-kpicard__action"
        href={safeHref}
        data-testid="kpicard-action"
      >
        {actionLabel}
      </a>
    ) : actionLabel && primaryAction?.onClick ? (
      <button
        type="button"
        className="fms-kpicard__action"
        onClick={primaryAction.onClick}
        data-testid="kpicard-action"
      >
        {actionLabel}
      </button>
    ) : null;

  const trendMeta = trend ? TREND_META[trend] : null;

  return (
    <article
      className={classNames(['fms-kpicard', className])}
      {...shellAttrs}
      aria-busy="false"
      data-testid="kpicard"
    >
      <div className="fms-kpicard__header">
        {icon ? (
          <span className="fms-kpicard__icon" aria-hidden="true">
            {icon}
          </span>
        ) : null}
        <HeadingTag id={titleId} className={caption}>
          {label}
        </HeadingTag>
      </div>

      <p className="fms-kpicard__value">{value}</p>

      {sublabel ? <p className="fms-kpicard__sublabel">{sublabel}</p> : null}

      {trendMeta ? (
        <div
          className={`fms-kpicard__trend ${trendMeta.cls}`}
          aria-label={trendMeta.dir}
          data-testid="kpicard-trend"
        >
          <span className="fms-kpicard__trend-arrow" aria-hidden="true">
            {trendMeta.arrow}
          </span>
          <span>{trendDelta ?? ''}</span>
        </div>
      ) : null}

      {actionNode}
    </article>
  );
}

export default KpiCard;