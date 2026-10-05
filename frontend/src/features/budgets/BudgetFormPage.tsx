import { useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";

import { Button } from "../../components/ui/Button";
import { Field } from "../../components/ui/Field";
import {
  createBudget,
  listBudgets,
  listCategories,
  updateBudget,
} from "../../services/api/endpoints";
import type {
  BudgetDTO,
  BudgetPeriodType,
  CategoryDTO,
  Money,
} from "../../services/api/types";
import {
  positiveAmount,
  rangeValid,
  required,
  validDate,
  validateForm,
  type Validator,
} from "../../utils/validation";
import {
  addMonths,
  endOfMonth,
  formatDate,
  isPastPeriod,
  startOfMonth,
  toIsoDate,
} from "../../utils/date";
import { useSession, selectUser } from "../../app/stores/session";
import { useBudgetStore } from "../../app/stores/budget";

/**
 * Budgets are read via listBudgets (page 1) because there is no dedicated
 * getBudget endpoint; edit mode finds its record by budgetId.
 */
const PAGE_SIZE = 100;
/** MVP reads the single base currency (single-timezone / single-currency). */
const BASE_CURRENCY = "USD";
const LOCALE = "en-US";
const TIME_ZONE = "UTC";

const PERIOD_TYPE_OPTIONS: ReadonlyArray<{ value: BudgetPeriodType; label: string }> = [
  { value: "MONTHLY", label: "Monthly" },
  { value: "QUARTERLY", label: "Quarterly" },
  { value: "ANNUAL", label: "Annual" },
];

/**
 * Default period window for a period type, applied when the type changes so
 * the user rarely has to pick dates manually (efficiency accelerant).
 * - ANNUAL: Jan 1 – Dec 31 of the current year.
 * - QUARTERLY: first day of the current quarter to the last day of it.
 * - MONTHLY: first day of the current month to the last day of it.
 */
function defaultWindow(periodType: BudgetPeriodType): { start: string; end: string } {
  const now = new Date();
  if (periodType === "ANNUAL") {
    const start = new Date(Date.UTC(now.getUTCFullYear(), 0, 1));
    return { start: toIsoDate(start), end: toIsoDate(endOfMonth(addMonths(start, 11))) };
  }
  if (periodType === "QUARTERLY") {
    const month = Math.floor(now.getUTCMonth() / 3) * 3;
    const start = startOfMonth(new Date(Date.UTC(now.getUTCFullYear(), month, 1)));
    return { start: toIsoDate(start), end: toIsoDate(endOfMonth(addMonths(start, 2))) };
  }
  const start = startOfMonth(now);
  return { start: toIsoDate(start), end: toIsoDate(endOfMonth(start)) };
}

export interface BudgetFormPageProps {
  /** Present only in /budgets/:id/edit — enables the optimistic-lock submit. */
  budgetId?: string;
  /** When supplied, edit mode uses this DTO as its source instead of a fetch. */
  initialBudget?: BudgetDTO;
  /** Base currency for the MVP (display + read-only currency field). */
  baseCurrency?: string;
  /** Fired on successful save so the router wrapper can toast + navigate. */
  onSaved?: (budget: BudgetDTO) => void;
  /** Fired for the cancel/back affordance. */
  onCancel?: () => void;
}

/**
 * Budget create/edit form (Module D). Composition of Field + Button, blur
 * validation via validation.ts, disabled submit until valid, a dynamic period
 * window preview ("Covers Jan 1 – Mar 31"), and a 409 conflict-recovery banner
 * on optimistic-lock failure.
 *
 * The `BackdatedBudgetsEnabled` session-settings flag is not part of this
 * dependency contract, so the module defaults it to `false` (the secure
 * Module-D boundary: past-period budgets are blocked in the client). The
 * server remains the authoritative gate; this disables only the start-date
 * picker min + an inline message, never an authorization decision.
 */
export default function BudgetFormPage(props: BudgetFormPageProps): ReactElement {
  const { budgetId, initialBudget, baseCurrency, onSaved, onCancel } = props;
  const isEdit = Boolean(budgetId);

  const categoryRef = useRef<HTMLSelectElement>(null);
  const amountRef = useRef<HTMLInputElement>(null);

  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">(
    isEdit ? "loading" : "ready"
  );
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);

  const [categories, setCategories] = useState<CategoryDTO[]>([]);
  const [budgets, setBudgets] = useState<BudgetDTO[]>([]);

  const [category, setCategory] = useState("");
  const [periodType, setPeriodType] = useState<BudgetPeriodType>("MONTHLY");
  const [periodStart, setPeriodStart] = useState("");
  const [periodEnd, setPeriodEnd] = useState("");
  const [allocated, setAllocated] = useState("");
  const [version, setVersion] = useState<number | null>(null);

  /** Fields mount only after edit data is seeded (Field captures value at mount). */
  const [formReady, setFormReady] = useState(!isEdit);

  const [submitting, setSubmitting] = useState(false);
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<{
    kind: "conflict" | "denied" | "notfound" | "generic" | "success";
    text: string;
  } | null>(null);

  const sessionUser = useSession(selectUser);
  const canWrite = useMemo(
    () => (sessionUser?.permissions ?? []).includes("budget:write"),
    [sessionUser]
  );

  /** Read once at mount and never flip mid-edit (see module docstring). */
  const backdatedEnabled = useMemo<boolean>(() => false, []);

  const current = useMemo<BudgetDTO | undefined>(
    () =>
      initialBudget ??
      (isEdit ? budgets.find((b) => b.id === budgetId) : undefined),
    [initialBudget, budgets, isEdit, budgetId]
  );

  const currency = useMemo(
    () => current?.allocated.currency ?? baseCurrency ?? BASE_CURRENCY,
    [current, baseCurrency]
  );

  // Edit mode: load categories + budgets and seed the form once the record
  // resolves. New mode still loads categories for the select populating.
  useEffect(() => {
    const controller = new AbortController();
    setLoadState(isEdit ? "loading" : "ready");
    setLoadError(null);

    const load = async (): Promise<void> => {
      const [cats, budgetsPage] = await Promise.all([
        listCategories({ pageSize: PAGE_SIZE }, controller.signal),
        isEdit ? listBudgets({ pageSize: PAGE_SIZE }, controller.signal) : Promise.resolve(undefined),
      ]);
      setCategories(cats.items ?? []);
      if (budgetsPage) setBudgets(budgetsPage.items ?? []);
      setLoadState("ready");
    };

    load().catch((err) => {
      if (err && err.name === "AbortError") return;
      setLoadError(err && err.message ? err.message : "Couldn't load budget data.");
      setLoadState("error");
    });

    return () => controller.abort();
  }, [isEdit, retryNonce]);

  useEffect(() => {
    if (isEdit && loadState === "ready" && current) {
      setCategory(current.categoryId);
      setPeriodType(current.periodType);
      setPeriodStart(current.periodStart);
      setPeriodEnd(current.periodEnd);
      setAllocated(current.allocated.amount);
      setVersion(current.version);
      setFormReady(true);
      categoryRef.current?.focus();
    }
  }, [isEdit, loadState, current]);

  const categorySchema = useMemo(() => [required("Category")], []);
  const startSchema = useMemo<Validator[]>(() => [validDate()], []);
  const endSchema = useMemo<Validator[]>(() => [validDate()], []);
  const amountSchema = useMemo<Validator[]>(() => [positiveAmount()], []);

  const validation = useMemo(
    () =>
      validateForm(
        { category, periodStart, periodEnd, allocated },
        {
          category: categorySchema,
          periodStart: startSchema,
          periodEnd: endSchema,
          allocated: amountSchema,
        }
      ),
    [category, periodStart, periodEnd, allocated, categorySchema, startSchema, endSchema, amountSchema]
  );

  const rangeError = useMemo(
    () => rangeValid(periodStart, periodEnd),
    [periodStart, periodEnd]
  );

  const today = useMemo(() => {
    // Only needed to set the start-date min when backdating is blocked.
    try {
      return new Date().toISOString().slice(0, 10);
    } catch {
      return undefined;
    }
  }, []);

  /** True when the start date is before local "today" and backdating is off. */
  const pastBlocked = useMemo(
    () =>
      backdatedEnabled
        ? false
        : isPastPeriod(
            periodStart,
            LOCALE,
            TIME_ZONE
          ),
    [periodStart, backdatedEnabled]
  );

  const hasValidStart = periodStart !== "" && rangeError === null;

  const canSubmit =
    formReady &&
    loadState === "ready" &&
    canWrite &&
    validation.length === 0 &&
    rangeError === null &&
    !pastBlocked;

  const bothDatesValid =
    formatDate(periodStart, LOCALE) !== "" &&
    formatDate(periodEnd, LOCALE) !== "" &&
    rangeError === null;
  const previewText =
    bothDatesValid
      ? `Covers ${formatDate(periodStart, LOCALE)} – ${formatDate(periodEnd, LOCALE)}`
      : "";

  const handlePeriodTypeChange = (e: ChangeEvent<HTMLSelectElement>): void => {
    const next = e.target.value as BudgetPeriodType;
    setPeriodType(next);
    // The period type defines the window — overwrite any manual dates with the
    // new default and clear server errors on both date fields.
    const win = defaultWindow(next);
    setPeriodStart(win.start);
    setPeriodEnd(win.end);
    setServerErrors((prev) => {
      const nextErr = { ...prev };
      delete nextErr.periodStart;
      delete nextErr.periodEnd;
      return nextErr;
    });
  };

  const handleStartChange = (e: ChangeEvent<HTMLInputElement>): void => {
    setPeriodStart(e.target.value);
    setServerErrors((prev) => {
      if (!prev.periodStart) return prev;
      const next = { ...prev };
      delete next.periodStart;
      return next;
    });
  };

  const handleEndChange = (e: ChangeEvent<HTMLInputElement>): void => {
    setPeriodEnd(e.target.value);
    setServerErrors((prev) => {
      if (!prev.periodEnd) return prev;
      const next = { ...prev };
      delete next.periodEnd;
      return next;
    });
  };

  const handleCategoryChange = (e: ChangeEvent<HTMLSelectElement>): void => {
    setCategory(e.target.value);
    setServerErrors((prev) => {
      if (!prev.category) return prev;
      const next = { ...prev };
      delete next.category;
      return next;
    });
  };

  /** 2-dp money guard: digits + one decimal separator only, max 2 decimals. */
  const handleAllocatedChange = (e: ChangeEvent<HTMLInputElement>): void => {
    const raw = e.target.value;
    const cleaned = raw.replace(/[^\d.]/g, "").replace(/(\..*)\./g, "$1");
    setAllocated(cleaned);
    setServerErrors((prev) => {
      if (!prev.allocated) return prev;
      const next = { ...prev };
      delete next.allocated;
      return next;
    });
  };

  /** Optimistic-lock read-only refetch. Drops local edits, reseeds from the
   * latest server state — never triggers a mutation (no silent overwrite). */
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
      if (validation.some((fe) => fe.field === "category")) {
        categoryRef.current?.focus();
      } else {
        amountRef.current?.focus();
      }
      return;
    }

    if (isEdit && current) {
      const expected = current.version ?? version;
      if (typeof expected !== "number" || !Number.isInteger(expected) || expected < 0) {
        setNotice({
          kind: "generic",
          text: "The budget version is invalid. Reload and try again.",
        });
        return;
      }
    }

    const money: Money = { amount: allocated.trim(), currency };
    const dto = {
      categoryId: category,
      periodType,
      periodStart,
      periodEnd,
      allocated: money,
    };

    setSubmitting(true);
    const request =
      isEdit && current
        ? updateBudget(current.id, dto, current.version ?? version ?? 0)
        : createBudget(dto);

    request
      .then((saved) => {
        // Refresh the client projection so the list/card reflects the new/edit
        // immediately. Non-authoritative display mirror; server remains truth.
        if (isEdit) {
          useBudgetStore.getState().invalidateCache(saved.id);
        } else {
          useBudgetStore.getState().setAllocations([saved]);
        }
        const prefix = isEdit ? "Budget updated" : "Budget saved";
        setNotice({ kind: "success", text: prefix });
        onSaved?.(saved);
      })
      .catch((err) => {
        // Map API errors to hard-coded localized copy; never render raw server text.
        if (err && err.name === "ValidationError") {
          const mapped: Record<string, string> = {};
          for (const fe of err.fieldErrors ?? []) {
            mapped[fe.field ?? "allocated"] = "Please correct this value and try again.";
          }
          setServerErrors(mapped);
          if (mapped.allocated) amountRef.current?.focus();
          return;
        }
        if (err && err.name === "ConflictError") {
          setNotice({ kind: "conflict", text: "This budget was modified by another user." });
          return;
        }
        if (err && err.name === "ForbiddenError") {
          setNotice({ kind: "denied", text: "You do not have permission to save budgets." });
          return;
        }
        if (err && err.name === "NotFoundError") {
          setNotice({ kind: "notfound", text: "This budget no longer exists. Reload to see the latest list." });
          return;
        }
        const ref = err && err.correlationId ? ` Reference: ${err.correlationId}` : "";
        setNotice({ kind: "generic", text: `Saving failed. Please try again.${ref}` });
      })
      .finally(() => setSubmitting(false));
  };

  // Denied: view-only role renders no edit affordances (least privilege). The
  // authoritative gate stays server-side; this only hides the form.
  if (!canWrite) {
    return (
      <main className="bfp-page">
        <h1 className="bfp-h1">{isEdit ? "Edit Budget" : "New Budget"}</h1>
        <div className="bfp-notice bfp-notice--error" role="alert" aria-atomic="true">
          <p>You do not have permission to save budgets.</p>
          <div className="bfp-actions">
            <Button variant="ghost" onClick={onCancel}>
              Back to budgets
            </Button>
          </div>
        </div>
      </main>
    );
  }

  // Loading (edit only) — never flash empty inputs before data arrives.
  if (isEdit && (loadState === "loading" || !formReady)) {
    return (
      <main className="bfp-page" aria-busy="true">
        <h1 className="bfp-h1">Edit Budget</h1>
        <p className="bfp-caption">Loading this budget…</p>
      </main>
    );
  }

  // Load failure (edit only) — never render form fields until data arrives, so
  // the user never edits phantom data and then hits 409/404 on save.
  if (isEdit && loadState === "error") {
    return (
      <main className="bfp-page">
        <h1 className="bfp-h1">Edit Budget</h1>
        <div className="bfp-notice bfp-notice--error" role="alert" aria-atomic="true">
          <p>{loadError ?? "Couldn't load this budget."}</p>
          <div className="bfp-actions">
            <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
              Retry
            </Button>
            <Button variant="ghost" onClick={onCancel}>
              Back to budgets
            </Button>
          </div>
        </div>
      </main>
    );
  }

  // Edit mode record is absent from the fetched list (e.g. deleted / wrong id).
  if (isEdit && loadState === "ready" && !formReady && !current) {
    return (
      <main className="bfp-page">
        <h1 className="bfp-h1">Edit Budget</h1>
        <div className="bfp-notice bfp-notice--error" role="alert" aria-atomic="true">
          <p>This budget could not be found. It may have been deleted.</p>
          <div className="bfp-actions">
            <Button variant="ghost" onClick={onCancel}>
              Back to budgets
            </Button>
          </div>
        </div>
      </main>
    );
  }

  return (
    <main className="bfp-page">
      <h1 className="bfp-h1">{isEdit ? "Edit Budget" : "New Budget"}</h1>
      <p className="bfp-caption">
        {isEdit && current
          ? `Editing budget for ${current.periodType.toLowerCase()} period`
          : "Set a spending limit per category for a period."}
      </p>

      {notice && notice.kind === "conflict" && (
        <div
          className="bfp-notice bfp-notice--error"
          role="alert"
          aria-atomic="true"
        >
          <p>
            <span aria-hidden="true">⚠ </span>
            {notice.text} <strong>Refresh</strong> to load the latest version.
          </p>
          <div className="bfp-actions">
            <Button
              variant="secondary"
              loading={loadState === "loading"}
              onClick={refreshLatest}
            >
              {loadState === "loading" ? "Reloading…" : "Reload"}
            </Button>
          </div>
        </div>
      )}

      {notice &&
        notice.kind !== "conflict" &&
        (notice.kind === "generic" ||
        notice.kind === "denied" ||
        notice.kind === "notfound" ? (
          <div className="bfp-notice bfp-notice--error" role="alert" aria-atomic="true">
            <p>{notice.text}</p>
            <div className="bfp-actions">
              <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
                Retry
              </Button>
            </div>
          </div>
        ) : (
          <div
            className="bfp-notice bfp-notice--success"
            role="status"
            aria-live="polite"
            aria-atomic="true"
          >
            <p>{notice.text}</p>
          </div>
        ))}

      <form className="bfp-card" onSubmit={handleSubmit} noValidate>
        <div className="bfp-fields">
          <div className="bfp-category">
            <Field
              id="category"
              label="Category"
              required
              validate={categorySchema}
              error={serverErrors.category ?? null}
              hint={
                categories.length === 0
                  ? "No categories available yet. Create a category first."
                  : "The budget applies to this category."
              }
            >
              <select
                ref={categoryRef}
                id="category"
                value={category}
                onChange={handleCategoryChange}
                disabled={loadState === "loading" && !isEdit}
              >
                <option value="">Select a category</option>
                {categories.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </select>
            </Field>
          </div>

          <fieldset className="bfp-period">
            <legend className="bfp-legend">Budget period</legend>

            <div className="bfp-period-type">
              <Field
                id="periodType"
                label="Period"
                required
                hint="Changing the period sets the date window."
              >
                <select
                  id="periodType"
                  value={periodType}
                  onChange={handlePeriodTypeChange}
                >
                  {PERIOD_TYPE_OPTIONS.map((opt) => (
                    <option key={opt.value} value={opt.value}>
                      {opt.label}
                    </option>
                  ))}
                </select>
              </Field>
            </div>

            <div className="bfp-date-grid">
              <div className="bfp-start">
                <Field
                  id="periodStart"
                  label="Start date"
                  required
                  validate={startSchema}
                  error={
                    pastBlocked
                      ? "Budgets for past periods require backdated analysis"
                      : serverErrors.periodStart ?? null
                  }
                >
                  <input
                    id="periodStart"
                    type="date"
                    value={periodStart}
                    onChange={handleStartChange}
                    min={backdatedEnabled ? undefined : today}
                  />
                </Field>
                {pastBlocked && (
                  <p className="bfp-past-hint">
                    Your organization restricts past-period budgets. Contact an
                    administrator to enable backdating.
                  </p>
                )}
              </div>

              <div className="bfp-end">
                <Field
                  id="periodEnd"
                  label="End date"
                  required
                  validate={endSchema}
                  error={serverErrors.periodEnd ?? rangeError ?? null}
                >
                  <input
                    id="periodEnd"
                    type="date"
                    value={periodEnd}
                    onChange={handleEndChange}
                    min={hasValidStart ? periodStart : undefined}
                  />
                </Field>
              </div>
            </div>

            {/* Window preview reserves its height even when empty to avoid CLS. */}
            <div
              className={
                bothDatesValid
                  ? "bfp-preview bfp-preview--visible"
                  : "bfp-preview"
              }
              aria-live="polite"
              role="status"
            >
              {previewText}
            </div>
          </fieldset>

          <div className="bfp-amount">
            <div className="bfp-balance">
              <Field
                id="allocated"
                label="Allocated amount"
                required
                validate={amountSchema}
                error={serverErrors.allocated ?? null}
                hint="Positive amount, at most 2 decimal places."
              >
                <input
                  ref={amountRef}
                  className="bfp-money-input"
                  id="allocated"
                  type="text"
                  inputMode="decimal"
                  value={allocated}
                  onChange={handleAllocatedChange}
                  autoComplete="off"
                  placeholder="0.00"
                />
              </Field>
              <span className="bfp-money-prefix" aria-hidden="true">
                {currency}
              </span>
            </div>
          </div>
        </div>

        {!canSubmit && (
          <p className="bfp-helper--submit">Complete required fields to save.</p>
        )}

        <div className="bfp-actions">
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
            {isEdit ? "Save changes" : "Save Budget"}
          </Button>
        </div>
      </form>
    </main>
  );
}

