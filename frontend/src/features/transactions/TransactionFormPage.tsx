import { useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";

import { Button } from "../../components/ui/Button";
import { Field } from "../../components/ui/Field";
import { ConflictRecoveryBanner } from "./ConflictRecoveryBanner";
import { listAccounts, listCategories } from "../../services/api/endpoints";
import type {
  AccountDTO,
  CategoryDTO,
  Money,
  TransactionCreateInput,
  TransactionDTO,
  TransactionType,
} from "../../services/api/types";
import {
  selectIsMutating,
  selectLastConflict,
  selectMutationStatus,
  selectTransactionById,
  statusFor,
  useTransactionsStore,
} from "../../app/stores/transactions";
import { parseIsoDate, todayInLocale } from "../../utils/date";
import {
  categoryRequired,
  futureDateAllowed,
  maxLength,
  positiveAmount,
  required,
  validDate,
  type Validator,
} from "../../utils/validation";

const PAGE_SIZE = 100;
const LOCALE = "en-US";
/** MVP reads transactions in the single base currency. */
const BASE_CURRENCY = "USD";

const INCOME = "INCOME" as TransactionType;
const EXPENSE = "EXPENSE" as TransactionType;

/**
 * Financial Transaction create/edit form (Module B). Composition of Field +
 * Button for account/date/description, with custom masked amount, category
 * multi-select, and derived/explicit type controls. Optimistic submit route
 * through the transactions store (rollback on error, banner on HTTP 409).
 */
export interface TransactionFormPageProps {
  /** Present only in /transactions/:id/edit — enables the optimistic-lock submit. */
  transactionId?: string;
  /** Fired on successful create/update so the router can toast + navigate. */
  onSaved?: (transaction?: TransactionDTO) => void;
  /** Fired for the cancel/back affordance. */
  onCancel?: () => void;
}

/** Run validators in order; return the first failing message, else "". */
function firstError(validators: Validator<string>[], value: string): string {
  for (const validator of validators) {
    const message = validator(value);
    if (message) return message;
  }
  return "";
}

export default function TransactionFormPage(
  props: TransactionFormPageProps
): ReactElement {
  const { transactionId, onSaved, onCancel } = props;
  const isEdit = Boolean(transactionId);

  // --- Load reference data (accounts + categories) ---------------------------
  const [accounts, setAccounts] = useState<AccountDTO[]>([]);
  const [categories, setCategories] = useState<CategoryDTO[]>([]);
  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">("loading");
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);
  /** Fields mount only after edit data is seeded (Field captures value at mount). */
  const [formReady, setFormReady] = useState(!isEdit);

  // --- Field values ----------------------------------------------------------
  const [accountId, setAccountId] = useState("");
  const [amountRaw, setAmountRaw] = useState("");
  const [amountTouched, setAmountTouched] = useState(false);
  const [dateValue, setDateValue] = useState("");
  const [selectedCategories, setSelectedCategories] = useState<string[]>([]);
  const [categoryTouched, setCategoryTouched] = useState(false);
  const [explicitType, setExplicitType] = useState<TransactionType>(EXPENSE);
  const [description, setDescription] = useState("");
  const [version, setVersion] = useState<number | null>(null);
  const [scheduledOverride, setScheduledOverride] = useState<boolean | null>(null);

  const [notice, setNotice] = useState<{
    kind: "conflict" | "denied" | "notfound" | "generic" | "success";
    text: string;
  } | null>(null);
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});

  // --- Store-derived mutation state ------------------------------------------
  const isMutating = useTransactionsStore(selectIsMutating);
  const mutationStatus = useTransactionsStore(selectMutationStatus);
  const lastConflict = useTransactionsStore(selectLastConflict);
  const editTx = useTransactionsStore((s) => selectTransactionById(s, transactionId ?? ""));
  const firstPageStatus = useTransactionsStore((s) => statusFor(s, null));

  // Load accounts + categories (single fetch, abortable on retry/unmount).
  useEffect(() => {
    const controller = new AbortController();
    setLoadState("loading");
    setLoadError(null);
    Promise.all([
      listAccounts({ pageSize: PAGE_SIZE }, controller.signal),
      listCategories({ pageSize: PAGE_SIZE }, controller.signal),
    ])
      .then(([accts, cats]) => {
        setAccounts(accts.items ?? []);
        setCategories(cats.items ?? []);
        setLoadState("ready");
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        setLoadError(err && err.message ? err.message : "Couldn't load accounts and categories.");
        setLoadState("error");
      });
    return () => controller.abort();
  }, [retryNonce]);

  // Edit mode: populate the store's first page so editTx resolves.
  useEffect(() => {
    if (!isEdit) return;
    void useTransactionsStore.getState().loadPage(null);
  }, [isEdit]);

  // Seed fields once edit data + option list resolve.
  useEffect(() => {
    if (!isEdit || loadState !== "ready" || !editTx || formReady) return;
    setAccountId(editTx.accountId);
    setAmountRaw(editTx.amount.amount);
    setDateValue(editTx.date);
    setSelectedCategories([...editTx.categoryIds]);
    setExplicitType(editTx.type);
    setDescription(editTx.description);
    setVersion(editTx.version);
    setScheduledOverride(editTx.scheduled);
    setFormReady(true);
  }, [isEdit, loadState, editTx, formReady]);

  // --- Scheduled tag ----------------------------------------------------------
  const todayMs = useMemo(
    () => parseIsoDate(todayInLocale(LOCALE))?.getTime() ?? 0,
    []
  );
  const selectedMs = dateValue ? (parseIsoDate(dateValue)?.getTime() ?? 0) : 0;
  const userRequestedScheduled = selectedMs > todayMs;
  const scheduled = scheduledOverride ?? userRequestedScheduled;

  // Polite live-region announcement when the tag appears. Static text only —
  // never echoes a raw/typed date string into a live region.
  const [scheduledNotice, setScheduledNotice] = useState<string | null>(null);
  const prevScheduled = useRef(scheduled);
  useEffect(() => {
    if (scheduled && !prevScheduled.current) {
      setScheduledNotice("This transaction is scheduled for a future date");
    } else if (!scheduled && prevScheduled.current) {
      setScheduledNotice(null);
    }
    prevScheduled.current = scheduled;
  }, [scheduled]);

  // --- Per-field validation ----------------------------------------------------
  const amountSchema = useMemo<Validator<string>[]>(
    () => [required("Amount"), positiveAmount()],
    []
  );
  const dateSchema = useMemo<Validator<string>[]>(
    () => [required("Date"), validDate(), futureDateAllowed(scheduled, { locale: LOCALE })],
    [scheduled]
  );
  const descSchema = useMemo<Validator<string>[]>(
    () => [required("Description"), maxLength(255, "Description")],
    []
  );

  const amountError = firstError(amountSchema, amountRaw);
  const dateError = firstError(dateSchema, dateValue);
  const descError = firstError(descSchema, description);
  const categoryError = categoryRequired()(selectedCategories) ?? "";

  // --- Derived type (income/expense) from selected categories -------------------
  const derivedType = useMemo<TransactionType | null>(() => {
    const types = new Set(
      selectedCategories
        .map((id) => categories.find((c) => c.id === id)?.type)
        .filter(Boolean)
    );
    return types.size === 1 ? ([...types][0] as TransactionType) : null;
  }, [selectedCategories, categories]);
  const showTypeToggle = selectedCategories.length > 0 && derivedType === null;
  const resolvedType = derivedType ?? explicitType;

  // Defense-in-depth: only ids returned by the endpoints may be submitted, so a
  // forged/tampered option never reaches the API (the server stays authoritative).
  const validAccountIds = useMemo(() => new Set(accounts.map((a) => a.id)), [accounts]);
  const validCategoryIds = useMemo(() => new Set(categories.map((c) => c.id)), [categories]);

  const canSubmit =
    formReady &&
    loadState === "ready" &&
    validAccountIds.has(accountId) &&
    amountError === "" &&
    dateError === "" &&
    descError === "" &&
    selectedCategories.length > 0 &&
    selectedCategories.every((id) => validCategoryIds.has(id)) &&
    !isMutating;

  // --- Handlers ----------------------------------------------------------------
  const handleAmountChange = (e: ChangeEvent<HTMLInputElement>): void => {
    const raw = e.target.value;
    let cleaned = raw.replace(/[^0-9.]/g, "");
    if (cleaned.startsWith(".")) cleaned = `0${cleaned}`;
    const parts = cleaned.split(".");
    if (parts.length > 1) {
      const integer = parts[0];
      const decimal = parts.slice(1).join("").slice(0, 2);
      cleaned = `${integer}.${decimal}`;
    }
    setAmountRaw(cleaned);
    setServerErrors((prev) => {
      if (!prev.amount) return prev;
      const next = { ...prev };
      delete next.amount;
      return next;
    });
  };

  const handleDateChange = (e: ChangeEvent<HTMLInputElement>): void => {
    const value = e.target.value;
    setDateValue(value);
    const ms = value ? (parseIsoDate(value)?.getTime() ?? 0) : 0;
    setScheduledOverride(ms > todayMs);
    setServerErrors((prev) => {
      if (!prev.date) return prev;
      const next = { ...prev };
      delete next.date;
      return next;
    });
  };

  const toggleCategory = (id: string): void => {
    setSelectedCategories((prev) =>
      prev.includes(id) ? prev.filter((c) => c !== id) : [...prev, id]
    );
  };

  const buildPayload = (): TransactionCreateInput => {
    const money: Money = { amount: amountRaw.trim(), currency: BASE_CURRENCY };
    return {
      accountId,
      categoryIds: [...selectedCategories],
      amount: money,
      date: dateValue,
      description: description.trim(),
      type: resolvedType,
      scheduled,
    };
  };

  // Optimistic-lock read-only refetch after a 409: drop local edits, reseed
  // from the latest server state. Never triggers a mutation (no overwrite).
  const refreshLatest = (): void => {
    setNotice(null);
    setServerErrors({});
    setFormReady(false);
    setVersion(null);
    useTransactionsStore.getState().resolveConflict("refresh");
  };

  const handleSubmit = (e: FormEvent<HTMLFormElement>): void => {
    e.preventDefault();
    setNotice(null);
    if (isMutating) return; // double-click no-op

    if (!canSubmit) return; // disabled until valid; guard Enter too

    // Whitelist the chosen ids against the fetched option sets (security).
    if (!validAccountIds.has(accountId)) {
      setNotice({
        kind: "generic",
        text: "The selected account is no longer available. Reload and try again.",
      });
      return;
    }
    if (!selectedCategories.every((id) => validCategoryIds.has(id))) {
      setNotice({
        kind: "generic",
        text: "One of the selected categories is no longer available. Reload and try again.",
      });
      return;
    }

    const payload = buildPayload();

    if (isEdit) {
      const expected = editTx?.version ?? version;
      if (typeof expected !== "number" || !Number.isInteger(expected) || expected < 0) {
        setNotice({
          kind: "generic",
          text: "The transaction version is invalid. Reload and try again.",
        });
        return;
      }
      void useTransactionsStore.getState().applyEdit(
        transactionId as string,
        payload,
        expected
      );
    } else {
      void useTransactionsStore.getState().applyCreate(payload);
    }
  };

  // Observe the store's mutation outcome (optimistic success / rollback-on-error).
  useEffect(() => {
    if (mutationStatus !== "success" && mutationStatus !== "error") return;
    if (mutationStatus === "success") {
      setNotice({
        kind: "success",
        text: isEdit ? "Transaction updated." : "Transaction added.",
      });
      onSaved?.(isEdit ? (editTx ?? undefined) : undefined);
    } else if (mutationStatus === "error") {
      // Rollback is handled inside the store; the user's input is preserved.
      setNotice({
        kind: "generic",
        text: "Couldn't save transaction. Check your connection and try again.",
      });
    }
  }, [mutationStatus, isEdit, onSaved, editTx]);

  // --- Render: loading (both modes) — reserves height, never flashes blank ---
  if (loadState === "loading" || (isEdit && !formReady && firstPageStatus === "loading")) {
    return (
      <main className="txf-page" aria-busy="true">
        <h1 className="txf-h1">{isEdit ? "Edit Transaction" : "New Transaction"}</h1>
        <p className="txf-caption">Loading…</p>
      </main>
    );
  }

  // --- Render: load failure ----------------------------------------------------
  if (loadState === "error") {
    return (
      <main className="txf-page">
        <h1 className="txf-h1">{isEdit ? "Edit Transaction" : "New Transaction"}</h1>
        <div className="txf-notice txf-notice--error" role="alert" aria-atomic="true">
          <p>{loadError ?? "Couldn't load accounts and categories."}</p>
          <div className="txf-actions">
            <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
              Retry
            </Button>
            <Button variant="ghost" onClick={onCancel}>
              Back to transactions
            </Button>
          </div>
        </div>
      </main>
    );
  }

  // --- Render: edit record missing --------------------------------------------
  if (isEdit && loadState === "ready" && firstPageStatus === "ready" && !editTx) {
    return (
      <main className="txf-page">
        <h1 className="txf-h1">Edit Transaction</h1>
        <div className="txf-notice txf-notice--error" role="alert" aria-atomic="true">
          <p>This transaction could not be found. It may have been deleted.</p>
          <div className="txf-actions">
            <Button variant="ghost" onClick={onCancel}>
              Back to transactions
            </Button>
          </div>
        </div>
      </main>
    );
  }

  const showConflict = lastConflict !== null && mutationStatus === "conflict";

  return (
    <main className="txf-page">
      <h1 className="txf-h1">{isEdit ? "Edit Transaction" : "New Transaction"}</h1>
      <p className="txf-caption">
        {isEdit && editTx
          ? "Editing this transaction."
          : "Record a movement of money."}
      </p>

      {showConflict && (
        <ConflictRecoveryBanner
          entityName="transaction"
          onRefresh={refreshLatest}
          disableDismiss
        />
      )}

      {notice && notice.kind === "conflict" ? null : notice ? (
        notice.kind === "generic" ||
        notice.kind === "denied" ||
        notice.kind === "notfound" ? (
          <div className="txf-notice txf-notice--error" role="alert" aria-atomic="true">
            <p>{notice.text}</p>
            <div className="txf-actions">
              <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
                Retry
              </Button>
            </div>
          </div>
        ) : (
          <div
            className="txf-notice txf-notice--success"
            role="status"
            aria-live="polite"
            aria-atomic="true"
          >
            <p>{notice.text}</p>
          </div>
        )
      ) : null}

      <form className="txf-card" onSubmit={handleSubmit} noValidate>
        <div className="txf-field">
          <Field
            id="account"
            label="Account"
            required
            error={serverErrors.account ?? null}
          >
            <select
              id="account"
              value={accountId}
              onChange={(e) => {
                setAccountId(e.target.value);
                setServerErrors((prev) => {
                  if (!prev.account) return prev;
                  const next = { ...prev };
                  delete next.account;
                  return next;
                });
              }}
            >
              <option value="">Select an account</option>
              {accounts.map((account) => (
                <option key={account.id} value={account.id}>
                  {account.name}
                </option>
              ))}
            </select>
          </Field>
        </div>

        {/* Amount — custom control: Field would overwrite the masked value with
            its own internal state, so it cannot own this input. */}
        <div className="txf-field txf-amount">
          <label className="txf-label" htmlFor="txf-amount">
            Amount <span className="txf-required">Required</span>
          </label>
          <div className="txf-control-wrap">
            <span className="txf-money-prefix" aria-hidden="true">
              $
            </span>
            <input
              id="txf-amount"
              className="txf-money-input"
              type="text"
              inputMode="decimal"
              autoComplete="off"
              placeholder="0.00"
              value={amountRaw}
              onChange={handleAmountChange}
              onBlur={() => setAmountTouched(true)}
              aria-invalid={amountTouched && (amountError || serverErrors.amount) ? true : undefined}
              aria-describedby={amountTouched && (amountError || serverErrors.amount) ? "txf-amount-message" : undefined}
            />
          </div>
          {amountTouched && (amountError || serverErrors.amount) && (
            <p
              id="txf-amount-message"
              className="txf-field__message txf-field__message--error"
              role="alert"
              aria-live="polite"
            >
              {amountError || serverErrors.amount}
            </p>
          )}
        </div>

        {/* Date — the future-date "Scheduled" tag is a sibling of the Field. */}
        <div className="txf-date-row">
          <div className="txf-field txf-date">
            <Field
              id="date"
              label="Date"
              required
              validate={dateSchema}
              error={serverErrors.date ?? null}
            >
              <input
                id="date"
                type="date"
                value={dateValue}
                onChange={handleDateChange}
              />
            </Field>
          </div>
          {scheduled ? (
            <>
              <span role="status" aria-live="polite" className="txf-scheduled-tag">
                Scheduled
              </span>
              {scheduledNotice && (
                <span className="txf-sr" aria-live="polite">
                  {scheduledNotice}
                </span>
              )}
            </>
          ) : null}
        </div>

        {/* Category — custom multi-select (Field holds a single string). */}
        <fieldset className="txf-category" onBlur={() => setCategoryTouched(true)}>
          <legend className="txf-label">
            Category <span className="txf-required">Required</span>
          </legend>
          <div className="txf-chips">
            {categories.length === 0 ? (
              <p className="txf-empty">No categories available yet.</p>
            ) : (
              categories.map((category) => {
                const checked = selectedCategories.includes(category.id);
                return (
                  <label
                    key={category.id}
                    className={checked ? "txf-chip txf-chip--on" : "txf-chip"}
                  >
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={() => toggleCategory(category.id)}
                    />
                    <span className="txf-chip-check" aria-hidden="true">
                      {checked ? "✓" : ""}
                    </span>
                    <span className="txf-chip-type" aria-hidden="true">
                      {category.type === INCOME ? "Income" : "Expense"}
                    </span>
                    <span>{category.name}</span>
                  </label>
                );
              })
            )}
          </div>
          {categoryTouched && categoryError && (
            <p
              className="txf-field__message txf-field__message--error"
              role="alert"
              aria-live="polite"
            >
              {categoryError}
            </p>
          )}
        </fieldset>

        {/* Type — segmented control shown only when categories are ambiguous. */}
        {showTypeToggle && (
          <div
            role="radiogroup"
            aria-labelledby="txf-type-legend"
            className="txf-toggle"
          >
            <span id="txf-type-legend" className="txf-label">
              Type
            </span>
            <div className="txf-toggle-row">
              {([INCOME, EXPENSE] as TransactionType[]).map((type) => {
                const active = resolvedType === type;
                return (
                  <label
                    key={type}
                    role="radio"
                    aria-checked={active}
                    className={
                      active ? "txf-toggle-seg txf-toggle-seg--on" : "txf-toggle-seg"
                    }
                  >
                    <input
                      type="radio"
                      name="txf-type"
                      value={type}
                      checked={active}
                      onChange={() => setExplicitType(type)}
                    />
                    <span className="txf-toggle-check" aria-hidden="true">
                      {active ? "✓" : ""}
                    </span>
                    <span>{type === INCOME ? "Income" : "Expense"}</span>
                  </label>
                );
              })}
            </div>
          </div>
        )}

        {/* Description — Field owns maxLength + counter hint. */}
        <div className="txf-field txf-desc">
          <Field
            id="description"
            label="Description"
            required
            validate={descSchema}
            error={serverErrors.description ?? null}
            hint={
              description.length === 0
                ? "Up to 255 characters."
                : `${description.length}/255`
            }
          >
            <textarea
              id="description"
              rows={3}
              maxLength={255}
              placeholder="What was this transaction for?"
              value={description}
              onChange={(e) => {
                setDescription(e.target.value);
                setServerErrors((prev) => {
                  if (!prev.description) return prev;
                  const next = { ...prev };
                  delete next.description;
                  return next;
                });
              }}
            />
          </Field>
        </div>

        {!canSubmit && (
          <p className="txf-helper--submit">Complete the required fields to save.</p>
        )}

        <div className="txf-actions">
          <Button variant="ghost" onClick={onCancel} disabled={isMutating}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant="primary"
            size="lg"
            loading={isMutating}
            disabled={!canSubmit}
          >
            {isEdit ? "Save changes" : "Add transaction"}
          </Button>
        </div>
      </form>
    </main>
  );
}

