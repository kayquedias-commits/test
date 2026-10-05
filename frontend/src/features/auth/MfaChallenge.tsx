/**
 * MFA challenge step (TOTP) — Blueprint §5.1 (MFA for all users) / §8.1 login flow.
 *
 * Rends the 6-digit authenticator code entry on the `/mfa` route. Composes the
 * shared `Field`/`Button` controls and delegates verification to the session
 * store (`completeMfa`), which is the sole authority for TOTP validation — this
 * component performs format-only client sanitization (never code verification)
 * so it stays safe against client-clock drift.
 *
 * @security The code lives only in transient component state and is never
 *   persisted (no localStorage/sessionStorage), never logged, and never echoed
 *   into the DOM. All rendered text is static or allowlisted from the store.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";
import { useSession } from "../../app/stores/session";
import { Field } from "../../components/ui/Field";
import { Button } from "../../components/ui/Button";

const MFA_CODE_LENGTH = 6;
const LOGIN_PATH = "/login";

/* ------------------------------------------------------------------------- *
 * Colocated styles, injected once as a synthetic <style>. No inline `style=`
 * attributes and no style text derived from user data (CSP-clean). Token names
 * follow the existing --fms-* convention and fall back to safe defaults.
 * ------------------------------------------------------------------------- */
const MFA_PAGE_CSS = `
:root{
  --mfa-page-bg:#f8fafc; --mfa-card-bg:#ffffff; --mfa-text-primary:#111827;
  --mfa-text-secondary:#475569; --mfa-back-hover:#111827;
  --fms-space-1:4px; --fms-space-2:8px; --fms-space-3:12px; --fms-space-4:16px;
  --fms-space-6:24px; --fms-space-8:32px;
  --fms-text-sm:14px; --fms-text-base:16px; --fms-text-2xl:24px; --fms-text-3xl:30px;
  --fms-font-medium:500; --fms-font-semibold:600; --fms-font-bold:700;
  --fms-leading-tight:1.25; --fms-leading-normal:1.5; --fms-leading-relaxed:1.75;
  --fms-radius-md:8px; --fms-radius-lg:12px;
  --fms-shadow-md:0 4px 6px -1px rgba(0,0,0,0.1), 0 2px 4px -1px rgba(0,0,0,0.06);
  --fms-transition-fast:150ms ease-out;
}
.mfa-page{
  display:grid; place-items:center; min-height:100vh; padding:var(--fms-space-8) var(--fms-space-4);
  background:var(--mfa-page-bg);
}
.mfa-card{
  width:100%; max-width:400px; box-sizing:border-box; padding:var(--fms-space-6) var(--fms-space-4);
  background:var(--mfa-card-bg); border-radius:var(--fms-radius-lg); box-shadow:var(--fms-shadow-md);
}
.mfa-title{
  margin:0 0 var(--fms-space-2); font-size:var(--fms-text-3xl); font-weight:var(--fms-font-bold);
  line-height:var(--fms-leading-tight); color:var(--mfa-text-primary);
}
.mfa-helper{
  max-width:65ch; margin:0 0 var(--fms-space-6); font-size:var(--fms-text-sm);
  font-weight:var(--fms-font-medium); line-height:var(--fms-leading-normal); color:var(--mfa-text-secondary);
}
.mfa-form{ display:flex; flex-direction:column; gap:var(--fms-space-6); }
.mfa-back-row{ margin-top:var(--fms-space-8); text-align:center; }
.mfa-back{ background:transparent; color:var(--mfa-text-secondary); min-height:44px; }
.mfa-back:hover{ color:var(--mfa-back-hover); text-decoration:underline; }
.mfa-back:focus-visible{ outline:var(--fms-focus-width,3px) solid var(--color-field-border-focus,#2563eb); outline-offset:2px; border-radius:var(--fms-radius-md); }
.mfa-sr{ position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; border:0; }
@media (min-width:768px){
  .mfa-card{ padding:var(--fms-space-8); }
}
@media (prefers-reduced-motion:reduce){
  .mfa-card, .mfa-title, .mfa-helper{ transition:none; }
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-mfa-page-styles")) {
  const style = document.createElement("style");
  style.id = "fms-mfa-page-styles";
  style.textContent = MFA_PAGE_CSS;
  document.head.appendChild(style);
}

/** Props consumed by the MFA challenge step. */
export interface MfaChallengeProps {
  /** Fire once the session flips to `authenticated`, letting the parent route
   *  to the role-tailored dashboard. Navigation is delegated (never done here). */
  onVerified?: () => void;
}

/**
 * Two-factor authentication (TOTP) challenge view.
 *
 * Auto-submits on the 6th digit (announced via a polite live region), keeps all
 * error/focus handling in the TOTP field, and delegates navigation to the
 * parent through `onVerified`.
 */
export function MfaChallenge({ onVerified }: MfaChallengeProps): ReactElement {
  const [code, setCode] = useState("");
  const [suppressError, setSuppressError] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);

  const submittedRef = useRef(false);
  const attemptedRef = useRef(false);
  const onVerifiedRef = useRef(onVerified);
  useEffect(() => {
    onVerifiedRef.current = onVerified;
  }, [onVerified]);

  const status = useSession((s) => s.status);
  const isCompletingMfa = useSession((s) => s.isCompletingMfa);
  const mfaError = useSession((s) => s.mfaError);

  const canSubmit = code.length === MFA_CODE_LENGTH && !isCompletingMfa;
  // Store messages are allowlisted (generic, solution-oriented); errors from the
  // component never interpolate the code or any raw API payload.
  const displayedError = localError ?? (mfaError && !suppressError ? mfaError.message : null);

  /** Navigate to the role dashboard once, and only once, authentication lands. */
  useEffect(() => {
    if (status === "authenticated") onVerifiedRef.current?.();
  }, [status]);

  // Restore focus to the TOTP input on an invalid-code error. Field clones the
  // control with its own props and does not forward a child ref, so the input
  // is located by the stable id Field guarantees. The input persists across the
  // re-render, so focus is retained naturally; this is a defensive re-focus.
  useEffect(() => {
    if (!mfaError) return;
    const el = document.getElementById("mfa-code");
    if (el instanceof HTMLElement) el.focus();
  }, [mfaError]);

  // Loop protection: if a completed attempt leaves status "mfa_pending" with no
  // surfaced error (stale store/server misconfiguration), surface a generic
  // message and stay on the view — never redirect and never auto-resubmit.
  useEffect(() => {
    if (isCompletingMfa) {
      attemptedRef.current = true;
      return;
    }
    if (!attemptedRef.current) return;
    attemptedRef.current = false;
    const s = useSession.getState();
    if (s.status === "mfa_pending" && !s.mfaError) {
      setLocalError(
        "Verification is taking too long. Please check your code and try again.",
      );
    }
  }, [isCompletingMfa]);

  /** Format-only sanitization; auto-submits exactly once on the 6th digit. */
  const handleChange = useCallback((e: ChangeEvent<HTMLInputElement>) => {
    const digits = e.target.value.replace(/\D/g, "").slice(0, MFA_CODE_LENGTH);
    setCode(digits);
    setSuppressError(false);
    setLocalError(null);
    submittedRef.current = false;
    const s = useSession.getState();
    if (digits.length === MFA_CODE_LENGTH && !s.isCompletingMfa) {
      submittedRef.current = true;
      void s.completeMfa(digits);
    }
  }, []);

  const handleSubmit = useCallback(
    (e: FormEvent<HTMLFormElement>) => {
      e.preventDefault();
      if (code.length !== MFA_CODE_LENGTH || isCompletingMfa) return;
      submittedRef.current = true;
      setSuppressError(false);
      setLocalError(null);
      void useSession.getState().completeMfa(code);
    },
    [code, isCompletingMfa],
  );

  /** Clear any in-progress challenge state before leaving to the sign-in page. */
  const backToSignIn = useCallback(() => {
    useSession.getState().cancelMfa();
  }, []);

  return (
    <main className="mfa-page">
      <section className="mfa-card" aria-label="Two-factor authentication">
        <h1 className="mfa-title">Two-factor authentication</h1>
        <p className="mfa-helper">Enter the 6-digit code from your authenticator app.</p>

        <form className="mfa-form" onSubmit={handleSubmit} noValidate>
          <Field
            id="mfa-code"
            label="Authentication code"
            error={displayedError}
            disabled={isCompletingMfa}
          >
            <input
              id="mfa-code"
              type="text"
              inputMode="numeric"
              pattern="[0-9]{6}"
              maxLength={MFA_CODE_LENGTH}
              autoComplete="one-time-code"
              autoFocus
              onChange={handleChange}
            />
          </Field>

          <Button
            type="submit"
            variant="primary"
            size="lg"
            loading={isCompletingMfa}
            disabled={!canSubmit}
          >
            {isCompletingMfa ? "Verifying…" : "Verify & continue"}
          </Button>

          {isCompletingMfa ? (
            <div className="mfa-sr" role="status">
              Verifying code…
            </div>
          ) : null}
        </form>

        <div className="mfa-back-row">
          <Button
            variant="ghost"
            size="md"
            className="mfa-back"
            href={LOGIN_PATH}
            onClick={backToSignIn}
          >
            Back to sign in
          </Button>
        </div>
      </section>
    </main>
  );
}

export default MfaChallenge;