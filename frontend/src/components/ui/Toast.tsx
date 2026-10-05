import type { ReactElement, ReactNode } from 'react';
import { useNotifications, selectPolite, selectAssertive } from '../../app/stores/notifications';
import type { Notification } from '../../app/stores/notifications';

/**
 * Accessible toast host for the aria-live messaging channel (Blueprint §8.3).
 * Pure presentation over the notifications store: it subscribes to the polite
 * and assertive regions and renders their contents. It owns no timers or
 * business logic — the store handles TTL, pause/resume, FIFO cap and text
 * sanitization. Rendered once from App.tsx.
 *
 * ARIA contract follows the accessibility analysis: each container declares
 * only `aria-live`; `aria-atomic` sits on the individual toast (never the
 * container) so a single arrival/departure does not re-announce the whole
 * remaining stack. Polite toasts use `role="status"`; assertive toasts omit
 * that role to avoid a double announcement against the assertive container.
 */

const toastStylesId = 'fms-toast-styles';

const FMS_TOAST_CSS = `
.fms-toast__region {
  position: fixed;
  top: var(--space-6, 24px);
  right: var(--space-6, 24px);
  z-index: 1000;
  display: flex;
  flex-direction: column;
  gap: var(--space-3, 12px);
  width: min(360px, calc(100vw - 32px));
  box-sizing: border-box;
  pointer-events: none;
}
/* Empty regions stay in the DOM (so the live region stays registered to SRs)
   but are clipped out visually — never display:none. */
.fms-toast__region[data-count="0"] {
  position: absolute;
  width: 1px;
  height: 1px;
  clip-path: inset(50%);
  overflow: hidden;
  white-space: nowrap;
  z-index: -1;
}
.fms-toast {
  pointer-events: auto;
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #d0d7de);
  border-radius: var(--radius-lg, 12px);
  box-shadow: var(--shadow-lg, 0 10px 15px -3px rgba(0,0,0,0.1), 0 4px 6px -2px rgba(0,0,0,0.05));
  padding: var(--space-4, 16px);
  display: flex;
  align-items: flex-start;
  gap: var(--space-3, 12px);
  width: 360px;
  box-sizing: border-box;
  animation: fms-toast-in 200ms var(--transition-base, ease-out);
}
@keyframes fms-toast-in {
  from { opacity: 0; transform: translateY(-8px); }
  to   { opacity: 1; transform: translateY(0); }
}
.fms-toast__icon {
  flex: none;
  width: 32px;
  height: 32px;
  border-radius: var(--radius-md, 8px);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--color-text-inverse, #ffffff);
}
.fms-toast[data-severity="success"] .fms-toast__icon {
  background: var(--color-success-soft, #dafbe1);
  color: var(--color-success, #1a7f37);
}
.fms-toast[data-severity="error"] .fms-toast__icon {
  background: var(--color-error-soft, #ffebe9);
  color: var(--color-error, #d1242f);
}
.fms-toast[data-severity="warning"] .fms-toast__icon {
  background: var(--color-warning-soft, #fff8c5);
  color: var(--color-warning, #9a6700);
}
.fms-toast[data-severity="info"] .fms-toast__icon {
  background: var(--color-info-soft, #ddf4ff);
  color: var(--color-info, #0969da);
}
.fms-toast__content {
  flex: 1;
  min-width: 0;
}
.fms-toast__title {
  margin: 0;
  font-size: var(--text-base, 16px);
  font-weight: var(--font-semibold, 600);
  line-height: var(--leading-tight, 1.25);
  color: var(--color-text-primary, #1f2937);
}
.fms-toast__body {
  margin: var(--space-2, 8px) 0 0;
  font-size: var(--text-sm, 14px);
  font-weight: var(--font-normal, 400);
  line-height: var(--leading-normal, 1.5);
  color: var(--color-text-secondary, #374151);
  max-width: 280px;
}
.fms-toast__dismiss {
  flex: none;
  width: 44px;
  height: 44px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: none;
  background: transparent;
  color: var(--color-text-tertiary, #6b7280);
  cursor: pointer;
  border-radius: var(--radius-sm, 4px);
  transition: color var(--transition-fast, 150ms) ease-out;
}
.fms-toast__dismiss:hover {
  color: var(--color-text-primary, #1f2937);
}
.fms-toast__dismiss:focus-visible {
  outline: 2px solid var(--color-info, #0969da);
  outline-offset: 2px;
}
@media (max-width: 767px) {
  .fms-toast__region {
    left: var(--space-4, 16px);
    right: var(--space-4, 16px);
    width: auto;
    gap: var(--space-2, 8px);
  }
  .fms-toast {
    width: auto;
    max-width: calc(100vw - 32px);
  }
}
@media (prefers-reduced-motion: reduce) {
  .fms-toast {
    animation: none;
    transition: none;
  }
}
`;

