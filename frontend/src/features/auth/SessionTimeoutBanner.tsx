/**
 * SessionTimeoutBanner — inactivity warning banner (Blueprint §8.1 / §9.1).
 *
 * Presentational sibling to `useSessionTimeout`: renders a fixed, dismissible
 * strip with a live MM:SS countdown, a primary "Continue session" action (via
 * the shared Button) and an icon-only dismiss. All timing, live-region
 * announcements (polite warning + assertive expiry), the expiry redirect, and
 * server-authoritative sliding-window enforcement live in the hook — this
 * component never redirects, never extends on its own, and renders nothing
 * while the session is healthy.
 *
 * The banner itself is intentionally NOT a live region: its countdown changes
 * every second, so a `role="status"`/`aria-live` container would re-announce
 * on every tick (screen-reader spam). The hook owns the one-shot polite warning
 * at the 60s mark and the assertive expiry via its own visually-hidden region.
 *
 * Dismissal suppresses the banner only until the final non-dismissible
 * threshold (last 15s), at which point it re-appears without a dismiss control
 * so a silent expiry cannot resurface. A transient `extend()` failure shows a
 * non-blocking inline hint while the hook keeps the countdown running; the
 * banner never shows a state the server has not confirmed.
 */
import { useCallback, useEffect, useState } from "react";
import type { ReactElement } from "react";
import { useSessionTimeout } from "../../hooks/useSessionTimeout";
import { Button } from "../../components/ui/Button";

/**
 * Stylesheet colocated with the component, injected once as a <style> element
 * (no per-element inline `style=` attributes) so it holds under a strict
 * `style-src` CSP context, matching Button/LoginPage conventions.
 */
const SESSION_BANNER_CSS = `
.stb{
  position:fixed;top:0;left:0;right:0;z-index:1000;
  background-color:#FFFBEB;border-bottom:1px solid #FCD34D;
  box-shadow:0 4px 6px -1px rgba(0,0,0,0.1),0 2px 4px -1px rgba(0,0,0,0.06);
  padding:12px 16px;
  transform:translateY(-100%);opacity:0;
  transition:transform 200ms ease-out,opacity 200ms ease-out;
}
.stb--visible{transform:translateY(0);opacity:1;}
.stb__inner{
  display:flex;align-items:center;justify-content:space-between;gap:16px;
  flex-wrap:wrap;width:100%;max-width:1280px;margin:0 auto;
}
.stb__info{display:flex;align-items:center;gap:8px;flex-wrap:wrap;}
.stb__text{
  margin:0;font-size:14px;font-weight:500;line-height:1.5;color:#1F2937;
}
.stb__count{
  font-size:16px;font-weight:700;line-height:1.5;color:#111827;
  font-variant-numeric:tabular-nums;
}
.stb__actions{display:flex;align-items:center;gap:12px;flex-wrap:wrap;}
.stb__error{
  margin:0;font-size:14px;font-weight:500;line-height:1.5;color:#991b1b;
}
.stb__dismiss{
  display:inline-flex;align-items:center;justify-content:center;
  width:44px;height:44px;min-width:44px;min-height:44px;
  flex:none;padding:0;border:none;background:transparent;
  border-radius:8px;color:#6B7280;cursor:pointer;
  transition:background-color 150ms ease-out,color 150ms ease-out;
}
.stb__dismiss:hover{background-color:#f3f4f6;color:#374151;}
.stb__dismiss:active{background-color:#e5e7eb;color:#111827;}
.stb__dismiss:focus-visible{
  outline:3px solid #4f46e5;outline-offset:2px;
}
.stb__dismiss:disabled{opacity:0.5;cursor:not-allowed;}
.stb__dismiss svg{width:20px;height:20px;}
@media (min-width:768px){
  .stb{padding:12px 24px;}
}
@media (prefers-reduced-motion:reduce){
  .stb{transition-duration:0.01ms;}
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-session-banner-styles")) {
  const style = document.createElement("style");
  style.id = "fms-session-banner-styles";
  style.textContent = SESSION_BANNER_CSS;
  document.head.appendChild(style);
}

/** Below this deadline the banner re-appears and cannot be dismissed. */
const FINAL_WARNING_THRESHOLD_MS = 15_000;
/** Solution-oriented inline hint for a transient `extend()` failure. */
const EXTEND_RETRY_COPY = "Could not extend your session — please try again.";

/** Decorative dismiss glyph — never color-only; carries a plain aria-label. */
function DismissIcon(): ReactElement {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <line x1="6" y1="6" x2="18" y2="18" />
      <line x1="18" y1="6" x2="6" y2="18" />
    </svg>
  );
}

/**
 * Format a millisecond duration as MM:SS for the countdown. Returns "--:--"
 * for a non-finite deadline (e.g. anonymous session). Padded, tabular-nums
 * output keeps digit width stable so ticks never cause layout shift.
 */
function formatRemaining(ms: number): string {
  if (!Number.isFinite(ms)) return "--:--";
  const total = Math.max(0, Math.ceil(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`;
}

export function SessionTimeoutBanner(): ReactElement {
  const { remainingMs, showWarning, isExtending, extend } = useSessionTimeout();

  const [dismissed, setDismissed] = useState(false);
  const [extendFailed, setExtendFailed] = useState(false);

  // Reset the banner's own UI state whenever the warning window closes (either
  // a successful extend or an expiry), so a fresh inactivity cycle starts clean.
  useEffect(() => {
    if (!showWarning) {
      setDismissed(false);
      setExtendFailed(false);
    }
  }, [showWarning]);

  // Re-show the banner (no dismiss) inside the final non-dismissible window so
  // a dismissed warning can never drift into silent expiry.
  useEffect(() => {
    if (showWarning && remainingMs <= FINAL_WARNING_THRESHOLD_MS) {
      setDismissed(false);
    }
  }, [showWarning, remainingMs]);

  const continueSession = useCallback(async () => {
    setExtendFailed(false);
    const res = await extend();
    // A transient failure leaves the countdown running (the hook resumes it);
    // show the inline hint only for that case, never for expired.
    if (!res.ok && !res.expired) setExtendFailed(true);
  }, [extend]);

  // Renders nothing (no reserved space, no CLS) while the session is healthy or
  // the banner was dismissed; the hook fires its own invisible announcements.
  if (!showWarning || dismissed) return null as ReactElement;

  return (
    <aside className="stb stb--visible" aria-label="Session warning">
      <div className="stb__inner">
        <div className="stb__info">
          <p className="stb__text">Your session will end soon</p>
          <span
            className="stb__count"
            aria-label={`Time remaining: ${formatRemaining(remainingMs)}`}
          >
            {formatRemaining(remainingMs)}
          </span>
        </div>

        <div className="stb__actions">
          {extendFailed ? (
            <span className="stb__error" role="status" aria-live="polite">
              {EXTEND_RETRY_COPY}
            </span>
          ) : null}

          <Button
            variant="primary"
            size="md"
            loading={isExtending}
            disabled={isExtending}
            onClick={() => {
              void continueSession();
            }}
          >
            Continue session
          </Button>

          <button
            type="button"
            className="stb__dismiss"
            onClick={() => setDismissed(true)}
            disabled={isExtending}
            aria-label="Dismiss session warning"
          >
            <DismissIcon />
          </button>
        </div>
      </div>
    </aside>
  );
}

export default SessionTimeoutBanner;