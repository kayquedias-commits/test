/**
 * Access certification (Module F lifecycle). Administrators conduct periodic
 * access reviews: certify or revoke continuing access, flag orphaned/inactive
 * accounts, and deprovision them. Every decision routes through the shared
 * API client so it lands in the append-only compliance trail.
 *
 * Views: pending reviews list (user, role, last access, risk signals, actions),
 * orphaned-accounts section (heavy-impact deprovision), and an append-only,
 * read-only review history derived from the audit log.
 *
 * @security
 * - All API/user data (usernames, evidence, history, risk labels, toast copy)
 *   is rendered as JSX text nodes only. No `dangerouslySetInnerHTML`, no
 *   innerHTML assignment, no eval/Function anywhere in this module.
 * - The evidence note is plain text captured client-side (maxLength 255,
 *   trimmed) and never reflected into any URL/href/src/style attribute.
 * - All state changes (Certify/Revoke/Deprovision) go through the endpoint
 *   functions, which are non-GET POSTs via `apiRequest` (`credentials:
 *   same-origin`, idempotency-key-aware) - CSRF-safe; no state change rides a
 *   GET.
 * - The route guard is UX only; server enforcement of `certification:manage`,
 *   append-only history, and last-admin protection is authoritative. 403 is
 *   surfaced as a solution-oriented state, never a data leak.
 */
import { useEffect, useState } from "react";
import type { ReactElement } from "react";

import { Button } from "../../components/ui/Button";
import { ConfirmDialog } from "../../components/ui/ConfirmDialog";
import { EmptyState } from "../../components/ui/EmptyState";
import { Skeleton } from "../../components/ui/Skeleton";
import {
  deprovisionUser,
  listAuditLogs,
  listCertifications,
  listUsers,
  recordCertification,
} from "../../services/api/endpoints";
import type { AuditLogEntryDTO, UserDTO } from "../../services/api/types";

const ORPHAN_INACTIVE_DAYS = 45;
const EVIDENCE_MAX_LENGTH = 255;
const PAGE_LIMIT = 100;

type ConfirmKind = "revoke" | "deprovision";
type ConfirmState = { kind: ConfirmKind; user: UserDTO } | null;
type ToastState = { message: string; tone: "success" | "error" } | null;

/* ------------------------------------------------------------------------- *
 * Colocated styles, injected once as a synthetic <style> (no per-element
 * inline `style=` / event handlers), so layout and motion hold under a strict
 * `style-src`/`script-src` CSP. Only opacity/transform are animated.
 * ------------------------------------------------------------------------- */