const FMS_BUDGET_FORM_CSS = `
.bfp-page {
  max-width: 640px;
  margin-inline: auto;
  padding: 24px 16px;
}
.bfp-h1 {
  margin: 0 0 8px;
  font-size: 24px;
  font-weight: 600;
  line-height: 1.25;
  color: var(--color-text-primary, #0f172a);
}
.bfp-caption {
  margin: 0 0 24px;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.bfp-card {
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1), 0 2px 4px -1px rgba(0,0,0,0.06);
  padding: 24px;
}
.bfp-fields {
  display: flex;
  flex-direction: column;
  gap: 24px;
}
.bfp-period {
  border: none;
  margin: 0;
  padding: 16px;
  background: var(--color-bg-subtle, #f8fafc);
  border-radius: 8px;
}
.bfp-legend {
  padding: 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-secondary, #475569);
  margin-bottom: 12px;
}
.bfp-date-grid {
  display: grid;
  grid-template-columns: 1fr;
  gap: 16px;
}
@media (min-width: 768px) {
  .bfp-date-grid {
    grid-template-columns: 1fr 1fr;
  }
}
.bfp-preview {
  min-height: 40px;
  margin-top: 16px;
  padding: 8px 12px;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
  background: var(--color-surface, #ffffff);
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 4px;
  display: flex;
  align-items: center;
  opacity: 0;
  transform: translateY(4px);
  transition: opacity 150ms ease-out, transform 150ms ease-out;
}
.bfp-preview--visible {
  opacity: 1;
  transform: translateY(0);
}
.bfp-past-hint {
  margin: -4px 0 8px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.bfp-balance {
  position: relative;
}
.bfp-money-prefix {
  position: absolute;
  left: 12px;
  top: 51px;
  transform: translateY(-50%);
  color: var(--color-text-muted, #64748b);
  pointer-events: none;
}
.bfp-balance .bfp-money-input {
  padding-left: 52px; /* room for the currency code prefix */
}
.bfp-helper--submit {
  margin: 16px 0 0;
  font-size: 14px;
  line-height: 1.5;
  color: var(--color-text-muted, #64748b);
}
.bfp-actions {
  display: flex;
  gap: 12px;
  margin-top: 32px;
  flex-wrap: wrap;
}
@media (max-width: 767px) {
  .bfp-actions {
    flex-direction: column;
  }
  .bfp-actions > * {
    flex: 1 1 100%;
    justify-content: center;
  }
}
.bfp-notice {
  padding: 16px;
  border: 1px solid var(--color-border, #cbd5e1);
  border-radius: 8px;
  margin-bottom: 16px;
}
.bfp-notice p {
  margin: 0 0 12px;
  font-size: 14px;
  line-height: 1.5;
}
.bfp-notice p:only-child {
  margin: 0;
}
.bfp-notice--error {
  background: var(--color-danger-bg, #fef2f2);
  border-color: var(--color-danger, #dc2626);
  color: var(--color-danger, #dc2626);
}
.bfp-notice--success {
  background: #f0fdf4;
  border-color: #16a34a;
  color: #166534;
}
@media (prefers-reduced-motion: reduce) {
  .bfp-preview {
    transition: none;
    transform: none;
  }
}
`;

if (
  typeof document !== "undefined" &&
  !document.getElementById("fms-budget-form-styles")
) {
  const style = document.createElement("style");
  style.id = "fms-budget-form-styles";
  style.textContent = FMS_BUDGET_FORM_CSS;
  document.head.appendChild(style);
}