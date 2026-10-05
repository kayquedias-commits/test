import { useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, FormEvent, ReactElement } from "react";

import { Button } from "../../components/ui/Button";
import { Field } from "../../components/ui/Field";
import {
  createCategory,
  listCategories,
  updateCategory,
} from "../../services/api/endpoints";
import type { CategoryDTO, CategoryType } from "../../services/api/types";
import { maxLength, required, validateForm } from "../../utils/validation";

const PAGE_SIZE = 100;

export interface CategoryFormPageProps {
  /** Present when editing (route /categories/:id/edit). */
  categoryId?: string;
  /** Preloaded entity for edit mode; when absent the entity is located from the list fetch. */
  initialCategory?: CategoryDTO;
  /** Called with the saved entity so the caller can refresh and navigate to the list. */
  onSaved?: (category: CategoryDTO) => void;
  /** Escape hatch that navigates away without saving. */
  onCancel?: () => void;
}

/**
 * Create/edit form for a category or one-level-deep subcategory. Enforces the
 * one-level parent rule (only top-level categories may be parents) and blocks
 * type flips for categories already used by transactions. Server remains the
 * authority for both rules; these constraints only reduce avoidable 409s.
 */
export default function CategoryFormPage(
  props: CategoryFormPageProps
): ReactElement {
  const { categoryId, initialCategory, onSaved, onCancel } = props;
  const isEdit = Boolean(categoryId);

  const nameRef = useRef<HTMLInputElement>(null);

  const [categories, setCategories] = useState<CategoryDTO[]>([]);
  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">(
    "loading"
  );
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);

  const [name, setName] = useState("");
  const [type, setType] = useState<CategoryType>("INCOME");
  const [parentId, setParentId] = useState("");
  const [version, setVersion] = useState<number | null>(null);

  const [submitting, setSubmitting] = useState(false);
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<{
    kind: "conflict" | "denied" | "notfound" | "generic";
    text: string;
  } | null>(null);

  useEffect(() => {
    setLoadState("loading");
    setLoadError(null);
    const controller = new AbortController();
    listCategories({ pageSize: PAGE_SIZE }, controller.signal)
      .then((res) => {
        setCategories(res.items ?? []);
        setLoadState("ready");
      })
      .catch((err) => {
        if (err.name === "AbortError") {
          return;
        }
        setLoadError(err.message ?? "Failed to load categories.");
        setLoadState("error");
      });
    return () => controller.abort();
  }, [retryNonce]);

  const current = useMemo<CategoryDTO | undefined>(
    () =>
      initialCategory ??
      (isEdit ? categories.find((c) => c.id === categoryId) : undefined),
    [initialCategory, categories, categoryId, isEdit]
  );

  // Seed the form from the located entity once the list is available.
  useEffect(() => {
    if (!isEdit || loadState !== "ready" || !current) {
      return;
    }
    setName(current.name);
    setType(current.type);
    setVersion(current.version ?? null);
    setParentId(current.parentId ?? "");
  }, [isEdit, loadState, current]);

  const inUseLocked = useMemo(
    () => isEdit && current?.inUse === true,
    [isEdit, current]
  );
  const hasSubcategories = useMemo(
    () =>
      isEdit && Boolean(current) && categories.some((c) => c.parentId === current?.id),
    [isEdit, current, categories]
  );
  const parentLocked = hasSubcategories;

  const topLevelParents = useMemo(
    () => categories.filter((c) => c.parentId === null && c.id !== current?.id),
    [categories, current]
  );

  const nameSchema = useMemo(
    () => [required("Name"), maxLength(255, "Name")],
    []
  );
  const validation = useMemo(
    () => validateForm({ name }, { name: nameSchema }),
    [name, nameSchema]
  );
  const canSubmit = validation.name.length === 0;

  // Parent is already accounted for once the form is seeded; keep results local.
  const handleNameChange = (e: ChangeEvent<HTMLInputElement>): void => {
    setName(e.target.value);
    if (serverErrors.name) {
      setServerErrors((prev) => ({ ...prev, name: "" }));
    }
  };

  const handleParentChange = (e: ChangeEvent<HTMLSelectElement>): void => {
    setParentId(e.target.value);
  };

  const handleSubmit = (e: FormEvent<HTMLFormElement>): void => {
    e.preventDefault();
    setNotice(null);
    if (!canSubmit) {
      nameRef.current?.focus();
      return;
    }
    if (submitting) {
      return;
    }

    const trimmedName = name.trim();
    const parentValue = parentId === "" ? null : parentId;
    if (parentValue !== null && !topLevelParents.some((c) => c.id === parentValue)) {
      setNotice({ kind: "generic", text: "The selected parent is not a top-level category." });
      return;
    }
    if (isEdit && current) {
      const v = current.version ?? version;
      if (v === null || !Number.isInteger(v) || v < 0) {
        setNotice({ kind: "generic", text: "The category version is invalid. Reload and try again." });
        return;
      }
    }

    const dto = { name: trimmedName, type, parentId: parentValue };
    setSubmitting(true);

    const request = isEdit && current
      ? updateCategory(current.id, dto, current.version ?? version ?? 0)
      : createCategory(dto);

    request
      .then((saved) => {
        setNotice({ kind: "generic", text: "Category saved." });
        onSaved?.(saved);
      })
      .catch((err) => {
        if (err.name === "ValidationError" && err.fieldErrors) {
          const mapped: Record<string, string> = {};
          for (const fe of err.fieldErrors) {
            mapped[fe.field] = fe.message;
            if (fe.field === "name") {
              nameRef.current?.focus();
            }
          }
          setServerErrors(mapped);
          return;
        }
        if (err.name === "ConflictError") {
          setNotice({ kind: "conflict", text: "This category was modified by another user." });
          return;
        }
        if (err.name === "ForbiddenError") {
          setNotice({ kind: "denied", text: "You do not have permission to save categories." });
          return;
        }
        if (err.name === "NotFoundError") {
          setNotice({ kind: "notfound", text: "This category no longer exists. Reload to see the latest list." });
          return;
        }
        const correlationId = err.correlationId;
        setNotice({
          kind: "generic",
          text: correlationId
            ? `Saving failed. Reference: ${correlationId}`
            : "Saving failed. Please try again.",
        });
      })
      .finally(() => setSubmitting(false));
  };

  if (loadState === "loading") {
    return (
      <section aria-busy="true" className="cat-page">
        <h1 className="cat-title">{isEdit ? "Edit Category" : "New Category"}</h1>
        <p className="cat-caption">Loading…</p>
      </section>
    );
  }

  if (loadState === "error") {
    return (
      <section className="cat-page">
        <h1 className="cat-title">{isEdit ? "Edit Category" : "New Category"}</h1>
        <p className="cat-caption">{loadError ?? "Category not found or unavailable."}</p>
        <div className="cat-actions cat-actions--footer">
          <Button
            variant="secondary"
            onClick={() => setRetryNonce((n) => n + 1)}
          >
            Retry
          </Button>
          <Button variant="ghost" onClick={onCancel}>
            Back to categories
          </Button>
        </div>
      </section>
    );
  }

  const parentHelper = parentLocked
    ? "Parent is locked — this category has subcategories."
    : topLevelParents.length === 0
    ? "No parent categories available yet. Create a top-level category first."
    : "Subcategories are one level deep — only top-level categories appear. Selecting none creates a top-level category.";

  return (
    <section className="cat-page">
      <h1 className="cat-title">{isEdit ? "Edit Category" : "New Category"}</h1>
      <p className="cat-caption">
        {isEdit && current?.parentId
          ? "Editing a subcategory"
          : "Define an income or expense category"}
      </p>

      {notice && (
        <div
          role="alert"
          aria-atomic="true"
          className={`cat-notice cat-notice--${notice.kind}`}
        >
          <span className="cat-notice__text">{notice.text}</span>
          {notice.kind === "conflict" && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setRetryNonce((n) => n + 1)}
            >
              Reload
            </Button>
          )}
        </div>
      )}

      <form className="cat-card" onSubmit={handleSubmit} noValidate>
        <Field
          id="name"
          label="Name"
          required
          validate={nameSchema}
          error={serverErrors.name ?? null}
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

        <fieldset className="cat-fieldset" disabled={inUseLocked}>
          <legend className="cat-legend">
            Type
            {inUseLocked && (
              <span className="cat-badge" aria-hidden="true">
                {type === "INCOME" ? "Income" : "Expense"}
              </span>
            )}
          </legend>
          {inUseLocked ? (
            <p className="cat-fieldset__value">
              {type === "INCOME" ? "Income" : "Expense"}
            </p>
          ) : (
            <div className="cat-radio-row">
              <label className="cat-radio">
                <input
                  type="radio"
                  name="type"
                  value="INCOME"
                  checked={type === "INCOME"}
                  onChange={() => setType("INCOME")}
                />
                <span>Income</span>
              </label>
              <label className="cat-radio">
                <input
                  type="radio"
                  name="type"
                  value="EXPENSE"
                  checked={type === "EXPENSE"}
                  onChange={() => setType("EXPENSE")}
                />
                <span>Expense</span>
              </label>
            </div>
          )}
          <p
            className="cat-fieldset__hint"
            {...(inUseLocked ? { "aria-describedby": "type-lock-hint" } : {})}
          >
            {inUseLocked
              ? "Type is locked — this category is used by existing transactions. Changing it would corrupt historical reporting."
              : "Choose the direction of cash flow for this category."}
          </p>
          {inUseLocked && (
            <span id="type-lock-hint" className="cat-visually-hidden" aria-hidden="true">
              Type is locked for categories in use
            </span>
          )}
        </fieldset>

        {parentLocked ? (
          <div className="cat-field">
            <span className="cat-label">Parent</span>
            <p className="cat-fieldset__value">
              {current?.parentId
                ? categories.find((c) => c.id === current?.parentId)?.name ?? "Top-level"
                : "Top-level"}
            </p>
            <p className="cat-fieldset__hint">{parentHelper}</p>
          </div>
        ) : (
          <Field
            id="parent"
            label="Parent"
            hint={parentHelper}
          >
            <select
              id="parent"
              value={parentId}
              onChange={handleParentChange}
              disabled={loadState !== "ready" || topLevelParents.length === 0}
            >
              <option value="">No parent (top-level category)</option>
              {topLevelParents.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </Field>
        )}

        {!canSubmit && (
          <p className="cat-helper cat-helper--submit">
            Fill required fields to save.
          </p>
        )}

        <div className="cat-actions cat-actions--footer">
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
            Save
          </Button>
        </div>
      </form>
    </section>
  );
}