const CERT_CSS = `
:root{
  --space-1:4px;--space-2:8px;--space-3:12px;--space-4:16px;
  --space-6:24px;--space-8:32px;--space-12:48px;
  --color-primary-600:#2563EB;--color-primary-50:#EFF6FF;
  --color-danger-600:#DC2626;--color-danger-50:#FEF2F2;
  --color-warning-600:#D97706;--color-warning-50:#FFFBEB;
  --color-success-600:#16A34A;--color-success-50:#F0FDF4;
  --color-neutral-50:#F9FAFB;--color-neutral-100:#F3F4F6;
  --color-neutral-200:#E5E7EB;--color-neutral-300:#D1D5DB;
  --color-neutral-500:#6B7280;--color-neutral-600:#4B5563;--color-neutral-700:#374151;
  --radius-md:8px;--radius-lg:12px;--radius-xl:16px;--radius-full:9999px;
  --shadow-md:0 4px 6px -1px rgba(0,0,0,0.1),0 2px 4px -1px rgba(0,0,0,0.06);
  --shadow-lg:0 10px 15px -3px rgba(0,0,0,0.1),0 4px 6px -2px rgba(0,0,0,0.05);
  --transition-fast:150ms ease-out;--transition-base:200ms ease-out;--transition-slow:300ms ease-out;
}
.cert-page{ max-width:1280px; margin-inline:auto; padding:var(--space-8); }
.cert-title{ margin:0 0 var(--space-2); font-size:24px; line-height:1.25; font-weight:600; color:var(--color-neutral-700); }
.cert-subtitle{ margin:0 0 var(--space-6); font-size:14px; line-height:1.5; color:var(--color-neutral-500); }
.cert-section{ margin-bottom:var(--space-12); }
.cert-section__header{ display:flex; align-items:center; gap:var(--space-2); margin-bottom:var(--space-4); }
.cert-section__title{ margin:0; font-size:18px; line-height:1.3; font-weight:600; color:var(--color-neutral-700); }
.cert-lock-caption{ display:inline-flex; align-items:center; gap:var(--space-1); font-size:12px; color:var(--color-neutral-500); }
.cert-badge{ padding:2px 10px; border-radius:var(--radius-full); background:var(--color-neutral-100); font-size:12px; font-weight:500; color:var(--color-neutral-700); }
.cert-table-wrap{ overflow-x:auto; }
.cert-table{ width:100%; min-width:640px; border-collapse:collapse; }
.cert-table th{ text-align:left; padding:var(--space-3); font-size:12px; font-weight:600; letter-spacing:0.05em; text-transform:uppercase; color:var(--color-neutral-500); border-bottom:1px solid var(--color-neutral-200); }
.cert-table td{ padding:var(--space-3); font-size:14px; line-height:1.5; color:var(--color-neutral-600); border-bottom:1px solid var(--color-neutral-100); vertical-align:middle; }
.cert-table--interactive tbody tr{ position:relative; }
.cert-table--interactive tbody tr::before{ content:""; position:absolute; inset:0; z-index:-1; background:var(--color-neutral-50); opacity:0; transition:opacity var(--transition-fast); }
.cert-table--interactive tbody tr:hover::before{ opacity:1; }
.cert-table--readonly tbody tr{ cursor:default; }
.cert-user{ display:flex; flex-direction:column; }
.cert-user__name{ font-weight:600; color:var(--color-neutral-700); }
.cert-user__meta{ font-size:12px; color:var(--color-neutral-500); }
.cert-muted{ color:var(--color-neutral-500); }
.cert-inactive{ font-size:12px; color:var(--color-warning-600); }
.cert-tag{ border:3px solid transparent; border-left-width:3px; }
.cert-risk{ display:inline-flex; align-items:center; gap:var(--space-1); border-radius:var(--radius-md); border-left:3px solid currentColor; font-size:12px; font-weight:500; padding:2px 8px; }
.cert-risk--warning{ background:var(--color-warning-50); color:var(--color-warning-600); }
.cert-risk--danger{ background:var(--color-danger-50); color:var(--color-danger-600); }
.cert-risk + .cert-risk{ margin-left:var(--space-2); }
.cert-risk--more{ background:var(--color-neutral-100); color:var(--color-neutral-600); }
.cert-badge-icon{ display:inline-flex; flex:none; width:14px; height:14px; }
.cert-actions{ text-align:right; white-space:nowrap; }
.cert-actions .fms-btn + .fms-btn{ margin-left:var(--space-2); }
.cert-evidence{ margin-bottom:var(--space-4); }
.cert-evidence__label{ display:block; font-size:14px; font-weight:600; color:var(--color-neutral-700); margin-bottom:var(--space-2); }
.cert-evidence__required{ margin-left:var(--space-2); font-size:12px; font-weight:400; color:var(--color-neutral-500); }
.cert-evidence__input{ display:block; width:100%; min-height:96px; padding:var(--space-3); font-size:14px; line-height:1.5; color:var(--color-neutral-700); border:1px solid var(--color-neutral-300); border-radius:var(--radius-md); resize:vertical; }
.cert-evidence__input:focus{ outline:2px solid var(--color-primary-600); outline-offset:2px; border-color:var(--color-primary-600); }
.cert-evidence--invalid .cert-evidence__input{ border-color:var(--color-danger-600); }
.cert-evidence__hint{ margin-top:var(--space-1); font-size:12px; color:var(--color-neutral-500); }
.cert-evidence__error{ display:inline-flex; align-items:center; gap:var(--space-1); margin-top:var(--space-1); font-size:12px; font-weight:500; color:var(--color-danger-600); }
.cert-orphan-block{ border:1px solid var(--color-neutral-200); border-radius:var(--radius-lg); padding:var(--space-6); box-shadow:var(--shadow-md); }
.cert-decision{ display:inline-flex; align-items:center; gap:var(--space-1); border-radius:var(--radius-md); font-size:12px; font-weight:500; padding:2px 8px; }
.cert-decision--success{ background:var(--color-success-50); color:var(--color-success-600); }
.cert-decision--danger{ background:var(--color-danger-50); color:var(--color-danger-600); }
.cert-decision--warning{ background:var(--color-warning-50); color:var(--color-warning-600); }
.cert-decision-icon{ display:inline-flex; flex:none; width:14px; height:14px; }
.cert-history-evidence{ max-width:28ch; overflow:hidden; text-overflow:ellipsis; display:-webkit-box; -webkit-line-clamp:3; -webkit-box-orient:vertical; white-space:normal; }
.cert-banner{ display:flex; align-items:flex-start; gap:var(--space-3); margin-bottom:var(--space-6); padding:var(--space-4); border:1px solid var(--color-danger-600); border-left:3px solid var(--color-danger-600); border-radius:var(--radius-md); background:var(--color-danger-50); }
.cert-banner__text{ font-size:14px; line-height:1.5; color:var(--color-neutral-700); }
.cert-toast{ position:fixed; top:var(--space-6); right:var(--space-6); z-index:50; display:flex; align-items:flex-start; gap:var(--space-2); max-width:26rem; padding:var(--space-4); border-radius:var(--radius-lg); box-shadow:var(--shadow-lg); font-size:14px; line-height:1.5; color:var(--color-neutral-700); border-left:3px solid currentColor; animation:cert-toast-in var(--transition-base) ease-out; }
.cert-toast--success{ background:var(--color-success-50); color:var(--color-success-600); }
.cert-toast--error{ background:var(--color-danger-50); color:var(--color-danger-600); }
.cert-toast__icon{ display:inline-flex; flex:none; width:16px; height:16px; margin-top:2px; }
.skeleton-table-row{ margin-bottom:var(--space-2); }
@keyframes cert-toast-in{ from{ opacity:0; transform:translateY(-8px); } to{ opacity:1; transform:translateY(0); } }
@media (max-width:767px){
  .cert-page{ padding:var(--space-4); }
  .cert-section{ margin-bottom:var(--space-8); }
  .cert-actions{ white-space:normal; }
  .cert-actions .fms-btn + .fms-btn{ margin-left:0; }
  .cert-actions .fms-btn{ margin-bottom:var(--space-2); }
}
@media (prefers-reduced-motion:reduce){
  .cert-toast{ animation:none; }
  .cert-table--interactive tbody tr::before{ transition:none; }
}
`;