/**
 * Icon set keyed by a fixed severity allowlist. SVG payloads are static,
 * developer-authored markup only — never derived from or interpolated with
 * external input. All glyphs are decorative: aria-hidden, meaning carried by
 * the contiguous title/body text.
 */
const ICON_GLYPHS: Record<Notification['severity'], ReactElement> = {
  success: (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 6 9 17l-5-5" />
    </svg>
  ),
  error: (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
  ),
  warning: (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
  ),
  info: (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="10" />
      <line x1="12" y1="16" x2="12" y2="12" />
      <line x1="12" y1="8" x2="12.01" y2="8" />
    </svg>
  ),
};

const X_GLYPH: ReactElement = (
  <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <line x1="18" y1="6" x2="6" y2="18" />
    <line x1="6" y1="6" x2="18" y2="18" />
  </svg>
);

function injectToastStyles(): void {
  if (typeof document === 'undefined' || document.getElementById(toastStylesId)) return;
  const style = document.createElement('style');
  style.id = toastStylesId;
  style.textContent = FMS_TOAST_CSS;
  document.head.appendChild(style);
}

function renderToast(n: Notification, index: number): ReactNode {
  return (
    <div
      key={n.id}
      role={n.live === 'polite' ? 'status' : undefined}
      aria-atomic="true"
      data-severity={n.severity}
      data-index={index}
      className="fms-toast"
    >
      <span className="fms-toast__icon" aria-hidden="true">
        {ICON_GLYPHS[n.severity]}
      </span>
      <div className="fms-toast__content">
        <p className="fms-toast__title">{n.title}</p>
        {n.body ? <p className="fms-toast__body">{n.body}</p> : null}
      </div>
      {n.dismissible ? (
        <button
          type="button"
          className="fms-toast__dismiss"
          aria-label={`Dismiss notification: ${n.title}`}
          onClick={() => useNotifications.getState().dismiss(n.id)}
          onPointerEnter={() => useNotifications.getState().pause(n.id)}
          onPointerLeave={() => useNotifications.getState().resume(n.id)}
          onFocus={() => useNotifications.getState().pause(n.id)}
          onBlur={() => useNotifications.getState().resume(n.id)}
        >
          {X_GLYPH}
        </button>
      ) : null}
    </div>
  );
}

/** Toast host. Two permanently mounted aria-live regions (polite, assertive). */
export function Toast(): ReactElement | null {
  injectToastStyles();

  const polite = useNotifications(selectPolite);
  const assertive = useNotifications(selectAssertive);

  return (
    <>
      <div
        aria-live="polite"
        className="fms-toast__region fms-toast__region--polite"
        data-count={polite.length}
      >
        {polite.map((n, i) => renderToast(n, i))}
      </div>
      <div
        aria-live="assertive"
        className="fms-toast__region fms-toast__region--assertive"
        data-count={assertive.length}
      >
        {assertive.map((n, i) => renderToast(n, i))}
      </div>
    </>
  );
}

export default Toast;