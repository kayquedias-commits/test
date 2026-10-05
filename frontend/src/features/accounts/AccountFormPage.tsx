import { useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";

import { Button } from "../../components/ui/Button";
import { Field } from "../../components/ui/Field";
import { ConflictRecoveryBanner } from "../transactions/ConflictRecoveryBanner";
import {
  createAccount,
  listAccounts,
  updateAccount,
} from "../../services/api/endpoints";
import type { AccountDTO, AccountType, Money } from "../../services/api/types";
import {
  maxLength,
  required,
  validateForm,
  type Validator,
} from "../../utils/validation";
import { CurrencyParseError, parseMinorUnits } from "../../utils/currency";

/**
 * The account list is read via listAccounts (page 1) because there is no
 * dedicated getAccount endpoint; edit mode finds its record by accountId.
 */
const PAGE_SIZE = 100;
/** MVP reads the account's currency as the single base currency. */
const BASE_CURRENCY = "USD";

const ACCOUNT_TYPE_OPTIONS: ReadonlyArray<{ value: AccountType; label: string }> = [
  { value: "checking", label: "Checking" },
  { value: "savings", label: "Savings" },
  { value: "credit", label: "Credit Card" },
  { value: "cash", label: "Cash" },
  { value: "other", label: "Other" },
];

export interface AccountFormPageProps {
  /** Present only in /accounts/:id/edit — enables the optimistic-lock submit. */
  accountId?: string;
  /** When supplied, edit mode uses this DTO as its source instead of a fetch. */
  initialAccount?: AccountDTO;
  /** Base currency for the MVP (display + read-only currency field). */
  baseCurrency?: string;
  /** Fired on successful save so the router wrapper can toast + navigate. */
  onSaved?: (account: AccountDTO) => void;
  /** Fired for the cancel/back affordance. */
  onCancel?: () => void;
}

/**
 * Validates the opening-balance input. It is optional (empty -> 0) and must be
 * a positive decimal with at most 2 decimal places, via parseMinorUnits. This
 * differs from validation.positiveAmount because zero is a permitted opening
 * balance, so positiveAmount would wrongly flag the default.
 */
function openingBalanceValidator(value: string): string | null {
  const raw = value == null ? "" : String(value).trim();
  if (raw === "") return null; // optional -> default 0
  try {
    const units = parseMinorUnits(raw);
    if (units < 0n) {
      return "Opening balance cannot be negative. Enter 0 or a positive amount.";
    }
    return null; // zero (including "0.00") is valid
  } catch (error) {
    if (error instanceof CurrencyParseError) {
      return "Enter an opening balance with at most 2 decimal places, for example 12.50.";
    }
    return "Enter a valid opening balance.";
  }
}

function currencySymbol(currencyCode: string): string {
  try {
    const parts = new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: currencyCode,
    }).formatToParts(0);
    const part = parts.find((p) => p.type === "currency");
    return part ? part.value : currencyCode;
  } catch {
    return currencyCode;
  }
}

/**
 * Account create/edit form (Module A). Composition of Field + Button, blur
 * validation via validation.ts/currency.ts, disabled submit until valid, and a
 * 409 conflict-recovery banner on optimistic-lock failure.
 */