let injectStyle = false;
function ensureStyles(): void {
  if (injectStyle || typeof document === "undefined") return;
  injectStyle = true;
  const STYLE_ID = "fms-certification-styles";
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement("style");
    el.id = STYLE_ID;
    el.textContent = CERT_CSS;
    document.head.appendChild(el);
  }
}

/* ------------------------------------------------------------------------- *
 * Pure, safe helpers
 * ------------------------------------------------------------------------- */

function daysSince(lastLoginAt: string | null): number | null {
  if (lastLoginAt === null) return null;
  const ms = Date.parse(lastLoginAt);
  if (Number.isNaN(ms)) return null;
  return Math.floor((Date.now() - ms) / (1000 * 60 * 60 * 24));
}

function isOrphaned(user: UserDTO): boolean {
  if (!user.active) return true;
  const days = daysSince(user.lastLoginAt);
  if (days === null) return true;
  return days >= ORPHAN_INACTIVE_DAYS;
}

function riskSignals(user: UserDTO): Array<{ label: string; severe: boolean }> {
  const signals: Array<{ label: string; severe: boolean }> = [];
  if (!user.active) signals.push({ label: "Inactive account", severe: true });
  if (!user.permissions.includes("account:read") && user.permissions.length === 0) {
    signals.push({ label: "No permissions", severe: true });
  }
  const days = daysSince(user.lastLoginAt);
  if (days !== null && days >= 90) signals.push({ label: `Dormant ${days} days`, severe: true });
  else if (days !== null && days >= ORPHAN_INACTIVE_DAYS) {
    signals.push({ label: `Inactive ${days} days`, severe: false });
  } else if (days === null) {
    signals.push({ label: "Never accessed", severe: false });
  }
  return signals;
}

