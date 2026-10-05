import { useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";

import { Field } from "../../components/ui/Field";
import { Button } from "../../components/ui/Button";
import { createUser, updateUser } from "../../services/api/endpoints";
import { ApiError, ConflictError, ValidationError } from "../../services/api/client";
import type { UserDTO, UserRole } from "../../services/api/types";
import { required, validateForm } from "../../utils/validation";

/*
 * Forms for creating and editing a user (Module F). Create mode supplies a
 * password; edit mode never renders one (the server re-authenticates the
 * acting admin through their current password elsewhere). The last admin's
 * role/active controls are disabled with persistent, screen-reader-announceable
 * helper text — a native disabled control cannot receive focus or show a
 * hover-only tooltip, so the reason is always visible text referenced via
 * aria-describedby.
 *
 * The acting admin's least-privilege assignable-role set, the success/error
 * toasts, and the redirect back to the users list are provided as props
 * (assignableRoles, onSuccess, onError, onCancel, onRefresh) rather than
 * imported from the permission/notification stores or the router. That keeps
 * this page inside its dependency contract; the parent wires those into the
 * app's permission map, toast store, and /admin/users route.
 *
 * Contract gaps handled here (the summary drops below): `UserDTO` carries no
 * `version`, so the optimistic-lock counter for `updateUser` must be supplied
 * by the parent via the `version` prop — edit mode fails closed (submit
 * disabled) when it is absent rather than overwriting. `createUser(dto)` has
 * no typed `password` field, so the create payload is cast; the password is
 * still serialized and must be sent exactly as typed (the server enforces the
 * password policy — client strength checks are UX only).
 *
 * Role assignment and the last-admin invariant are enforced server-side; the
 * client gating here is UX only and is never an authorization boundary.
 */

interface PasswordResult {
  valid: boolean;
  text: string;
}

/** Anchored, linear allowlist — no backreferences or nested quantifiers (ReDoS-safe). */
const USERNAME_RE = /^[A-Za-z0-9_.-]{3,64}$/;
const USERNAME_MSG =
  "Use only letters, numbers, underscores, dashes, or dots (3–64 characters).";

const PASSWORD_MIN = 8;
const PASSWORD_MSG =
  "Use at least 8 characters, including an uppercase letter and a number.";

const LAST_ADMIN_NOTE = "The system must keep at least one administrator.";

const ROLE_LABELS: Record<UserRole, string> = {
  ADMIN: "System administrator",
  FINANCE_MANAGER: "Finance manager",
  ACCOUNTANT: "Accountant",
  BUDGET_OWNER: "Budget owner",
  AUDITOR: "Auditor",
  VIEW_ONLY: "View only",
  EMPLOYEE: "Employee",
};

/** Role options grouped by tier so an acting admin scans at most five per group. */
const ROLE_GROUPS: ReadonlyArray<{ label: string; roles: readonly UserRole[] }> = [
  { label: "Administration", roles: ["ADMIN"] },
  { label: "Finance", roles: ["FINANCE_MANAGER", "ACCOUNTANT", "BUDGET_OWNER"] },
  { label: "Limited access", roles: ["AUDITOR", "VIEW_ONLY", "EMPLOYEE"] },
];

/** Fallback full backend role scope when the parent does not supply a scoped set. */
const DEFAULT_ASSIGNABLE: readonly UserRole[] = [
  "ADMIN",
  "FINANCE_MANAGER",
  "ACCOUNTANT",
  "BUDGET_OWNER",
  "AUDITOR",
  "VIEW_ONLY",
  "EMPLOYEE",
];

function validateUsername(value: string): string | null {
  if (!USERNAME_RE.test(value)) return USERNAME_MSG;
  return null;
}

/** Empty-state handled by `required`; this only reports the strength policy. */
function validatePasswordStrength(value: string): string | null {
  if (value == null || value === "") return null;
  return evaluatePasswordStrength(value).valid ? null : PASSWORD_MSG;
}

function evaluatePasswordStrength(value: string): PasswordResult {
  if (value.length === 0) return { valid: false, text: "" };
  const hasUpper = /[A-Z]/.test(value);
  const hasDigit = /[0-9]/.test(value);
  const lengthOk = value.length >= PASSWORD_MIN;
  if (!lengthOk || !hasUpper || !hasDigit) {
    return {
      valid: false,
      text: `Needs ${PASSWORD_MIN}+ characters, an uppercase letter, and a number.`,
    };
  }
  return { valid: true, text: "Strong password" };
}

/**
 * Switch-styled checkbox with a min 44×44 px hit area and an accessible name.
 * `type="button"` is mandatory: inside a <form> the native default would submit.
 */
function AccessSwitch({
  id,
  checked,
  onChange,
  disabled,
  describedBy,
}: {
  id: string;
  checked: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
  describedBy?: string;
}): ReactElement {
  return (
    <span className="upf__switch">
      <button
        type="button"
        id={id}
        role="switch"
        aria-checked={checked}
        aria-describedby={describedBy}
        disabled={disabled}
        className="upf__switch__toggle"
        onClick={() => onChange(!checked)}
      >
        <span className="upf__switch__thumb" aria-hidden="true" />
      </button>
      <span className="upf__switch__label">Active</span>
    </span>
  );
}

export interface UserFormPageProps {
  /** Record being edited; omit/null => create mode. */
  user?: UserDTO | null;
  /** Optimistic-lock counter for edit-mode `updateUser` (UserDTO has none). */
  version?: number;
  /** True when the edited record is the system's last admin => role/active disabled. */
  isLastAdmin?: boolean;
  /** Id of the session user; enables the self-demotion confirm guard. */
  actingUserId?: string;
  /** Least-privilege role set the acting admin may assign (mirror of backend scope). */
  assignableRoles?: ReadonlyArray<UserRole>;
  /** Called on successful create/update (parent shows a toast and redirects). */
  onSuccess?: () => void;
  /** Emitted with a plain-text message for non-field errors. */
  onError?: (message: string) => void;
  /** Optional re-fetch used by the 409 recovery banner's Refresh action. */
  onRefresh?: () => void;
  /** Emitted when Cancel is pressed (parent navigates to /admin/users). */
  onCancel?: () => void;
}

export function UserFormPage({
  user,
  version,
  isLastAdmin = false,
  actingUserId,
  assignableRoles,
  onSuccess,
  onError,
  onRefresh,
  onCancel,
}: UserFormPageProps): ReactElement {
  const assignable = assignableRoles ?? DEFAULT_ASSIGNABLE;
  const isCreate = !user;

  const [username, setUsername] = useState(user?.username ?? "");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<UserRole | undefined>(user?.role ?? assignable[0]);
  const [active, setActive] = useState<boolean>(user?.active ?? true);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [usernameServerError, setUsernameServerError] = useState<string | null>(null);
  const [roleServerError, setRoleServerError] = useState<string | null>(null);
  const [bannerRef, setBannerRef] = useState<HTMLDivElement | null>(null);

  // Fail closed on an unusable version — never send an unvalidated counter.
  const editVersion =
    typeof version === "number" && Number.isInteger(version) && version >= 0
      ? version
      : null;
  const missingEditVersion = !isCreate && editVersion === null;

  const passwordStrength = useMemo(
    () => evaluatePasswordStrength(password),
    [password],
  );

  const schema = useMemo(
    () =>
      isCreate
        ? {
            username: [required("Username"), validateUsername],
            password: [required("Password"), validatePasswordStrength],
          }
        : { username: [required("Username"), validateUsername] },
    [isCreate],
  );

  const isFormValid = useMemo(
    () => validateForm({ username, password }, schema).length === 0,
    [username, password, schema],
  );

  // Submitting stays disabled when invalid, while loading, with no assignable
  // role to grant, or when an edit has no version to optimistically lock on.
  const submitDisabled =
    isSubmitting ||
    !isFormValid ||
    assignable.length === 0 ||
    missingEditVersion ||
    (isCreate && !password);

  useEffect(() => {
    // Move focus to the conflict banner so it is announced before other content.
    if (conflict && bannerRef) bannerRef.focus();
  }, [conflict, bannerRef]);

  useEffect(
    () => () => {
      // Do not let the password linger in component state after unmount.
      setPassword("");
    },
    [],
  );

  function handleUsernameChange(e: { target: { value: string } }): void {
    setUsername(e.target.value);
    setUsernameServerError(null);
  }

  function handlePasswordChange(e: { target: { value: string } }): void {
    setPassword(e.target.value);
  }

  function handleRoleChange(e: { target: { value: string } }): void {
    setRole(e.target.value as UserRole);
    setRoleServerError(null);
  }

  async function handleSubmit(): Promise<void> {
    if (isSubmitting) return;
    setIsSubmitting(true);
    setConflict(false);

    // Self-demotion guard: the acting admin removing their own admin access is
    // a hard-to-reverse, destructive action — require explicit confirmation.
    if (
      !isCreate &&
      user &&
      actingUserId &&
      actingUserId === user.id &&
      (role !== user.role || active !== user.active)
    ) {
      const ok = window.confirm(
        "You will lose administrator access to this account. Continue?",
      );
      if (!ok) {
        setIsSubmitting(false);
        return;
      }
    }

    try {
      if (isCreate) {
        // UserInput omits `password`; cast the payload — the password is still
        // serialized and sent exactly as typed, never trimmed or normalized.
        const dto = {
          username: username.trim(),
          role: role as UserRole,
          active,
          password,
        };
        await createUser(dto as never);
        setPassword("");
        onSuccess?.();
      } else if (user) {
        if (editVersion === null) {
          onError?.(
            "This user record is missing its version. Reload the page to edit it.",
          );
          return;
        }
        await updateUser(
          user.id,
          { username: username.trim(), role: role as UserRole, active },
          editVersion,
        );
        onSuccess?.();
      }
    } catch (err) {
      if (err instanceof ValidationError) {
        for (const fe of err.fieldErrors) {
          if (fe.field === "username") setUsernameServerError(fe.message);
          if (fe.field === "role") setRoleServerError(fe.message);
        }
      } else if (err instanceof ConflictError) {
        // 409 — never overwrite silently; surface the recovery banner.
        setConflict(true);
      } else if (err instanceof ApiError) {
        onError?.(err.message);
      } else {
        onError?.("Something went wrong. Check your connection and try again.");
      }
    } finally {
      setIsSubmitting(false);
    }
  }

  const cancelAction = onCancel ?? (() => {});
  const refreshAction = onRefresh ?? cancelAction;

  const roleHint = isLastAdmin
    ? LAST_ADMIN_NOTE
    : assignable.length === 0
      ? "You cannot assign any roles with your current permissions."
      : undefined;

  return (
    <section className="upf" aria-labelledby="upf-title">
      <h1 id="upf-title" className="upf__title">
        {isCreate ? "Create User" : "Edit User"}
      </h1>

      {conflict ? (
        <div
          role="alert"
          tabIndex={-1}
          ref={setBannerRef}
          className="upf__banner upf__banner--conflict"
        >
          <span className="upf__banner-icon" aria-hidden="true">
            !
          </span>
          <p className="upf__banner-text">
            This user was modified by another administrator. Review the latest
            changes and try again.
          </p>
          <Button variant="primary" size="md" onClick={() => refreshAction()}>
            Refresh
          </Button>
          <Button variant="ghost" size="md" onClick={() => setConflict(false)}>
            Dismiss
          </Button>
        </div>
      ) : null}

      <form
        className="upf__form"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void handleSubmit();
        }}
      >
        <Field
          label="Username"
          required
          validate={[required("Username"), validateUsername]}
          error={usernameServerError}
          hint={isCreate ? "Used to sign in." : undefined}
        >
          <input
            type="text"
            name="username"
            inputMode="text"
            autoComplete="username"
            maxLength={64}
            value={username}
            onChange={handleUsernameChange}
          />
        </Field>

        {isCreate ? (
          <Field
            label="Password"
            required
            validate={[required("Password"), validatePasswordStrength]}
            hint="You will not be able to view it again."
          >
            <input
              type="password"
              name="password"
              autoComplete="new-password"
              value={password}
              onChange={handlePasswordChange}
            />
          </Field>
        ) : null}

        {isCreate ? (
          <div className="upf__strength" aria-live="polite">
            <span className="upf__strength-label">
              {password ? passwordStrength.text : "Set a password to continue."}
            </span>
            <span
              className="upf__strength-meter"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={passwordStrength.valid ? 100 : password ? 40 : 0}
              aria-label="Password strength"
            >
              <span
                className={`upf__strength-fill upf__strength-fill--${
                  passwordStrength.valid ? "ok" : password ? "part" : "empty"
                }`}
              />
            </span>
          </div>
        ) : null}

        <Field
          label="Role"
          required
          disabled={isLastAdmin || assignable.length === 0}
          hint={roleHint}
          error={roleServerError}
        >
          <select
            name="role"
            value={role ?? ""}
            onChange={handleRoleChange}
          >
            {assignable.length === 0 ? (
              <option value="">No roles you can assign</option>
            ) : (
              ROLE_GROUPS.map((group) => {
                const allowed = group.roles.filter((r) => assignable.includes(r));
                if (allowed.length === 0) return null;
                return (
                  <optgroup key={group.label} label={group.label}>
                    {allowed.map((r) => (
                      <option key={r} value={r}>
                        {ROLE_LABELS[r]}
                      </option>
                    ))}
                  </optgroup>
                );
              })
            )}
          </select>
        </Field>

        {assignable.length === 0 ? (
          <p className="upf__note upf__note--warning">
            You cannot assign any roles with your current permissions.
          </p>
        ) : null}

        {missingEditVersion ? (
          <p className="upf__note upf__note--warning">
            This user record is missing its version. Reload the page to edit it.
          </p>
        ) : null}

        <div className="upf__active">
          <span className="upf__active-label">Account status</span>
          <AccessSwitch
            id="active"
            checked={active}
            onChange={setActive}
            disabled={isLastAdmin}
            describedBy={isLastAdmin ? "active-last-admin-note" : undefined}
          />
          {isLastAdmin ? (
            <p id="active-last-admin-note" className="upf__note upf__note--warning">
              {LAST_ADMIN_NOTE}
            </p>
          ) : null}
        </div>

        <div className="upf__actions">
          <Button variant="ghost" size="md" onClick={() => cancelAction()} type="button">
            Cancel
          </Button>
          <Button
            variant="primary"
            size="lg"
            type="submit"
            disabled={submitDisabled}
            loading={isSubmitting}
          >
            {isCreate ? "Create User" : "Save Changes"}
          </Button>
        </div>
      </form>

      <style>{`
        .upf{max-width:480px;margin:0 auto;padding:0 var(--space-4,16px)}
        .upf__title{margin:0 0 var(--space-6,24px);font-size:var(--text-2xl,24px);font-weight:var(--font-semibold,600);line-height:var(--leading-tight,1.25)}
        .upf__form{display:block}
        .upf__strength{margin-top:var(--space-3,12px);margin-bottom:var(--space-6,24px)}
        .upf__strength-label{font-size:var(--text-sm,14px);color:var(--color-text-muted,#6b7280)}
        .upf__strength-meter{display:block;margin-top:var(--space-1,4px);height:8px;border-radius:var(--radius-full,9999px);background:var(--color-disabled-bg,#e5e7eb);overflow:hidden}
        .upf__strength-fill{display:block;height:100%;border-radius:var(--radius-full,9999px);background:var(--color-warning,#d97706);width:0%;transition:width .2s ease-out}
        .upf__strength-fill--part{width:40%}
        .upf__strength-fill--ok{width:100%;background:var(--color-success,#16a34a)}
        .upf__note{margin:var(--space-1,4px) 0 0;font-size:var(--text-sm,14px)}
        .upf__note--warning{color:var(--color-warning,#92400e)}
        .upf__active{display:flex;align-items:center;gap:var(--space-4,16px);margin-top:var(--space-6,24px);flex-wrap:wrap}
        .upf__active-label{flex:1 1 100%;margin:0;font-size:var(--text-sm,14px);font-weight:var(--font-medium,500);color:var(--color-text,#111827)}
        .upf__switch{display:inline-flex;align-items:center;gap:var(--space-2,8px)}
        .upf__switch__toggle{position:relative;width:52px;height:44px;padding:0;border:0;background:transparent;cursor:pointer;min-width:44px;min-height:44px}
        .upf__switch__thumb{position:absolute;left:0;top:50%;width:44px;height:24px;transform:translateY(-50%);border-radius:var(--radius-full,9999px);background:var(--color-border,#d1d5db);transition:background-color .2s ease-out,transform .2s ease-out}
        .upf__switch__thumb::after{content:"";position:absolute;left:2px;top:2px;width:20px;height:20px;border-radius:50%;background:#fff;box-shadow:var(--shadow-sm,0 1px 2px rgba(0,0,0,0.05));transition:transform .2s ease-out}
        .upf__switch__toggle[aria-checked="true"] .upf__switch__thumb{background:var(--color-primary,#2563eb)}
        .upf__switch__toggle[aria-checked="true"] .upf__switch__thumb::after{transform:translateX(20px)}
        .upf__switch__toggle:focus-visible{outline:2px solid var(--color-primary,#2563eb);outline-offset:2px;border-radius:var(--radius-md,8px)}
        .upf__switch__toggle:disabled{cursor:not-allowed;opacity:.6}
        .upf__switch__label{font-size:var(--text-base,16px);color:var(--color-text,#111827)}
        .upf__actions{display:flex;flex-direction:row-reverse;gap:var(--space-4,16px);margin-top:var(--space-8,32px)}
        .upf__actions .fms-btn{min-width:44px;min-height:44px}
        .upf__banner{display:flex;align-items:center;gap:var(--space-3,12px);padding:var(--space-4,16px);margin-bottom:var(--space-6,24px);border-radius:var(--radius-md,8px);border-left:3px solid var(--color-error,#dc2626);background:#fee2e2;box-shadow:var(--shadow-md,0 4px 6px -1px rgba(0,0,0,0.1));flex-wrap:wrap}
        .upf__banner-icon{flex:0 0 auto;display:inline-flex;align-items:center;justify-content:center;width:24px;height:24px;border-radius:50%;background:var(--color-error,#dc2626);color:#fff;font-weight:var(--font-bold,700)}
        .upf__banner-text{flex:1 1 auto;margin:0;font-size:var(--text-sm,14px);color:var(--color-text,#111827);line-height:var(--leading-normal,1.5)}
        .upf__banner .fms-btn{min-width:44px;min-height:44px}
        @media (max-width:767px){
          .upf{border:none;box-shadow:none}
          .upf__actions{flex-direction:column-reverse}
          .upf__actions .fms-btn--primary{width:100%}
        }
        @media (prefers-reduced-motion:reduce){
          .upf *{transition:none!important}
        }
      `}</style>
    </section>
  );
}