export default function AccountFormPage(
  props: AccountFormPageProps
): ReactElement {
  const { accountId, initialAccount, baseCurrency, onSaved, onCancel } = props;
  const isEdit = Boolean(accountId);

  const nameRef = useRef<HTMLInputElement>(null);
  const typeRef = useRef<HTMLSelectElement>(null);

  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">(
    isEdit ? "loading" : "ready"
  );
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);

  const [name, setName] = useState("");
  const [type, setType] = useState<AccountType>("checking");
  const [openingBalance, setOpeningBalance] = useState("");
  const [version, setVersion] = useState<number | null>(null);
  const [accounts, setAccounts] = useState<AccountDTO[]>([]);

  /** Fields mount only after edit data is seeded (Field captures value at mount). */
  const [formReady, setFormReady] = useState(!isEdit);

  const [submitting, setSubmitting] = useState(false);
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<{
    kind: "conflict" | "denied" | "notfound" | "generic" | "success";
    text: string;
  } | null>(null);

  const current = useMemo<AccountDTO | undefined>(
    () =>
      initialAccount ??
      (isEdit ? accounts.find((a) => a.id === accountId) : undefined),
    [initialAccount, accounts, isEdit, accountId]
  );

  const currency = useMemo(
    () => current?.openingBalance.currency ?? baseCurrency ?? BASE_CURRENCY,
    [current, baseCurrency]
  );

  // Edit mode: load the account list and seed the form once the record resolves.
  useEffect(() => {
    if (!isEdit) {
      setLoadState("ready");
      return;
    }
    const controller = new AbortController();
    setLoadState("loading");
    setLoadError(null);
    listAccounts({ pageSize: PAGE_SIZE }, controller.signal)
      .then((res) => {
        setAccounts(res.items ?? []);
        setLoadState("ready");
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        setLoadError(err && err.message ? err.message : "Couldn't load this account.");
        setLoadState("error");
      });
    return () => controller.abort();
  }, [isEdit, retryNonce]);

  useEffect(() => {
    if (isEdit && loadState === "ready" && current) {
      setName(current.name);
      setType(current.type);
      setOpeningBalance(current.openingBalance.amount);
      setVersion(current.version);
      setFormReady(true);
    }
  }, [isEdit, loadState, current]);

  const nameSchema = useMemo(() => [required("Name"), maxLength(255, "Name")], []);
  const balanceSchema = useMemo<Validator[]>(() => [openingBalanceValidator], []);

  const validation = useMemo(
    () =>
      validateForm(
        { name, openingBalance },
        { name: nameSchema, openingBalance: balanceSchema }
      ),
    [name, openingBalance, nameSchema, balanceSchema]
  );

  const nameHint = name.length === 0 ? "Up to 255 characters." : `${name.length}/255`;
  const balanceHint =
    type === "credit"
      ? "Current balance owed on this card (if any), or 0."
      : "Balance of this account when created. Defaults to 0.";

  const canSubmit =
    formReady &&
    validation.name.length === 0 &&
    validation.openingBalance.length === 0 &&
    loadState === "ready";

  const handleNameChange = (e: ChangeEvent<HTMLInputElement>): void => {
    setName(e.target.value);
    setServerErrors((prev) => {
      if (!prev.name) return prev;
      const next = { ...prev };
      delete next.name;
      return next;
    });
  };

  const handleTypeChange = (e: ChangeEvent<HTMLSelectElement>): void => {
    setType(e.target.value as AccountType);
  };

  const handleBalanceChange = (e: ChangeEvent<HTMLInputElement>): void => {
    setOpeningBalance(e.target.value);
    setServerErrors((prev) => {
      if (!prev.openingBalance) return prev;
      const next = { ...prev };
      delete next.openingBalance;
      return next;
    });
  };

  // Optimistic-lock read-only refetch. Drops local edits and reseeds from the
  // latest server state — never triggers a mutation (no silent overwrite).
  const refreshLatest = (): void => {
    setNotice(null);
    setServerErrors({});
    setFormReady(false);
    setVersion(null);
    setRetryNonce((n) => n + 1);
  };

  const handleSubmit = (e: FormEvent<HTMLFormElement>): void => {
    e.preventDefault();
    setNotice(null);

    if (submitting) return; // double-click no-op

    if (!canSubmit) {
      // Guide the user to the first invalid field instead of a dead button.
      if (validation.name.length > 0) {
        nameRef.current?.focus();
      } else if (validation.openingBalance.length > 0) {
        nameRef.current?.focus();
      }
      return;
    }

    if (isEdit && current) {
      const expected = current.version ?? version;
      if (typeof expected !== "number" || !Number.isInteger(expected) || expected < 0) {
        setNotice({
          kind: "generic",
          text: "The account version is invalid. Reload and try again.",
        });
        return;
      }
    }

    const money: Money = {
      amount: openingBalance.trim() === "" ? "0" : openingBalance.trim(),
      currency,
    };
    const dto = { name: name.trim(), type, openingBalance: money };

    setSubmitting(true);
    const request =
      isEdit && current
        ? updateAccount(current.id, dto, current.version ?? version ?? 0)
        : createAccount(dto);

    request
      .then((saved) => {
        const prefix = isEdit ? "Account updated" : "Account created";
        setNotice({ kind: "success", text: `${prefix} — ${saved.name}` });
        onSaved?.(saved);
      })
      .catch((err) => {
        // Map API errors to hard-coded localized copy; never render raw server text.
        if (err && err.name === "ValidationError" && err.fieldErrors) {
          const mapped: Record<string, string> = {};
          for (const fe of err.fieldErrors) {
            mapped[fe.field ?? "name"] = "Please correct this value and try again.";
          }
          setServerErrors(mapped);
          if (mapped.name) nameRef.current?.focus();
          return;
        }
        if (err && err.name === "ConflictError") {
          setNotice({ kind: "conflict", text: "The account was modified by another user." });
          return;
        }
        if (err && err.name === "ForbiddenError") {
          setNotice({ kind: "denied", text: "You do not have permission to save accounts." });
          return;
        }
        if (err && err.name === "NotFoundError") {
          setNotice({ kind: "notfound", text: "This account no longer exists. Reload to see the latest list." });
          return;
        }
        const ref = err && err.correlationId ? ` Reference: ${err.correlationId}` : "";
        setNotice({ kind: "generic", text: `Saving failed. Please try again.${ref}` });
      })
      .finally(() => setSubmitting(false));
  };

  // Loading (edit only) — never flash empty inputs before data arrives.
  if (isEdit && (loadState === "loading" || !formReady)) {
    return (
      <main className="afp-page" aria-busy="true">
        <h1 className="afp-h1">Edit Account</h1>
        <p className="afp-caption">Loading this account…</p>
      </main>
    );
  }

  // Load failure (edit only) — don't render form fields until data arrives, so
  // the user never edits phantom data and then hits 409/404 on save.
  if (isEdit && loadState === "error") {
    return (
      <main className="afp-page">
        <h1 className="afp-h1">Edit Account</h1>
        <div className="afp-notice afp-notice--error" role="alert" aria-atomic="true">
          <p>{loadError ?? "Couldn't load this account."}</p>
          <div className="afp-actions">
            <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
              Retry
            </Button>
            <Button variant="ghost" onClick={onCancel}>
              Back to accounts
            </Button>
          </div>
        </div>
      </main>
    );
  }

  // Edit mode record is absent from the fetched list (e.g. deleted / wrong id).
  if (isEdit && loadState === "ready" && !formReady && !current) {
    return (
      <main className="afp-page">
        <h1 className="afp-h1">Edit Account</h1>
        <div className="afp-notice afp-notice--error" role="alert" aria-atomic="true">
          <p>This account could not be found. It may have been deleted.</p>
          <div className="afp-actions">
            <Button variant="ghost" onClick={onCancel}>
              Back to accounts
            </Button>
          </div>
        </div>
      </main>
    );
  }

  return (
    <main className="afp-page">
      <h1 className="afp-h1">{isEdit ? "Edit Account" : "Add Account"}</h1>
      <p className="afp-caption">
        {isEdit && current
          ? `Editing: ${current.name}`
          : "Define a financial account."}
      </p>

      {notice && notice.kind === "conflict" && (
        <ConflictRecoveryBanner
          entityName="account"
          onRefresh={refreshLatest}
          disableDismiss
        />
      )}

      {notice &&
        notice.kind !== "conflict" &&
        (notice.kind === "generic" ||
        notice.kind === "denied" ||
        notice.kind === "notfound" ? (
          <div className="afp-notice afp-notice--error" role="alert" aria-atomic="true">
            <p>{notice.text}</p>
            <div className="afp-actions">
              <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
                Retry
              </Button>
            </div>
          </div>
        ) : (
          <div
            className="afp-notice afp-notice--success"
            role="status"
            aria-live="polite"
            aria-atomic="true"
          >
            <p>{notice.text}</p>
          </div>
        ))}

      <form className="afp-card" onSubmit={handleSubmit} noValidate>
        <div className="afp-grid">
          <div className="afp-name">
            <Field
              id="name"
              label="Name"
              required
              validate={nameSchema}
              error={serverErrors.name ?? null}
              hint={nameHint}
            >
              <input
                ref={nameRef}
                id="name"
                type="text"
                value={name}
                onChange={handleNameChange}
                autoComplete="off"
                maxLength={255}
              />
            </Field>
          </div>

          <div className="afp-type">
            <Field id="type" label="Type" required hint="What kind of account is this?">
              <select
                ref={typeRef}
                id="type"
                value={type}
                onChange={handleTypeChange}
              >
                {ACCOUNT_TYPE_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </select>
            </Field>
          </div>

          {/* The input must be the direct Field child so Field drives its blur
              validation / value / aria state; the currency symbol is an adjacent
              positioned sibling overlaying the input's left padding. */}
          <div className="afp-balance">
            <Field
              id="openingBalance"
              label="Opening Balance"
              validate={balanceSchema}
              error={serverErrors.openingBalance ?? null}
              hint={balanceHint}
            >
              <input
                className="afp-money-input"
                id="openingBalance"
                type="text"
                inputMode="decimal"
                value={openingBalance}
                onChange={handleBalanceChange}
                autoComplete="off"
                placeholder="0.00"
              />
            </Field>
            <span className="afp-money-prefix" aria-hidden="true">
              {currencySymbol(currency)}
            </span>
          </div>

          {/* Read-only currency rendered as labeled static text — not a disabled
              input, which would create a dead tab stop and be skipped by SRs. */}
          <div className="afp-currency">
            <div className="afp-field">
              <span className="afp-label">Currency</span>
              <p className="afp-currency-value">
                {currency} <span>(base)</span>
              </p>
            </div>
          </div>
        </div>

        {!canSubmit && (
          <p className="afp-helper--submit">Complete required fields to save.</p>
        )}

        <div className="afp-actions">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant="primary"
            size="lg"
            loading={submitting}
            disabled={!canSubmit || submitting}
          >
            {isEdit ? "Save changes" : "Save Account"}
          </Button>
        </div>
      </form>
    </main>
  );
}