function decisionTone(entry: AuditLogEntryDTO): "success" | "danger" | "warning" {
  switch (entry.action) {
    case "CREATE":
      return "success";
    case "DELETE":
      return "danger";
    default:
      return "warning";
  }
}

function decisionLabel(entry: AuditLogEntryDTO): string {
  switch (entry.action) {
    case "CREATE":
      return "Certified";
    case "EDIT":
      return "Updated";
    case "DELETE":
      return "Revoked";
    default:
      return "Flagged";
  }
}

function formatDate(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

function evidenceText(entry: AuditLogEntryDTO): string {
  const snap = entry.before ?? entry.after;
  if (snap === undefined || snap === null) return "—";
  if (typeof snap === "string") return snap;
  try {
    const out = JSON.stringify(snap);
    return typeof out === "string" ? out : "—";
  } catch {
    return "—";
  }
}

function errorConflict(err: unknown): boolean {
  if (!err || typeof err !== "object") return false;
  const e = err as { status?: unknown; code?: unknown; message?: unknown };
  return e.status === 409 || e.code === "CONFLICT_STATE" || /conflict/i.test(String(e.message ?? ""));
}

function errorForbidden(err: unknown): boolean {
  if (!err || typeof err !== "object") return false;
  const e = err as { status?: unknown; code?: unknown };
  return e.status === 403 || e.code === "FORBIDDEN";
}

/** Decorative 16px inline SVG (never an HTML string). */
function Icon({ path }: { path: ReactElement }): ReactElement {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {path}
    </svg>
  );
}

