/**
 * AppShell — semantic application frame (Blueprint §8.3).
 *
 * Renders the shared landmarks every route mounts inside: skip-to-content link,
 * top session-warning slot, <header> with brand + user menu, the composed
 * <nav aria-label="Primary"> (Navigation is permission-filtered and manages its
 * own responsive disclosure), <main id="main-content"> for route children, and
 * a minimal <footer>. No <h1> is authored here — pages own the single heading.
 *
 * The shell is deliberately thin: Navigation owns the primary nav (its mobile
 * toggle, focus return, Escape/outside-click close) and SessionTimeoutBanner owns
 * its fixed overlay + CLS behavior. This file only frames them and provides the
 * responsive grid. Header role/name are accepted as optional props because the
 * session store is not part of this file's import contract; when absent the
 * badge is omitted.
 */
import type { ReactNode } from "react";
import { Navigation } from "./Navigation";
import { SessionTimeoutBanner } from "../../features/auth/SessionTimeoutBanner";
import { useSessionTimeout } from "../../hooks/useSessionTimeout";

export interface AppShellProps {
  /** Outlet-style route content rendered inside <main>. */
  children: ReactNode;
  /** Current session role for the header badge; rendered as a text node only. */
  role?: string | null;
  /** Display name for the header user menu when no role is available. */
  userName?: string | null;
}

/**
 * Colocated styles injected once (CSP-safe <style>, no inline style= attrs /
 * inline handlers) — matches Navigation/SessionTimeoutBanner conventions. Per
 * the security review: state-driven classes only, transform/opacity animations.
 */
const APP_SHELL_CSS = `
.app-shell{
  display:flex;flex-direction:column;min-height:100vh;
  background:var(--color-surface,#ffffff);
  color:var(--color-text,#0f172a);
  font-family:var(--font-family,system-ui,-apple-system,"Segoe UI",sans-serif);
}
.app-shell__header{
  display:flex;align-items:center;justify-content:space-between;
  gap:var(--space-6,24px);min-height:56px;
  padding:var(--space-3,12px) var(--space-4,16px);
  background:var(--color-surface,#ffffff);
  border-bottom:1px solid var(--color-border,#e2e8f0);
  box-shadow:var(--shadow-sm,0 1px 2px rgba(0,0,0,0.05));
}
.app-shell__brand{
  font-size:var(--text-lg,18px);font-weight:var(--font-semibold,600);
  color:var(--color-text,#0f172a);line-height:1.25;min-height:44px;
  display:flex;align-items:center;white-space:nowrap;
}
.app-shell__user{display:flex;align-items:center;gap:var(--space-3,12px);min-height:44px;}
.app-shell__badge{
  font-size:var(--text-xs,12px);font-weight:var(--font-normal,400);
  color:var(--color-text-secondary,#475569);
  background:var(--color-surface-2,#f8fafc);
  border:1px solid var(--color-border,#e2e8f0);
  border-radius:var(--radius-full,9999px);
  padding:var(--space-1,4px) var(--space-3,12px);
  white-space:nowrap;
}
.app-shell__logout{
  min-width:44px;min-height:44px;
  padding:var(--space-2,8px) var(--space-4,16px);
  border:0;border-radius:var(--radius-md,8px);
  background:transparent;color:var(--color-text-secondary,#475569);
  font-size:var(--text-sm,14px);font-family:inherit;font-weight:500;cursor:pointer;
  transition:background-color 150ms ease-out,color 150ms ease-out,transform 150ms ease-out;
}
.app-shell__logout:hover:not(:disabled){background:var(--color-surface-hover,#f1f5f9);color:var(--color-text,#0f172a);}
.app-shell__logout:focus-visible{outline:2px solid var(--color-primary,#2563eb);outline-offset:2px;box-shadow:0 0 0 4px rgba(37,99,235,0.35);}
.app-shell__logout:active:not(:disabled){transform:translateY(1px) scale(0.98);}
.app-shell__logout:disabled{opacity:.6;cursor:not-allowed;}
.app-shell__body{display:flex;flex-direction:column;flex:1 1 auto;align-items:stretch;min-width:0;}
.app-shell__main{
  flex:1 1 auto;min-width:0;width:100%;
  margin:0 auto;padding:var(--space-8,32px) var(--space-4,16px);
  outline:none;
}
.app-shell__footer{
  padding:var(--space-4,16px) var(--space-4,16px);
  border-top:1px solid var(--color-border,#e2e8f0);
  background:var(--color-surface,#ffffff);
  color:var(--color-text-muted,#64748b);
  font-size:var(--text-sm,14px);line-height:1.5;
}
.skip-link{
  position:absolute;left:-9999px;top:0;z-index:100;
  display:inline-block;padding:var(--space-3,12px) var(--space-4,16px);
  background:var(--color-primary,#2563eb);color:#ffffff;
  border-radius:0 0 var(--radius-md,8px) 0;
  font-size:var(--text-sm,14px);font-weight:500;text-decoration:none;
}
.skip-link:focus-visible{left:0;}
@media (min-width:768px){
  .app-shell__header{min-height:56px;padding:var(--space-3,12px) var(--space-6,24px);}
  .app-shell__body{flex-direction:row;}
  .app-shell__main{max-width:1280px;padding:var(--space-8,32px) var(--space-6,24px);}
}
@media (prefers-reduced-motion:reduce){
  .app-shell__logout{transition:none;}
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-app-shell-styles")) {
  const style = document.createElement("style");
  style.id = "fms-app-shell-styles";
  style.textContent = APP_SHELL_CSS;
  document.head.appendChild(style);
}

export function AppShell({ children, role = null, userName = null }: AppShellProps) {
  const { logout } = useSessionTimeout();
  const identity = role ?? userName;

  return (
    <div className="app-shell">
      <a className="skip-link" href="#main-content">
        Skip to main content
      </a>
      <SessionTimeoutBanner />

      <header className="app-shell__header">
        <span className="app-shell__brand">Financial Management</span>
        <div className="app-shell__user">
          {identity ? <span className="app-shell__badge">{identity}</span> : null}
          <button
            type="button"
            className="app-shell__logout"
            onClick={() => void logout()}
          >
            Sign out
          </button>
        </div>
      </header>

      <div className="app-shell__body">
        {/* Navigation renders its own <nav aria-label="Primary"> + mobile disclosure. */}
        <Navigation />
        <main id="main-content" tabIndex={-1} className="app-shell__main">
          {children}
        </main>
      </div>

      <footer className="app-shell__footer">
        © {new Date().getFullYear()} Financial Management System
      </footer>
    </div>
  );
}

export default AppShell;