const FMS_ACCOUNT_FORM_CSS = `
.afp-page {
  max-width: 640px;
  margin-inline: auto;
  padding: 24px 16px;
}
.afp-h1 {
  margin: 0 0 8px;
  font-size: 24px;
  font-weight: 600;
  line-height: 1.25;
  color: var(--color-text-primary, #0f172a);
}
.afp-caption {
  margin: 0 0 24px;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.afp-card {
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1), 0 2px 4px -1px rgba(0,0,0,0.06);
  padding: 24px;
}
.afp-grid {
  display: grid;
  grid-template-columns: 1fr;
  gap: 8px 16px;
}
@media (min-width: 768px) {
  .afp-grid {
    grid-template-columns: 1fr 1fr;
  }
}
.afp-name { grid-column: 1 / -1; }
@media (min-width: 768px) {
  .afp-type { grid-column: 1; }
  .afp-balance { grid-column: 2; }
  .afp-currency { grid-column: 1 / -1; }
}
.afp-balance {
  position: relative;
}
/* Currency symbol overlays the input's left padding, vertically centered on
   the 44px control. The control sits below the label (14px/21px line +
   8px margin), giving the top offset asserted here. */
.afp-money-prefix {
  position: absolute;
  left: 12px;
  top: 51px;
  transform: translateY(-50%);
  color: var(--color-text-muted, #64748b);
  pointer-events: none;
}
.afp-balance .afp-money-input {
  padding-left: 32px; /* space-8 indent so the symbol does not overlap text */
}
.afp-field {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.afp-label {
  font-size: 14px;
  font-weight: 500;
  color: var(--color-text-secondary, #475569);
}
.afp-currency-value {
  margin: 0;
  min-height: 44px;
  display: flex;
  align-items: center;
  padding: 0 14px;
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  background: var(--color-bg-subtle, #f8fafc);
  color: var(--color-text-muted, #64748b);
  font-size: 16px;
  line-height: 1.5;
}
.afp-helper--submit {
  margin: 16px 0 0;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.afp-actions {
  display: flex;
  gap: 12px;
  margin-top: 32px;
  flex-wrap: wrap;
}
@media (max-width: 767px) {
  .afp-actions {
    flex-direction: column;
  }
  .afp-actions > * {
    flex: 1 1 100%;
    justify-content: center;
  }
}
.afp-notice {
  padding: 16px;
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  margin-bottom: 16px;
}
.afp-notice p {
  margin: 0 0 12px;
  font-size: 14px;
  line-height: 1.5;
}
.afp-notice p:only-child {
  margin: 0;
}
.afp-notice--error {
  background: var(--color-danger-bg, #fef2f2);
  border-color: var(--color-danger, #dc2626);
  color: var(--color-danger, #dc2626);
}
.afp-notice--success {
  background: #f0fdf4;
  border-color: #16a34a;
  color: #166534;
}
@media (prefers-reduced-motion: reduce) {
  .afp-balance .afp-money-input,
  .afp-currency-value {
    transition: none;
  }
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-account-form-styles")) {
  const style = document.createElement("style");
  style.id = "fms-account-form-styles";
  style.textContent = FMS_ACCOUNT_FORM_CSS;
  document.head.appendChild(style);
}