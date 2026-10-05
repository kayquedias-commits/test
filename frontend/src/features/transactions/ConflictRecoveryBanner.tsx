import { useEffect, useRef, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { createPortal } from 'react-dom';

import { Button } from '../../components/ui/Button';

/**
 * HTTP 409 optimistic-lock recovery UI. Warning-styled banner offering a
 * read-only "Reload record" refetch plus an optional polite "Dismiss".
 * Dismiss never force-saves — the server remains the 409 enforcement boundary,
 * so a dismissed banner re-returns 409 on the next conflicting save.
 */
export interface ConflictFieldDiff {
  label: string;
  before: string;
  after: string;
}

export interface ConflictDiff {
  entity: string;
  fields: ConflictFieldDiff[];
}

export type ConflictBannerMode = 'inline' | 'modal';

export interface ConflictRecoveryBannerProps {
  /** Overrides the spec copy. Untrusted text; rendered as a text node only. */
  message?: string;
  /** Inserted into the default spec copy. Untrusted text; text node only. */
  entityName?: string;
  /** Read-only refetch (React Query `refetch`). Must never trigger a mutation. */
  onRefresh: () => void;
  /** Renders inside a focused modal when 'modal', otherwise an inline banner. */
  mode?: ConflictBannerMode;
  /** Omits the Dismiss action entirely (single mandatory recovery path). */
  disableDismiss?: boolean;
  /** Called after the polite dismissal announcement, before the banner hides. */
  onDismiss?: () => void;
  /** Server-provided conflict diff summary; only allowlisted fields are shown. */
  diff?: ConflictDiff | null;
  /** True while the refetch is in flight; swaps the button label to "Reloading…". */
  loading?: boolean;
  /** True when the last reload failed; keeps the banner and re-enables Reload. */
  reloadFailed?: boolean;
}

const WARNING_GLYPH = '\u26A0\uFE0F';

const DEFAULT_MESSAGE = 'This record was modified by another user.';

const DISMISS_ANNOUNCEMENT =
  'Conflict dismissed. Your unsaved changes still conflict with another user\u2019s updates and cannot be saved until you reload.';

const RELOAD_FAILED_MESSAGE =
  'Reload failed. Check your connection and try again.';

// Client-side allowlist of financially safe, user-visible diff fields.
// Defense-in-depth: any field the server sends outside this set is dropped,
// which prevents a misconfigured payload from leaking sensitive ledger data.
const ALLOWED_DIFF_FIELDS = new Set(['description', 'date', 'category', 'amount']);

const MODAL_ROLE_ID = 'fms-conflict-modal-title';

// Warning palette (amber). --color-warning-text (#92400E) on --color-warning-bg
// (#FFFBEB) measures 7.04:1, exceeding the 4.5:1 AA floor for normal text.
const FMS_CONFLICT_CSS = `
.fms-conflict{
  display:flex; align-items:flex-start; gap:12px; width:100%;
  max-width:1280px; margin-inline:auto; padding:16px;
  border:1px solid var(--color-warning-border,#FCD34D); border-radius:8px;
  background-color:var(--color-warning-bg,#FFFBEB); color:var(--color-warning-text,#92400E);
  box-shadow:0 1px 2px rgba(0,0,0,0.05);
}
.fms-conflict:focus-visible{ outline:3px solid var(--color-primary,#4f46e5); outline-offset:2px; }
.fms-conflict__icon{ flex:none; font-size:20px; line-height:1; margin-top:2px; color:var(--color-warning-icon,#D97706); }
.fms-conflict__content{ flex:1; min-width:0; display:flex; flex-direction:column; gap:12px; }
.fms-conflict__message{ margin:0; font-size:16px; font-weight:600; line-height:1.5; max-width:65ch; }
.fms-conflict__diff{ margin-top:4px; padding-top:12px; border-top:1px solid var(--color-warning-border-soft,#FDE68A); }
.fms-conflict__diff-title{ margin:0 0 8px; font-size:14px; font-weight:600; }
.fms-conflict__diff-list{ margin:0; padding:0; list-style:none; display:flex; flex-direction:column; gap:12px; }
.fms-conflict__diff-key{ display:block; font-size:12px; font-weight:500; text-transform:capitalize; margin-bottom:4px; }
.fms-conflict__diff-values{ margin:0; display:flex; flex-direction:column; gap:4px; }
.fms-conflict__diff-pair{ display:flex; gap:8px; }
.fms-conflict__diff-side{ flex:none; width:120px; font-size:12px; font-weight:500; color:var(--color-warning-text,#92400E); opacity:0.9; }
.fms-conflict__diff-value{ margin:0; font-size:12px; font-family:ui-monospace,SFMono-Regular,Menlo,monospace; overflow-wrap:anywhere; }
.fms-conflict__reload-fail{ margin:0; font-size:14px; line-height:1.5; }
.fms-conflict__actions{ display:flex; flex-direction:column; gap:8px; margin-top:4px; }
.fms-conflict__polite{ position:absolute; width:1px; height:1px; overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; }

.fms-conflict--modal{
  border-radius:12px; padding:24px;
  box-shadow:0 10px 15px -3px rgba(0,0,0,0.1), 0 4px 6px -2px rgba(0,0,0,0.05);
  max-width:min(480px, calc(100vw - 32px));
}
.fms-conflict__overlay{
  position:fixed; inset:0; display:flex; align-items:center; justify-content:center;
  background-color:rgba(0,0,0,0.4); z-index:50; padding:16px;
}
.fms-conflict__sr-title{ position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; border:0; }

@media (min-width:768px){
  .fms-conflict{ flex-direction:row; align-items:center; }
  .fms-conflict__content{ flex-direction:row; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:16px; }
  .fms-conflict__actions{ flex-direction:row; align-items:center; gap:8px; margin-top:0; }
}
@media (prefers-reduced-motion: reduce){
  .fms-conflict,.fms-conflict__overlay,.fms-conflict--modal{ transition:none !important; }
}
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-conflict-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = FMS_CONFLICT_CSS;
    document.head.appendChild(el);
  }
}

/** Builds a copy-only view of the diff, discarding any non-allowlisted field. */
function sanitizeDiff(diff: ConflictDiff | null | undefined): ConflictDiff | null {
  if (!diff) return null;
  const fields = diff.fields.filter((field) => ALLOWED_DIFF_FIELDS.has(field.label));
  if (fields.length === 0) return null;
  return { entity: diff.entity, fields };
}

/** First enabled focusable control inside the banner, if any. */
function primaryAction(container: HTMLElement | null): HTMLElement | null {
  if (!container) return null;
  const focusables = Array.from(
    container.querySelectorAll<HTMLElement>(
      'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
    ),
  );
  return focusables.find((el) => !el.hasAttribute('disabled')) ?? null;
}

function ConflictRecoveryBannerComponent({
  message,
  entityName,
  onRefresh,
  mode = 'inline',
  disableDismiss = false,
  onDismiss,
  diff,
  loading = false,
  reloadFailed = false,
}: ConflictRecoveryBannerProps): ReactElement {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const politeRef = useRef<HTMLDivElement | null>(null);
  const triggerRef = useRef<HTMLElement | null>(null);
  const [politeText, setPoliteText] = useState('');
  const [isVisible, setIsVisible] = useState(true);

  const effectiveDiff = sanitizeDiff(diff);
  const copy =
    message ??
    (entityName
      ? `This ${entityName} was modified by another user.`
      : DEFAULT_MESSAGE);

  // Focus the banner (or, in modal mode, its primary action) on mount so
  // keyboard/screen-reader users encounter the conflict first.
  useEffect(() => {
    const node = containerRef.current;
    if (!node) return;
    triggerRef.current = document.activeElement as HTMLElement | null;
    node.focus({ preventScroll: true });
    if (mode === 'modal') {
      primaryAction(node)?.focus();
    }
  }, [mode]);

  // Modal-only focus trap, Escape-to-dismiss, scroll locking, and focus
  // restoration to the control that triggered the conflicting action.
  useEffect(() => {
    if (mode !== 'modal' || !isVisible) return undefined;

    const node = containerRef.current;
    if (!node) return undefined;

    const body = document.body;
    const previousOverflow = body.style.overflow;
    body.style.overflow = 'hidden';

    const handleKeyDown = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') {
        event.preventDefault();
        handleDismiss();
        return;
      }
      if (event.key !== 'Tab') return;
      const list = Array.from(
        node.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        ),
      ).filter((el) => !el.hasAttribute('disabled'));
      if (list.length === 0) return;
      const first = list[0];
      const last = list[list.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (event.shiftKey) {
        if (active === first || !node.contains(active)) {
          event.preventDefault();
          last.focus();
        }
      } else if (active === last || !node.contains(active)) {
        event.preventDefault();
        first.focus();
      }
    };
    node.addEventListener('keydown', handleKeyDown);

    return () => {
      node.removeEventListener('keydown', handleKeyDown);
      body.style.overflow = previousOverflow;
      triggerRef.current?.focus();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, isVisible]);

  // Dismiss only hides the banner; it never reconciles by overwriting. The
  // next conflicting save re-returns 409 from the server.
  const handleDismiss = (): void => {
    setPoliteText(DISMISS_ANNOUNCEMENT);
    onDismiss?.();
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setIsVisible(false);
      return;
    }
    setTimeout(() => setIsVisible(false), 150);
  };

  // The Button itself suppresses clicks while loading; guard is belt-and-braces.
  const handleRefresh = (): void => {
    if (!loading) onRefresh();
  };

  const renderDiff: ReactNode | null = effectiveDiff ? (
    <section className="fms-conflict__diff" role="group" aria-label="Changed fields">
      <h2 className="fms-conflict__diff-title">Changed fields</h2>
      <ul className="fms-conflict__diff-list">
        {effectiveDiff.fields.map((field) => (
          <li className="fms-conflict__diff-item" key={field.label}>
            <span className="fms-conflict__diff-key">{field.label}</span>
            <dl className="fms-conflict__diff-values">
              <div className="fms-conflict__diff-pair">
                <dt className="fms-conflict__diff-side">Your version</dt>
                <dd className="fms-conflict__diff-value">{field.before}</dd>
              </div>
              <div className="fms-conflict__diff-pair">
                <dt className="fms-conflict__diff-side">Current version</dt>
                <dd className="fms-conflict__diff-value">{field.after}</dd>
              </div>
            </dl>
          </li>
        ))}
      </ul>
    </section>
  ) : null;

  const actions = (
    <div className="fms-conflict__actions">
      <Button variant="primary" loading={loading} onClick={handleRefresh}>
        {loading ? 'Reloading\u2026' : 'Reload record'}
      </Button>
      {!disableDismiss && (
        <Button variant="ghost" onClick={handleDismiss}>
          Dismiss
        </Button>
      )}
    </div>
  );

  const bannerContent = (
    <>
      <span className="fms-conflict__icon" aria-hidden="true">
        {WARNING_GLYPH}
      </span>
      <div className="fms-conflict__content">
        <p className="fms-conflict__message">{copy}</p>
        {renderDiff}
        {reloadFailed && (
          <p className="fms-conflict__reload-fail">{RELOAD_FAILED_MESSAGE}</p>
        )}
        {actions}
      </div>
    </>
  );

  let banner: ReactElement;
  if (mode === 'modal') {
    banner = (
      <div
        ref={containerRef}
        className="fms-conflict fms-conflict--modal"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={MODAL_ROLE_ID}
        tabIndex={-1}
      >
        <h2 id={MODAL_ROLE_ID} className="fms-conflict__sr-title">
          Record conflict
        </h2>
        {bannerContent}
      </div>
    );
  } else {
    banner = (
      <div
        ref={containerRef}
        className="fms-conflict fms-conflict--inline"
        role="alert"
        aria-atomic="true"
        tabIndex={-1}
      >
        {bannerContent}
      </div>
    );
  }

  if (!isVisible) return <></>;

  // Polite region is a persistent sibling of the assertive banner, never
  // nested inside it, so removing the alert node is still announced.
  const polite = (
    <div
      ref={politeRef}
      className="fms-conflict__polite"
      aria-live="polite"
      aria-atomic="true"
    >
      {politeText}
    </div>
  );

  if (mode === 'modal') {
    return (
      <>
        {createPortal(
          <div className="fms-conflict__overlay">{banner}</div>,
          document.getElementById('root') ?? document.body,
        )}
        {polite}
      </>
    );
  }

  return (
    <>
      {banner}
      {polite}
    </>
  );
}

export default ConflictRecoveryBannerComponent;
export { ConflictRecoveryBannerComponent as ConflictRecoveryBanner };
export type { ConflictFieldDiff, ConflictDiff, ConflictBannerMode };