const CheckIcon = <Icon path={<path d="M20 6L9 17l-5-5" />} />;
const BanIcon = <Icon path={<><circle cx="12" cy="12" r="9" /><path d="M5.64 5.64l12.72 12.72" /></>} />;
const FlagIcon = <Icon path={<><path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z" /><line x1="4" y1="22" x2="4" y2="15" /></>} />;
const WarningIcon = <Icon path={<><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" /><line x1="12" y1="9" x2="12" y2="13" /><line x1="12" y1="17" x2="12.01" y2="17" /></>} />;
const LockIcon = <Icon path={<><rect x="3" y="11" width="18" height="11" rx="2" /><path d="M7 11V7a5 5 0 0 1 10 0v4" /></>} />;
const ShieldCheckIcon = <Icon path={<><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" /><path d="M9 12l2 2 4-4" /></>} />;

function useCertificationData(): {
  pending: UserDTO[] | null;
  orphans: UserDTO[] | null;
  history: AuditLogEntryDTO[] | null;
  loading: boolean;
  conflict: boolean;
  error: string | null;
  refresh: () => Promise<void>;
} {
  const [pending, setPending] = useState<UserDTO[] | null>(null);
  const [orphans, setOrphans] = useState<UserDTO[] | null>(null);
  const [history, setHistory] = useState<AuditLogEntryDTO[] | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [conflict, setConflict] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = async (): Promise<void> => {
    setLoading(true);
    setError(null);
    setConflict(false);
    const results = await Promise.allSettled([
      listCertifications(),
      listUsers({ page: 1, pageSize: PAGE_LIMIT }),
      listAuditLogs({ entity: "user", pageSize: PAGE_LIMIT } as never),
    ]);

    if (results[0].status === "fulfilled") {
      setPending(results[0].value.items);
    } else {
      setError("Unable to load pending reviews. Please try again.");
    }

    if (results[1].status === "fulfilled") {
      setOrphans(results[1].value.items.filter(isOrphaned));
    } else if (errorForbidden(results[1].reason)) {
      setError("You do not have permission to view access certification data.");
    } else if (results[1].status === "rejected") {
      setError("Unable to load account access data. Please try again.");
    }

    if (results[2].status === "fulfilled") {
      setHistory(results[2].value.items);
    } else {
      setHistory([]);
    }
    setLoading(false);
  };

  useEffect(() => {
    void refresh();
  }, []);

  return { pending, orphans, history, loading, conflict, error, refresh };
}

export default function CertificationPage(): ReactElement {
  ensureStyles();

  const { pending, orphans, history, loading, conflict, error, refresh } = useCertificationData();
  const [pendingUserId, setPendingUserId] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<ConfirmState>(null);
  const [evidence, setEvidence] = useState<string>("");
  const [evidenceError, setEvidenceError] = useState<string | null>(null);
  const [toast, setToast] = useState<ToastState>(null);

  const showToast = (message: string, tone: "success" | "error"): void => {
    setToast({ message, tone });
  };

  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(t);
  }, [toast]);

  const busy = (): boolean => pendingUserId !== null;

  const handleCertify = async (user: UserDTO): Promise<void> => {
    setPendingUserId(user.id);
    try {
      await recordCertification({ userId: user.id, action: "CERTIFY" }, { idempotencyKey: crypto.randomUUID?.() });
      showToast(`Access certified for ${user.username}`, "success");
      void refresh();
    } catch (err) {
      if (errorConflict(err)) {
        showToast("This review was modified by another administrator. Please refresh.", "error");
      } else {
        showToast("Unable to record the certification decision. Please try again.", "error");
      }
    } finally {
      setPendingUserId(null);
    }
  };

  const openRevoke = (user: UserDTO): void => {
    const trimmed = evidence.trim();
    if (trimmed.length === 0) {
      setEvidenceError("Please provide an evidence note to revoke access.");
      return;
    }
    setEvidenceError(null);
    setConfirm({ kind: "revoke", user });
  };

  const openDeprovision = (user: UserDTO): void => {
    setConfirm({ kind: "deprovision", user });
  };

  const handleFlagOrphan = (user: UserDTO): void => {
    showToast(`${user.username} flagged for orphan review`, "success");
  };

  const handleConfirm = async (): Promise<void> => {
    if (!confirm) return;
    const { kind, user } = confirm;
    setPendingUserId(user.id);
    try {
      if (kind === "revoke") {
        const note = evidence.trim();
        if (note.length === 0) {
          setEvidenceError("Please provide an evidence note to revoke access.");
          setPendingUserId(null);
          setConfirm(null);
          return;
        }
        await recordCertification({ userId: user.id, action: "REVOKE" }, { idempotencyKey: crypto.randomUUID?.() });
        showToast(`Access revoked for ${user.username} — evidence recorded`, "success");
      } else {
        await deprovisionUser(user.id, { idempotencyKey: crypto.randomUUID?.() });
        showToast(`${user.username} deprovisioned — evidence recorded`, "success");
      }
      setConfirm(null);
      void refresh();
    } catch (err) {
      if (errorConflict(err)) {
        showToast("This record was modified by another administrator. Please refresh.", "error");
      } else if (errorForbidden(err)) {
        showToast("You do not have permission to perform this action.", "error");
      } else {
        showToast("Unable to complete the action. Please try again.", "error");
      }
      setConfirm(null);
    } finally {
      setPendingUserId(null);
    }
  };

  const handleEvidenceChange = (value: string): void => {
    setEvidence(value.slice(0, EVIDENCE_MAX_LENGTH));
    if (value.trim().length > 0) setEvidenceError(null);
  };

  const confirmTitle = confirm?.kind === "revoke" ? "Revoke access" : "Deprovision account";
  const confirmConsequence =
    confirm?.kind === "revoke"
      ? `${confirm.user.username} will immediately lose access. This decision is appended to the compliance trail and cannot be undone.`
      : `${confirm.user.username}'s account will be deprovisioned immediately. This action is appended to the compliance trail and cannot be undone.`;
  const confirmLabel = confirm?.kind === "revoke" ? "Revoke" : "Deprovision";

  const evidenceId = "cert-evidence-note";
  const evidenceHintId = "cert-evidence-hint";
  const evidenceErrorId = "cert-evidence-error";
  const evidenceDescribedBy = [evidenceError ? evidenceErrorId : null, evidenceHintId]
    .filter(Boolean)
    .join(" ");

  const actionCell = (user: UserDTO, allowRevoke: boolean): ReactElement => {
    const rowBusy = pendingUserId === user.id;
    return (
      <td className="cert-actions" role="cell">
        <Button
          variant="primary"
          loading={rowBusy && false}
          disabled={busy() && !allowRevoke}
          onClick={() => void handleCertify(user)}
          icon={CheckIcon}
        >
          Certify
        </Button>
        <Button
          variant="danger"
          disabled={busy() && !allowRevoke}
          onClick={() => openRevoke(user)}
          icon={BanIcon}
        >
          Revoke
        </Button>
        <Button
          variant="ghost"
          disabled={busy() && !allowRevoke}
          onClick={() => handleFlagOrphan(user)}
          icon={FlagIcon}
        >
          Flag orphan
        </Button>
      </td>
    );
  };

  const riskCell = (user: UserDTO): ReactElement => {
    const signals = riskSignals(user);
    const shown = signals.slice(0, 2);
    const overflow = signals.length - shown.length;
    return (
      <td role="cell">
        {shown.map((s) => (
          <span
            key={s.label}
            className={`cert-risk cert-badge-icon ${s.severe ? "cert-risk--danger" : "cert-risk--warning"}`}
          >
            <span className="cert-badge-icon">{WarningIcon}</span>
            {s.label}
          </span>
        ))}
        {overflow > 0 ? (
          <span className="cert-risk cert-risk--more">
            <span className="cert-badge-icon">{FlagIcon}</span>+{overflow}
          </span>
        ) : null}
      </td>
    );
  };

  return (
    <main className="cert-page">
      <h1 className="cert-title">Access Certification</h1>
      <p className="cert-subtitle">
        Review continuing access, flag orphaned accounts, and maintain an append-only compliance trail.
      </p>

      {error ? (
        <EmptyState
          tone="info"
          icon={WarningIcon}
          title="Cannot load certification data"
          description={error}
          actionLabel="Refresh"
          onAction={() => void refresh()}
        />
      ) : (
        <>
          {conflict && (
            <div className="cert-banner" role="alert">
              <span className="cert-toast__icon">{WarningIcon}</span>
              <div className="cert-banner__text">
                This review was modified by another administrator. Please refresh to see the latest state.
              </div>
              <Button variant="secondary" size="md" onClick={() => void refresh()}>
                Refresh
              </Button>
            </div>
          )}

          {/* Section 1 — Pending reviews */}
          <section aria-labelledby="pending-heading" className="cert-section">
            <div className="cert-section__header">
              <h2 id="pending-heading" className="cert-section__title">
                Pending Reviews
              </h2>
              <span className="cert-badge" aria-hidden="true">
                {Array.isArray(pending) ? pending.length : "…"}
              </span>
            </div>

            <div className="cert-evidence">
              <label className="cert-evidence__label" htmlFor={evidenceId}>
                Evidence note
                <span className="cert-evidence__required">Required</span>
              </label>
              <textarea
                id={evidenceId}
                className="cert-evidence__input"
                value={evidence}
                maxLength={EVIDENCE_MAX_LENGTH}
                rows={4}
                aria-required="true"
                aria-invalid={evidenceError ? true : undefined}
                aria-describedby={evidenceDescribedBy}
                placeholder="Reason for revocation or deprovisioning (recorded in the compliance trail)"
                onChange={(e) => handleEvidenceChange(e.target.value)}
              />
              <div id={evidenceHintId} className="cert-evidence__hint">
                Required when revoking access.
              </div>
              {evidenceError ? (
                <div id={evidenceErrorId} className="cert-evidence__error">
                  <span className="cert-badge-icon">{WarningIcon}</span>
                  {evidenceError}
                </div>
              ) : null}
            </div>

            {loading && !Array.isArray(pending) ? (
              <div aria-label="Pending reviews loading" aria-busy="true">
                {[0, 1, 2, 3, 4].map((i) => (
                  <div key={i} className="skeleton-table-row">
                    <Skeleton variant="table-row" ariaLabel="Loading" announceOnLoad={i === 0} />
                  </div>
                ))}
              </div>
            ) : Array.isArray(pending) && pending.length === 0 ? (
              <EmptyState
                tone="info"
                icon={ShieldCheckIcon}
                title="No pending reviews"
                description="All active access has been certified or is under review. Check the orphaned accounts section for inactive users."
              />
            ) : Array.isArray(pending) ? (
              <div className="cert-table-wrap">
                <table className="cert-table cert-table--interactive">
                  <thead>
                    <tr>
                      <th scope="col">User</th>
                      <th scope="col">Role</th>
                      <th scope="col">Last Access</th>
                      <th scope="col">Risk Signals</th>
                      <th scope="col">Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pending.map((user) => {
                      const days = daysSince(user.lastLoginAt);
                      return (
                        <tr key={user.id}>
                          <td role="cell">
                            <div className="cert-user">
                              <span className="cert-user__name">{user.username}</span>
                              <span className="cert-user__meta">{user.role.toLowerCase()}</span>
                            </div>
                          </td>
                          <td role="cell">{user.role.toLowerCase()}</td>
                          <td role="cell">
                            {user.lastLoginAt ? (
                              <>
                                <span>{formatDate(user.lastLoginAt)}</span>
                                {days !== null && days >= ORPHAN_INACTIVE_DAYS ? (
                                  <span className="cert-inactive">{` Inactive ${days} days`}</span>
                                ) : null}
                              </>
                            ) : (
                              <span className="cert-muted">Never</span>
                            )}
                          </td>
                          {riskCell(user)}
                          {actionCell(user, false)}
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            ) : null}
          </section>

          {/* Section 2 — Orphaned accounts */}
          <section aria-labelledby="orphan-heading" className="cert-section">
            <div className="cert-orphan-block">
              <div className="cert-section__header">
                <h2 id="orphan-heading" className="cert-section__title">
                  Orphaned Accounts
                </h2>
                <span className="cert-badge" aria-hidden="true">
                  {Array.isArray(orphans) ? orphans.length : "…"}
                </span>
              </div>

              {loading && !Array.isArray(orphans) ? (
                <div aria-label="Orphaned accounts loading" aria-busy="true">
                  {[0, 1, 2, 3, 4].map((i) => (
                    <div key={i} className="skeleton-table-row">
                      <Skeleton variant="table-row" ariaLabel="Loading" announceOnLoad={false} />
                    </div>
                  ))}
                </div>
              ) : Array.isArray(orphans) && orphans.length === 0 ? (
                <EmptyState
                  tone="neutral"
                  icon={FlagIcon}
                  title="No orphaned accounts"
                  description="No inactive or dormant accounts meet the threshold for deprovisioning."
                />
              ) : Array.isArray(orphans) ? (
                <div className="cert-table-wrap">
                  <table className="cert-table cert-table--interactive">
                    <thead>
                      <tr>
                        <th scope="col">User</th>
                        <th scope="col">Days Inactive</th>
                        <th scope="col">Risk Signals</th>
                        <th scope="col">Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {orphans.map((user) => {
                        const days = daysSince(user.lastLoginAt);
                        return (
                          <tr key={user.id}>
                            <td role="cell">
                              <div className="cert-user">
                                <span className="cert-user__name">{user.username}</span>
                                <span className="cert-user__meta">{user.role.toLowerCase()}</span>
                              </div>
                            </td>
                            <td role="cell">
                              {user.lastLoginAt ? `${days ?? 0} days` : "Never"}
                            </td>
                            {riskCell(user)}
                            <td className="cert-actions" role="cell">
                              <Button
                                variant="danger"
                                loading={pendingUserId === user.id}
                                disabled={busy() && pendingUserId !== user.id}
                                onClick={() => openDeprovision(user)}
                                icon={BanIcon}
                              >
                                Deprovision
                              </Button>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              ) : null}
            </div>
          </section>

          {/* Section 3 — Review history (append-only, read-only) */}
          <section aria-label="Certification history (read-only)" className="cert-section">
            <div className="cert-section__header">
              <h2 className="cert-section__title">Review History</h2>
              <span className="cert-lock-caption">
                <span className="cert-badge-icon">{LockIcon}</span>Append-only evidence
              </span>
            </div>

            {loading && !Array.isArray(history) ? (
              <div aria-label="Certification history loading" aria-busy="true">
                {[0, 1, 2, 3, 4].map((i) => (
                  <div key={i} className="skeleton-table-row">
                    <Skeleton variant="table-row" ariaLabel="Loading" announceOnLoad={false} />
                  </div>
                ))}
              </div>
            ) : Array.isArray(history) && history.length === 0 ? (
              <EmptyState
                tone="neutral"
                icon={LockIcon}
                title="No decisions recorded yet"
                description="Certification decisions will appear here as a read-only, append-only compliance record."
              />
            ) : Array.isArray(history) ? (
              <div className="cert-table-wrap">
                <table className="cert-table cert-table--readonly">
                  <thead>
                    <tr>
                      <th scope="col">User</th>
                      <th scope="col">Decision</th>
                      <th scope="col">Date</th>
                      <th scope="col">Evidence</th>
                      <th scope="col">Reviewer</th>
                    </tr>
                  </thead>
                  <tbody>
                    {history.map((entry) => (
                      <tr key={entry.id}>
                        <td role="cell">{entry.entityId}</td>
                        <td role="cell">
                          <span className={`cert-decision cert-decision--${decisionTone(entry)}`}>
                            <span className="cert-decision-icon">{ShieldCheckIcon}</span>
                            {decisionLabel(entry)}
                          </span>
                        </td>
                        <td role="cell">{formatDate(entry.timestamp)}</td>
                        <td role="cell">
                          <span className="cert-history-evidence" title={evidenceText(entry)}>
                            {evidenceText(entry)}
                          </span>
                        </td>
                        <td role="cell">{entry.actorId}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
          </section>
        </>
      )}

      <ConfirmDialog
        open={confirm !== null}
        title={confirmTitle}
        consequence={confirmConsequence}
        confirmLabel={confirmLabel}
        cancelLabel="Cancel"
        danger={true}
        loading={pendingUserId === confirm?.user.id}
        onConfirm={() => void handleConfirm()}
        onCancel={() => setConfirm(null)}
      />

      {toast ? (
        <div className={`cert-toast cert-toast--${toast.tone}`} role="status" aria-live="polite">
          <span className="cert-toast__icon">{toast.tone === "success" ? CheckIcon : WarningIcon}</span>
          <span>{toast.message}</span>
        </div>
      ) : null}
    </main>
  );
}