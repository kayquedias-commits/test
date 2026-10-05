import { useEffect, useRef, useState } from "react";
import type { ChangeEvent, FormEvent } from "react";
import { Button } from "../../components/ui/Button";
import { Field } from "../../components/ui/Field";
import { ExportStatusPanel } from "./ExportStatusPanel";
import { usePermission } from "../../hooks/usePermission";
import { createExport } from "../../services/api/endpoints";
import type { ExportFormat } from "../../services/api/types";
import {
  addMonths,
  endOfMonth,
  isRangeValid,
  toIsoDate,
} from "../../utils/date";

/* ------------------------------------------------------------------------- *
 * Colocated styles, injected once (synthetic <style>). No inline styles and
 * no styles/sources derived from user data (CSP-clean). Token names use the
 * --fms-exp-* convention with safe fallbacks when a global token is absent.
 * ------------------------------------------------------------------------- */
const FMS_EXPORT_PAGE_CSS = `
:root {
  --fms-exp-primary: #2563eb;
  --fms-exp-primary-hover: #1d4ed8;
  --fms-exp-primary-active: #1e40af;
  --fms-exp-success: #16a34a;
  --fms-exp-error: #dc2626;
  --fms-exp-surface: #ffffff;
  --fms-exp-text: #111827;
  --fms-exp-text-secondary: #4b5563;
  --fms-exp-text-muted: #6b7280;
  --fms-exp-border: #e5e7eb;
  --fms-exp-border-strong: #d1d5db;
  --fms-exp-space-2: 8px;
  --fms-exp-space-3: 12px;
  --fms-exp-space-4: 16px;
  --fms-exp-space-6: 24px;
  --fms-exp-space-8: 32px;
  --fms-exp-text-sm: 14px;
  --fms-exp-text-base: 16px;
  --fms-exp-text-lg: 18px;
  --fms-exp-font-normal: 400;
  --fms-exp-font-medium: 500;
  --fms-exp-font-semibold: 600;
  --fms-exp-leading-tight: 1.25;
  --fms-exp-leading-normal: 1.5;
  --fms-exp-radius-sm: 4px;
  --fms-exp-radius-md: 8px;
  --fms-exp-radius-lg: 12px;
  --fms-exp-transition-fast: 150ms ease-out;
  --fms-exp-transition-base: 200ms ease-out;
}

.fms-exp-page {
  max-width: 1280px;
  margin: 0 auto;
  padding: var(--fms-exp-space-4) var(--fms-exp-space-4) var(--fms-exp-space-8);
  background: #f9fafb;
  min-height: 100%;
}
.fms-exp-title {
  margin: 0 0 var(--fms-exp-space-6);
  font-size: 24px;
  font-weight: var(--fms-exp-font-semibold);
  line-height: var(--fms-exp-leading-tight);
  color: var(--fms-exp-text);
}

/* Form card — Common Region for the whole request task. */
.fms-exp-card {
  background: var(--fms-exp-surface);
  border: 1px solid var(--fms-exp-border);
  border-radius: var(--fms-exp-radius-lg);
  box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1), 0 2px 4px -1px rgba(0,0,0,0.06);
  padding: var(--fms-exp-space-4);
  display: grid;
  grid-template-columns: 1fr;
  gap: var(--fms-exp-space-6);
  max-width: 640px;
}
@media (min-width: 768px) {
  .fms-exp-card { padding: var(--fms-exp-space-6); margin: 0 auto; }
}

.fms-exp-fieldset {
  border: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: var(--fms-exp-space-3);
}
.fms-exp-legend {
  padding: 0;
  font-size: var(--fms-exp-text-sm);
  font-weight: var(--fms-exp-font-medium);
  color: var(--fms-exp-text-secondary);
}

/* Format + scope radio tiles (identical chrome, equal affordance). */
.fms-exp-radio-group {
  display: flex;
  flex-direction: column;
  gap: var(--fms-exp-space-3);
}
@media (min-width: 768px) {
  .fms-exp-radio-group { flex-direction: row; }
}
.fms-exp-radio {
  position: relative;
  flex: 1 1 auto;
  min-height: 48px;
  display: flex;
  align-items: center;
  gap: var(--fms-exp-space-3);
  padding: var(--fms-exp-space-3);
  border: 1px solid var(--fms-exp-border-strong);
  border-radius: var(--fms-exp-radius-md);
  background: var(--fms-exp-surface);
  cursor: pointer;
  transition: border-color var(--fms-exp-transition-base), background-color var(--fms-exp-transition-base);
}
.fms-exp-radio:hover { background: #f9fafb; }
.fms-exp-radio:has(input:checked) {
  border-color: var(--fms-exp-primary);
  background: #f5f7ff;
}
.fms-exp-radio input {
  position: absolute;
  opacity: 0;
  width: 1px;
  height: 1px;
}
.fms-exp-radio__indicator {
  flex: none;
  width: 18px;
  height: 18px;
  border: 2px solid var(--fms-exp-border-strong);
  border-radius: 9999px;
  background: var(--fms-exp-surface);
  transition: border-color var(--fms-exp-transition-base), background-color var(--fms-exp-transition-base);
}
.fms-exp-radio:has(input:checked) .fms-exp-radio__indicator {
  border-color: var(--fms-exp-primary);
  background: var(--fms-exp-primary);
  box-shadow: inset 0 0 0 4px var(--fms-exp-surface);
}
.fms-exp-radio input:focus-visible + .fms-exp-radio__indicator {
  outline: 2px solid var(--fms-exp-primary);
  outline-offset: 2px;
}
.fms-exp-radio__label {
  font-size: var(--fms-exp-text-base);
  font-weight: var(--fms-exp-font-medium);
  color: var(--fms-exp-text);
}

/* Range preset chips. Active state indicated by border + text (never color
   alone). */
.fms-exp-chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--fms-exp-space-2);
}
.fms-exp-chip {
  min-height: 44px;
  padding: var(--fms-exp-space-2) var(--fms-exp-space-3);
  border: 1px solid var(--fms-exp-border-strong);
  border-radius: var(--fms-exp-radius-sm);
  background: var(--fms-exp-surface);
  font-size: var(--fms-exp-text-sm);
  font-weight: var(--fms-exp-font-medium);
  color: var(--fms-exp-text);
  cursor: pointer;
  transition: border-color var(--fms-exp-transition-fast), color var(--fms-exp-transition-fast),
    background-color var(--fms-exp-transition-fast);
}
.fms-exp-chip:hover { background: #f9fafb; }
.fms-exp-chip--active {
  border-color: var(--fms-exp-primary);
  color: var(--fms-exp-primary);
  background: #f5f7ff;
}
.fms-exp-chip:focus-visible {
  outline: 2px solid var(--fms-exp-primary);
  outline-offset: 2px;
}

/* Date range: two stacked on mobile, side-by-side from tablet up. */
.fms-exp-range {
  display: grid;
  grid-template-columns: 1fr;
  gap: var(--fms-exp-space-3);
}
@media (min-width: 768px) {
  .fms-exp-range { grid-template-columns: 1fr 1fr; gap: var(--fms-exp-space-4); }
}
.fms-exp-range .fms-field { margin-bottom: 0; }

/* Submit region. */
.fms-exp-submit {
  display: grid;
  gap: var(--fms-exp-space-2);
}
.fms-exp-submit__hint {
  margin: 0;
  font-size: var(--fms-exp-text-sm);
  color: var(--fms-exp-text-muted);
}
.fms-exp-submit__error {
  margin: 0;
  display: flex;
  align-items: center;
  gap: var(--fms-exp-space-2);
  font-size: var(--fms-exp-text-sm);
  font-weight: var(--fms-exp-font-medium);
  color: var(--fms-exp-error);
}
.fms-exp-submit .fms-btn { width: 100%; min-height: 48px; }
@media (min-width: 768px) {
  .fms-exp-submit { justify-items: end; }
  .fms-exp-submit .fms-btn { width: auto; min-width: 160px; }
}

/* Status region — separate Common Region, 32px below the form. */
.fms-exp-status-region {
  margin-top: var(--fms-exp-space-8);
  max-width: 640px;
  scroll-margin-top: var(--fms-exp-space-4);
}
@media (min-width: 768px) {
  .fms-exp-status-region { margin-left: auto; margin-right: auto; }
}

/* Permission-denied guidance (static, no affordance). */
.fms-exp-403 {
  max-width: 480px;
  margin: var(--fms-exp-space-8) auto;
  padding: var(--fms-exp-space-6);
  background: var(--fms-exp-surface);
  border: 1px solid var(--fms-exp-border);
  border-radius: var(--fms-exp-radius-lg);
  text-align: center;
  box-shadow: 0 1px 2px rgba(0,0,0,0.05);
  animation: fms-exp-403-in var(--fms-exp-transition-base);
}
@keyframes fms-exp-403-in {
  from { opacity: 0; transform: translateY(8px); }
  to { opacity: 1; transform: translateY(0); }
}
.fms-exp-403__icon {
  color: var(--fms-exp-text-muted);
  display: inline-flex;
  margin-bottom: var(--fms-exp-space-3);
}
.fms-exp-403__title {
  margin: 0 0 var(--fms-exp-space-2);
  font-size: var(--fms-exp-text-lg);
  font-weight: var(--fms-exp-font-semibold);
  color: var(--fms-exp-text);
}
.fms-exp-403__body {
  margin: 0;
  font-size: var(--fms-exp-text-base);
  line-height: var(--fms-exp-leading-normal);
  color: var(--fms-exp-text-secondary);
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-export-page-styles")) {
  const style = document.createElement("style");
  style.id = "fms-export-page-styles";
  style.textContent = FMS_EXPORT_PAGE_CSS;
  document.head.appendChild(style);
}

type ExportScope = "transactions" | "reports";

type RangePreset = { id: string; label: string };

const RANGE_PRESETS: RangePreset[] = [
  { id: "last30", label: "Last 30 days" },
  { id: "thisQuarter", label: "This quarter" },
  { id: "thisYear", label: "This year" },
  { id: "custom", label: "Custom" },
];

const SCOPE_OPTIONS: { value: ExportScope; label: string }[] = [
  { value: "transactions", label: "Transactions" },
  { value: "reports", label: "Reports summary" },
];

/** Resolve a named preset into an inclusive ISO date range, or null for a
 * no-op ("custom"). All arithmetic is UTC-normalized via the date utils. */
function computePresetRange(preset: string): { start: string; end: string } | null {
  const today = new Date();
  switch (preset) {
    case "last30":
      return { start: toIsoDate(addMonths(today, -1)), end: toIsoDate(today) };
    case "thisQuarter": {
      const qStartMonth = Math.floor(today.getUTCMonth() / 3) * 3;
      const qStart = new Date(Date.UTC(today.getUTCFullYear(), qStartMonth, 1));
      return { start: toIsoDate(qStart), end: toIsoDate(endOfMonth(addMonths(qStart, 2))) };
    }
    case "thisYear": {
      const year = today.getUTCFullYear();
      return {
        start: toIsoDate(new Date(Date.UTC(year, 0, 1))),
        end: toIsoDate(new Date(Date.UTC(year, 11, 31))),
      };
    }
    default:
      return null;
  }
}

const LOCK_ICON = (
  <svg viewBox="0 0 16 16" width="24" height="24" aria-hidden="true">
    <path
      fill="currentColor"
      d="M8 1.5a3.5 3.5 0 0 0-3.5 3.5v2H3a1.5 1.5 0 0 0-1.5 1.5v5A1.5 1.5 0 0 0 3 15h10a1.5 1.5 0 0 0 1.5-1.5v-5A1.5 1.5 0 0 0 13 7h-1.5v-2A3.5 3.5 0 0 0 8 1.5zM6 5a2 2 0 1 1 4 0v2H6V5z"
    />
  </svg>
);

/**
 * Export request UI (Module H). Permission-gated on `export:create`
 * (least-privilege §9.7): the affordance is hidden from navigation elsewhere;
 * if a user reaches this route without the permission, a static 403-style
 * guidance state renders instead of a blank screen. Submitting a valid range
 * creates an async job and hands it to ExportStatusPanel, which owns polling
 * and the READY (download with pre-signed URL) / FAILED (retry) states.
 */
export default function ExportPage() {
  const { can } = usePermission();

  const [format, setFormat] = useState<ExportFormat>("CSV");
  const [scope, setScope] = useState<ExportScope>("transactions");
  const initialRange = computePresetRange("last30");
  const [range, setRange] = useState<{ start: string; end: string }>(
    initialRange ?? { start: "", end: "" },
  );
  const [rangePreset, setRangePreset] = useState("last30");
  // Bumped only on preset clicks so date Fields remount with the resolved
  // range; manual edits never change the key, so typing never drops focus.
  const [presetNonce, setPresetNonce] = useState(0);
  const [endError, setEndError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);

  const statusRef = useRef<HTMLDivElement | null>(null);
  const prevJobRef = useRef<string | null>(null);

  const rangeValid = isRangeValid(range.start, range.end);
  const canSubmit = rangeValid && !submitting;

  // On a new job, move focus into the status region (form -> status order).
  useEffect(() => {
    if (jobId && prevJobRef.current !== jobId) {
      statusRef.current?.focus();
    }
    prevJobRef.current = jobId;
  }, [jobId]);

  if (!can("export:create")) {
    return (
      <section className="fms-exp-page">
        <div className="fms-exp-403" aria-live="polite">
          <span className="fms-exp-403__icon">{LOCK_ICON}</span>
          <h1 className="fms-exp-403__title">Export access required</h1>
          <p className="fms-exp-403__body">
            You don&apos;t have permission to export data. Contact an administrator if you need
            export access.
          </p>
        </div>
      </section>
    );
  }

  function applyPreset(id: string) {
    if (id === "custom") {
      setRangePreset("custom");
      setEndError(null);
      return;
    }
    const next = computePresetRange(id);
    if (!next) return;
    setRange(next);
    setRangePreset(id);
    setPresetNonce((n) => n + 1);
    setEndError(null);
  }

  function validateRange(start: string, end: string) {
    if (start && end && !isRangeValid(start, end)) {
      setEndError("End date must be on or after the start date.");
    } else {
      setEndError(null);
    }
  }

  function handleStartChange(e: ChangeEvent<HTMLInputElement>) {
    const value = e.target.value;
    setRange((prev) => ({ ...prev, start: value }));
    setRangePreset("custom");
    validateRange(value, range.end);
  }

  function handleEndChange(e: ChangeEvent<HTMLInputElement>) {
    const value = e.target.value;
    setRange((prev) => ({ ...prev, end: value }));
    setRangePreset("custom");
    validateRange(range.start, value);
  }

  function handleEndBlur() {
    validateRange(range.start, range.end);
  }

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (submitting) return;
    if (!isRangeValid(range.start, range.end)) {
      setEndError("End date must be on or after the start date.");
      return;
    }
    setSubmitting(true);
    setSubmitError(null);
    try {
      const job = await createExport(
        { format, range: { start: range.start, end: range.end } },
        { idempotencyKey: makeIdempotencyKey() },
      );
      setJobId(job.id);
    } catch {
      // Deliberately generic and solution-oriented; never echo a server/URL.
      setSubmitError("Could not create the export. Please try again.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section className="fms-exp-page">
      <h1 className="fms-exp-title">Export Data</h1>

      <form className="fms-exp-card" onSubmit={handleSubmit} noValidate>
        <fieldset className="fms-exp-fieldset">
          <legend className="fms-exp-legend">Format</legend>
          <div className="fms-exp-radio-group" role="radiogroup" aria-label="Format">
            {(["CSV", "PDF"] as ExportFormat[]).map((value) => (
              <label className="fms-exp-radio" key={value}>
                <input
                  type="radio"
                  name="format"
                  value={value}
                  checked={format === value}
                  onChange={() => setFormat(value)}
                />
                <span className="fms-exp-radio__indicator" aria-hidden="true" />
                <span className="fms-exp-radio__label">{value}</span>
              </label>
            ))}
          </div>
        </fieldset>

        <fieldset className="fms-exp-fieldset">
          <legend className="fms-exp-legend">Date Range</legend>
          <div className="fms-exp-chips">
            {RANGE_PRESETS.map((preset) => (
              <button
                key={preset.id}
                type="button"
                className={`fms-exp-chip${rangePreset === preset.id ? " fms-exp-chip--active" : ""}`}
                aria-pressed={rangePreset === preset.id}
                onClick={() => applyPreset(preset.id)}
              >
                {preset.label}
              </button>
            ))}
          </div>
          <div className="fms-exp-range">
            <Field
              key={`preset-${presetNonce}-start`}
              id="fms-exp-start"
              label="Start date"
              hint="First day of the range"
            >
              <input type="date" id="fms-exp-start" value={range.start} onChange={handleStartChange} />
            </Field>
            <Field
              key={`preset-${presetNonce}-end`}
              id="fms-exp-end"
              label="End date"
              error={endError}
              hint="Start must be on or before end"
            >
              <input
                type="date"
                id="fms-exp-end"
                value={range.end}
                onChange={handleEndChange}
                onBlur={handleEndBlur}
              />
            </Field>
          </div>
        </fieldset>

        <fieldset className="fms-exp-fieldset">
          <legend className="fms-exp-legend">Dataset Scope</legend>
          <div className="fms-exp-radio-group" role="radiogroup" aria-label="Dataset scope">
            {SCOPE_OPTIONS.map((opt) => (
              <label className="fms-exp-radio" key={opt.value}>
                <input
                  type="radio"
                  name="scope"
                  value={opt.value}
                  checked={scope === opt.value}
                  onChange={() => setScope(opt.value)}
                />
                <span className="fms-exp-radio__indicator" aria-hidden="true" />
                <span className="fms-exp-radio__label">{opt.label}</span>
              </label>
            ))}
          </div>
        </fieldset>

        <div className="fms-exp-submit">
          {!rangeValid && (
            <p className="fms-exp-submit__hint">Choose a valid date range to continue.</p>
          )}
          {submitError && <p className="fms-exp-submit__error">{submitError}</p>}
          <Button type="submit" variant="primary" size="lg" loading={submitting} disabled={!canSubmit}>
            {submitting ? "Creating export…" : "Create export"}
          </Button>
        </div>
      </form>

      {jobId && (
        <div
          ref={statusRef}
          className="fms-exp-status-region"
          tabIndex={-1}
          aria-label="Export status"
        >
          <ExportStatusPanel
            key={jobId}
            jobId={jobId}
            format={format}
            range={{ start: range.start, end: range.end }}
            onJobCreated={(id) => setJobId(id)}
          />
        </div>
      )}
    </section>
  );
}

/** Idempotency key for job creation (§7.2): UUID when available, else a
 * monotonic fallback. */
let exportSeq = 0;
function makeIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  exportSeq += 1;
  return `exp-page-${Date.now()}-${exportSeq}`;
}