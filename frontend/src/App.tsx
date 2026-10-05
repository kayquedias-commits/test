/**
 * Application composition root (Blueprint §6 / §8.1 / §8.3).
 *
 * Holds no business logic (SRP) — only wiring of the cross-cutting shell
 * concerns so feature pages never reimplement them:
 *   - a top-level ErrorBoundary that catches render/timeout failures from the
 *     whole router subtree and maps them to a recoverable UI (never a blank
 *     screen or stack trace),
 *   - the global toast channel (two permanently mounted aria-live regions),
 *   - the session-timeout warning banner (auth lifecycle),
 *   - the router itself, mounted via RouterProvider.
 *
 * AppShell (rendered inside the router) owns the skip-link, `<main
 * id="main-content">`, primary navigation and footer landmarks, so this file
 * adds none of those — preserving the "exactly one <h1> per active route"
 * invariant.
 */
import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";
import { RouterProvider } from "react-router-dom";

import { router } from "./app/router";
import { Toast } from "./components/ui/Toast";
import { SessionTimeoutBanner } from "./features/auth/SessionTimeoutBanner";

/**
 * Colocated, CSP-safe stylesheet surfaced once (no per-element inline `style=`
 * attributes, no inline handlers), matching the router/Toast/SessionTimeoutBanner
 * convention. Only opacity is animated — no layout properties (CLS ≤ 0.1).
 */
