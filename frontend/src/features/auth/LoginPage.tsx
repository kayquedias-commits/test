/**
 * Login screen — password step (Blueprint §8.1, Module F).
 *
 * Route `/login`. Single-role-agnostic form: username + password, submitted via
 * the session store's `login()` (which itself posts to `/v1/auth/login`). All
 * API work is delegated to the session store — this component never fetches
 * and never logs credentials. On success the session status flips to
 * `mfa_pending` or `authenticated` and navigation is delegated to injected
 * callbacks so the parent (route-aware) layer owns routing.
 *
 * Error surface is an allowlist: the store maps every failure to a
 * solution-oriented `loginError.message` (never internal state, stacks, or
 * enumeration hints), and it is rendered here strictly as escaped text.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";
import { useSession } from "../../app/stores/session";
import { Field } from "../../components/ui/Field";
import { Button } from "../../components/ui/Button";

export interface LoginPageProps {
  /** Fired when login enters the MFA challenge → parent routes to /mfa. */
  onRequireMfa?: () => void;
  /** Fired when login completes (authenticated) → parent routes to the role dashboard. */
  onSignedIn?: () => void;
}

/**
 * Stylesheet colocated with the component, injected once as a <style> element
 * (no per-element inline `style=` attributes) so it holds under a strict
 * `style-src` CSP context, matching Button/Field conventions.
 */