const FMS_TRANSACTION_FORM_CSS = `
.txf-page {
  max-width: 640px;
  margin-inline: auto;
  padding: 24px 16px;
}
.txf-h1 {
  margin: 0 0 8px;
  font-size: 24px;
  font-weight: 600;
  line-height: 1.25;
  color: var(--color-text-primary, #0f172a);
}
.txf-caption {
  margin: 0 0 24px;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.txf-card {
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1), 0 2px 4px -1px rgba(0,0,0,0.06);
  padding: 24px;
}
.txf-field { margin-bottom: 24px; }
.txf-label {
  display: block;
  font-size: 14px;
  font-weight: 500;
  color: var(--color-text-secondary, #475569);
  margin-bottom: 8px;
}
.txf-required {
  margin-left: 8px;
  font-size: 12px;
  font-weight: 400;
  color: var(--color-text-muted, #64748b);
}
.txf-field__message {
  min-height: 20px;
  margin: 4px 0 0;
  font-size: 14px;
  font-weight: 500;
  line-height: 1.5;
  color: var(--color-danger, #dc2626);
}
.txf-amount { position: relative; }
.txf-control-wrap { position: relative; }
.txf-money-input {
  width: 100%;
  min-height: 44px;
  padding: 8px 12px 8px 36px;
  font-size: 20px;
  line-height: 1.5;
  text-align: right;
  color: var(--color-text-primary, #0f172a);
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  box-sizing: border-box;
}
.txf-money-input:focus-visible {
  outline: 3px solid rgba(37,99,235,0.35);
  outline-offset: 2px;
  border-color: var(--color-primary, #2563eb);
}
.txf-money-prefix {
  position: absolute;
  left: 12px;
  top: 50%;
  transform: translateY(-50%);
  font-size: 20px;
  color: var(--color-text-muted, #64748b);
  pointer-events: none;
}
.txf-date-row {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin-bottom: 24px;
}
.txf-date { flex: 1; margin-bottom: 0; }
.txf-scheduled-tag {
  flex: none;
  margin-top: 2px;
  padding: 4px 12px;
  border-radius: 9999px;
  border: 1px solid rgba(37,99,235,0.35);
  background: rgba(37,99,235,0.12);
  color: var(--color-primary, #2563eb);
  font-size: 14px;
  font-weight: 600;
  line-height: 24px;
  white-space: nowrap;
}
.txf-sr {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
  border: 0;
}
.txf-category { border: 0; padding: 0; margin: 0 0 24px; min-width: 0; }
.txf-chips { display: flex; flex-wrap: wrap; gap: 8px; }
.txf-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 44px;
  padding: 8px 12px;
  border-radius: 9999px;
  border: 1px solid var(--color-border, #cbd5e1);
  background: var(--color-bg-subtle, #f8fafc);
  color: var(--color-text-primary, #0f172a);
  font-size: 14px;
  line-height: 1.5;
  cursor: pointer;
}
.txf-chip input,
.txf-toggle-seg input {
  position: absolute;
  opacity: 0;
  width: 1px;
  height: 1px;
}
.txf-chip:focus-within {
  outline: 3px solid rgba(37,99,235,0.35);
  outline-offset: 2px;
}
.txf-chip--on {
  background: var(--color-primary, #2563eb);
  border-color: var(--color-primary, #2563eb);
  color: #ffffff;
}
.txf-chip-check {
  width: 18px;
  text-align: center;
  font-weight: 700;
  color: var(--color-text-muted, #64748b);
}
.txf-chip--on .txf-chip-check { color: #ffffff; }
.txf-chip-type {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text-muted, #64748b);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 4px;
  padding: 0 4px;
}
.txf-chip--on .txf-chip-type {
  color: #ffffff;
  border-color: rgba(255,255,255,0.6);
}
.txf-empty { margin: 0; font-size: 14px; color: var(--color-text-muted, #64748b); }
.txf-toggle { margin-bottom: 24px; }
.txf-toggle-row { display: flex; flex-wrap: wrap; gap: 8px; }
.txf-toggle-seg {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: 44px;
  padding: 8px 16px;
  border-radius: 8px;
  border: 1px solid var(--color-border, #cbd5e1);
  background: var(--color-surface, #ffffff);
  color: var(--color-text-primary, #0f172a);
  font-size: 16px;
  font-weight: 500;
  cursor: pointer;
}
.txf-toggle-seg:focus-within {
  outline: 3px solid rgba(37,99,235,0.35);
  outline-offset: 2px;
}
.txf-toggle-seg--on {
  background: var(--color-primary, #2563eb);
  border-color: var(--color-primary, #2563eb);
  color: #ffffff;
}
.txf-toggle-check {
  width: 18px;
  text-align: center;
  font-weight: 700;
  color: var(--color-text-muted, #64748b);
}
.txf-toggle-seg--on .txf-toggle-check { color: #ffffff; }
.txf-helper--submit {
  margin: 16px 0 0;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.txf-actions {
  display: flex;
  gap: 12px;
  margin-top: 24px;
  flex-wrap: wrap;
}
@media (max-width: 767px) {
  .txf-actions { flex-direction: column; }
  .txf-actions > * { flex: 1 1 100%; justify-content: center; }
}
.txf-notice {
  padding: 16px;
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  margin-bottom: 16px;
}
.txf-notice p { margin: 0 0 12px; font-size: 14px; line-height: 1.5; }
.txf-notice p:only-child { margin: 0; }
.txf-notice--error {
  background: var(--color-danger-bg, #fef2f2);
  border-color: var(--color-danger, #dc2626);
  color: var(--color-danger, #dc2626);
}
.txf-notice--success {
  background: #f0fdf4;
  border-color: #16a34a;
  color: #166534;
}
@media (min-width: 768px) {
  .txf-card { margin-inline: auto; }
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-transaction-form-styles")) {
  const style = document.createElement("style");
  style.id = "fms-transaction-form-styles";
  style.textContent = FMS_TRANSACTION_FORM_CSS;
  document.head.appendChild(style);
}