const APP_ERROR_CSS = `
.app-error{
  display:flex;align-items:center;justify-content:center;
  min-height:100vh;width:100%;box-sizing:border-box;
  padding:24px;background-color:var(--color-bg, #f8fafc);
}
.app-error__card{
  width:100%;max-width:480px;padding:32px;box-sizing:border-box;
  background-color:var(--color-surface, #ffffff);
  border:1px solid var(--color-border, #e2e8f0);
  border-radius:var(--radius-lg, 12px);
  box-shadow:var(--shadow-lg, 0 10px 15px -3px rgba(0,0,0,0.1), 0 4px 6px -2px rgba(0,0,0,0.05));
  display:flex;flex-direction:column;gap:16px;align-items:flex-start;
  animation:app-error-in 200ms ease-out;
}
@keyframes app-error-in{from{opacity:0;}to{opacity:1;}}
.app-error__icon{
  width:32px;height:32px;color:var(--color-danger-strong, #b91c1c);
}
.app-error__title{
  margin:0;font-size:var(--text-2xl, 24px);font-weight:var(--font-bold, 700);
  line-height:var(--leading-tight, 1.25);color:var(--color-text, #111827);
}
.app-error__body{
  margin:0;font-size:var(--text-base, 16px);font-weight:var(--font-normal, 400);
  line-height:var(--leading-normal, 1.5);color:var(--color-text-secondary, #4b5563);
  max-width:65ch;
}
.app-error__actions{display:flex;flex-wrap:wrap;gap:12px;margin-top:8px;}
.app-error__btn{
  display:inline-flex;align-items:center;justify-content:center;
  min-height:44px;min-width:44px;padding:10px 24px;box-sizing:border-box;
  font-size:var(--text-base, 16px);font-weight:var(--font-semibold, 600);
  border-radius:var(--radius-md, 8px);border:none;cursor:pointer;
  transition:opacity 150ms ease-out;
}
.app-error__btn--primary{
  background-color:var(--color-danger, #dc2626);color:#ffffff;
}
.app-error__btn--primary:hover{background-color:var(--color-danger-strong, #b91c1c);}
.app-error__btn--ghost{
  background-color:transparent;color:var(--color-text, #111827);
  border:1px solid var(--color-border, #d1d5db);
}
.app-error__btn--ghost:hover{background-color:var(--color-bg, #f8fafc);}
.app-error__btn:disabled{opacity:0.6;cursor:not-allowed;}
.app-error__btn:focus-visible{
  outline:2px solid var(--color-text, #111827);outline-offset:2px;
}
@media (max-width:767px){
  .app-error__card{padding:24px;}
  .app-error__title{font-size:var(--text-xl, 20px);}
  .app-error__btn{flex:1 1 100%;}
}
@media (prefers-reduced-motion:reduce){
  .app-error__card{animation:none;}
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-app-error-styles")) {
  const style = document.createElement("style");
  style.id = "fms-app-error-styles";
  style.textContent = APP_ERROR_CSS;
  document.head.appendChild(style);
}

interface AppErrorBoundaryProps {
  children: ReactNode;
}

interface AppErrorBoundaryState {
  hasError: boolean;
}

/**
 * Top-level error boundary: catches render errors thrown anywhere in the router
 * subtree so the app never unmounts to a blank screen. The real error is logged
 * for support only — never rendered into the DOM (no stack-trace / internal-state
 * disclosure, and nothing an attacker can reflect). Two recoveries are offered:
 * Retry (re-renders the tree) and Reload application (fatal/boot path). Focus is
 * moved to the recovery region on mount and restored to main content on retry.
 */
class AppErrorBoundary extends Component<AppErrorBoundaryProps, AppErrorBoundaryState> {
  state: AppErrorBoundaryState = { hasError: false };

  private containerRef: HTMLMainElement | null = null;

  static getDerivedStateFromError(): AppErrorBoundaryState {
    // Set synchronously on catch — the recovery UI paints without a spinner
    // (Doherty's Law: visible feedback < 100 ms).
    return { hasError: true };
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo): void {
    // Diagnostic telemetry only; the thrown value never reaches the DOM.
    console.error("FMS application render failed", error, errorInfo);
  }

  componentDidUpdate(
    _prevProps: AppErrorBoundaryProps,
    prevState: AppErrorBoundaryState,
  ): void {
    // Focus is managed strictly on the hasError state transition — never on an
    // incidental re-render, which would steal focus from the user.
    if (!prevState.hasError && this.state.hasError) {
      // Recovery UI just mounted: move focus to the alert region.
      this.containerRef?.focus();
    } else if (prevState.hasError && !this.state.hasError) {
      // Retry succeeded: return focus to the app's main content landmark.
      const main = document.getElementById("main-content");
      (main as HTMLElement | null)?.focus();
    }
  }

  private handleRetry = (): void => {
    // Reset the error state to re-render the router subtree (recoverable errors).
    this.setState({ hasError: false });
  };

  setContainerRef = (node: HTMLMainElement | null): void => {
    this.containerRef = node;
  };

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <main
          className="app-error"
          role="alert"
          aria-live="assertive"
          tabIndex={-1}
          ref={this.setContainerRef}
        >
          <div className="app-error__card">
            <svg
              className="app-error__icon"
              viewBox="0 0 24 24"
              aria-hidden="true"
              focusable="false"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
              <line x1="12" y1="9" x2="12" y2="13" />
              <line x1="12" y1="17" x2="12.01" y2="17" />
            </svg>
            <h1 className="app-error__title">We couldn&apos;t load the app</h1>
            <p className="app-error__body">
              Something went wrong while starting the Financial Management
              system. Your data is safe. Please reload to continue.
            </p>
            <div className="app-error__actions">
              <button
                type="button"
                className="app-error__btn app-error__btn--primary"
                onClick={this.handleRetry}
              >
                Retry
              </button>
              <button
                type="button"
                className="app-error__btn app-error__btn--ghost"
                onClick={() => window.location.reload()}
              >
                Reload application
              </button>
            </div>
          </div>
        </main>
      );
    }

    return this.props.children;
  }
}

/**
 * App — composition root. Mounts the session banner, the toast channel, and the
 * router inside the top-level error boundary. Holds no business logic.
 */
export function App(): ReactNode {
  return (
    <AppErrorBoundary>
      <SessionTimeoutBanner />
      <Toast />
      <RouterProvider router={router} />
    </AppErrorBoundary>
  );
}

export default App;