const LOGIN_PAGE_CSS = `
:root{
  --fms-space-1:4px;--fms-space-2:8px;--fms-space-3:12px;--fms-space-4:16px;
  --fms-space-6:24px;--fms-space-8:32px;
  --fms-text-sm:14px;--fms-text-base:16px;--fms-text-3xl:30px;
  --fms-font-semibold:600;
  --fms-leading-tight:1.25;--fms-leading-normal:1.5;
  --fms-radius-md:8px;--fms-radius-lg:12px;
  --fms-shadow-lg:0 10px 15px -3px rgba(0,0,0,0.1),0 4px 6px -2px rgba(0,0,0,0.05);
  --fms-color-bg:#f8fafc;--fms-color-surface:#ffffff;--fms-color-border:#d1d5db;
  --fms-color-text:#1f2937;--fms-color-text-secondary:#475569;
  --fms-color-danger:#dc2626;--fms-color-danger-dark:#991b1b;
  --fms-transition-fast:150ms ease-out;
}
.lp-page{
  min-height:100vh;display:grid;place-items:center;
  padding:var(--fms-space-4,16px);
  background-color:var(--fms-color-bg,#f8fafc);
}
.lp-card{
  width:100%;max-width:440px;
  padding:var(--fms-space-6,24px);
  background-color:var(--fms-color-surface,#ffffff);
  border:1px solid var(--fms-color-border,#d1d5db);
  border-radius:var(--fms-radius-lg,12px);
  box-shadow:var(--fms-shadow-lg);
}
@media (min-width:768px){
  .lp-card{padding:var(--fms-space-8,32px);}
}
.lp-title{
  margin:0 0 var(--fms-space-2,8px);
  font-size:var(--fms-text-3xl,30px);font-weight:var(--fms-font-semibold,600);
  line-height:var(--fms-leading-tight,1.25);color:var(--fms-color-text,#1f2937);
}
.lp-intro{
  margin:0 0 var(--fms-space-6,24px);
  font-size:var(--fms-text-sm,14px);line-height:var(--fms-leading-normal,1.5);
  color:var(--fms-color-text-secondary,#475569);
  max-width:65ch;
}
.lp-form{
  display:flex;flex-direction:column;gap:var(--fms-space-4,16px);
}
.lp-error{
  display:flex;align-items:flex-start;gap:var(--fms-space-2,8px);
  padding:var(--fms-space-3,12px) var(--fms-space-4,16px);
  background-color:#fef2f2;border:1px solid var(--fms-color-danger,#dc2626);
  border-radius:var(--fms-radius-md,8px);
  color:var(--fms-color-danger-dark,#991b1b);
  font-size:var(--fms-text-sm,14px);line-height:var(--fms-leading-normal,1.5);
  opacity:0;transform:translateY(-2px);
  transition:opacity var(--fms-transition-fast,150ms ease-out),transform var(--fms-transition-fast,150ms ease-out);
}
.lp-error--visible{opacity:1;transform:translateY(0);}
.lp-error[hidden]{display:none;}
.lp-error p{margin:0;}
.lp-error-icon{flex:none;width:16px;height:16px;margin-top:2px;}
.lp-error-copy{min-height:0;display:flex;flex-direction:column;gap:var(--fms-space-1,4px);}
.lp-error-secondary{color:var(--fms-color-text-secondary,#475569);font-weight:400;}
@media (prefers-reduced-motion:reduce){
  .lp-card,.lp-error{transition-duration:0.01ms;}
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-login-page-styles")) {
  const style = document.createElement("style");
  style.id = "fms-login-page-styles";
  style.textContent = LOGIN_PAGE_CSS;
  document.head.appendChild(style);
}

/** Decorative warning glyph for the allowlisted error surface (never color-only). */
function WarningIcon(): ReactElement {
  return (
    <svg
      className="lp-error-icon"
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
      <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
  );
}

export function LoginPage({ onRequireMfa, onSignedIn }: LoginPageProps): ReactElement {
  const usernameRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  const status = useSession((s) => s.status);
  // login() never throws: every failure lands in this allowlisted error.
  const loginError = useSession((s) => s.loginError);
  const isLoggingIn = useSession((s) => s.isLoggingIn);

  // Required-ness is enforced by disabling submit until both fields are
  // non-empty; validation.ts is outside this file's import contract.
  const canSubmit = username.trim() !== "" && password.length > 0;

  const handleUsernameChange = useCallback((e: ChangeEvent<HTMLInputElement>) => {
    setUsername(e.target.value);
  }, []);

  const handlePasswordChange = useCallback((e: ChangeEvent<HTMLInputElement>) => {
    setPassword(e.target.value);
  }, []);

  const handleSubmit = useCallback(
    (e: FormEvent<HTMLFormElement>) => {
      e.preventDefault();
      if (isLoggingIn) return;
      if (!canSubmit) {
        if (username.trim() === "") usernameRef.current?.focus();
        else passwordRef.current?.focus();
        return;
      }
      // Delegated to the store — never reach the network from this component.
      void useSession.getState().login(username.trim(), password);
    },
    [canSubmit, isLoggingIn, username, password],
  );

  const cbMap = useMemo(
    () => ({ onRequireMfa, onSignedIn }),
    [onRequireMfa, onSignedIn],
  );

  // React on the store's post-login status to delegate navigation out to the
  // route-aware parent. No client-side MFA bypass: navigation happens only
  // after the MFA challenge is entered on a separate screen.
  useEffect(() => {
    if (status === "mfa_pending") cbMap.onRequireMfa?.();
    else if (status === "authenticated") cbMap.onSignedIn?.();
  }, [status, cbMap]);

  const errorCopy = loginError
    ? { title: loginError.message, secondary: null as string | null }
    : null;

  return (
    <main className="lp-page">
      <section className="lp-card" aria-label="Sign in">
        <h1 className="lp-title">Sign in</h1>
        <p className="lp-intro">Enter your credentials to access your financial workspace.</p>

        <form className="lp-form" onSubmit={handleSubmit} noValidate>
          <Field id="username" label="Username" required>
            <input
              id="username"
              type="text"
              value={username}
              onChange={handleUsernameChange}
              ref={usernameRef}
              autoComplete="username"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
            />
          </Field>

          <Field id="password" label="Password" required>
            <input
              id="password"
              type="password"
              value={password}
              onChange={handlePasswordChange}
              ref={passwordRef}
              autoComplete="current-password"
            />
          </Field>

          <div
            className={`lp-error${loginError ? " lp-error--visible" : ""}`}
            role="status"
            aria-live="polite"
            aria-atomic="true"
            hidden={!loginError}
          >
            {errorCopy ? (
              <>
                <WarningIcon />
                <div className="lp-error-copy">
                  <p>{errorCopy.title}</p>
                  {errorCopy.secondary ? <p className="lp-error-secondary">{errorCopy.secondary}</p> : null}
                </div>
              </>
            ) : null}
          </div>

          <Button
            type="submit"
            variant="primary"
            size="lg"
            loading={isLoggingIn}
            disabled={!canSubmit}
          >
            Sign in
          </Button>
        </form>
      </section>
    </main>
  );
}

export default LoginPage;