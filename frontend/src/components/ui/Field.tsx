import { cloneElement, isValidElement, useRef, useState } from "react";
import type {
  ChangeEvent,
  FocusEvent,
  ReactElement,
  ReactNode,
} from "react";
import { type Validator } from "../../utils/validation";

/* ------------------------------------------------------------------------- *
 * Colocated styles, injected once as a synthetic <style>. No inline styles,
 * no style/src built from user data (CSP-clean). Token names follow the
 * existing --fms-* convention; all values fall back to safe defaults when a
 * global token is absent.
 * ------------------------------------------------------------------------- */
const FMS_FIELD_CSS = `
:root{
  --color-label-text:#1f2937;--color-hint-text:#475569;--color-field-bg:#ffffff;
  --color-field-border:#9ca3af;--color-field-border-hover:#6b7280;
  --color-field-border-focus:#2563eb;--color-field-border-error:#b91c1c;
  --color-field-border-success:#15803d;--color-field-error-text:#b91c1c;
  --color-field-success-text:#15803d;--color-field-disabled-bg:#f3f4f6;
  --color-field-disabled-text:#9ca3af;
  --fms-field-height:44px;--fms-field-message-min-h:24px;
}
.fms-field{display:block;width:100%;margin-bottom:var(--fms-space-4,16px)}
.fms-field__label{display:block;font-size:var(--fms-text-sm,14px);font-weight:var(--fms-font-medium,500);line-height:var(--fms-leading-normal,1.5);color:var(--color-label-text);margin-bottom:var(--fms-space-2,8px)}
.fms-field__required{display:inline-block;margin-left:var(--fms-space-2,8px);font-size:var(--fms-text-xs,12px);font-weight:var(--fms-font-normal,400);color:var(--color-hint-text)}
.fms-field__control{display:block;width:100%;min-height:44px;padding:var(--fms-space-2,8px) var(--fms-space-3,12px);font-size:var(--fms-text-base,16px);line-height:var(--fms-leading-normal,1.5);color:#1f2937;background:var(--color-field-bg);border:1px solid var(--color-field-border);border-radius:var(--fms-radius-md,8px);transition:border-color var(--fms-transition-fast,150ms ease-out)}
.fms-field__control:hover{border-color:var(--color-field-border-hover)}
.fms-field__control:focus-visible{outline:2px solid var(--color-field-border-focus);outline-offset:2px;border-color:var(--color-field-border-focus)}
.fms-field--invalid .fms-field__control{border-color:var(--color-field-border-error)}
.fms-field--success .fms-field__control{border-color:var(--color-field-border-success)}
.fms-field__control:disabled{background:var(--color-field-disabled-bg);color:var(--color-field-disabled-text);cursor:not-allowed}
.fms-field__message{min-height:var(--fms-field-message-min-h);margin-top:var(--fms-space-1,4px);display:flex;align-items:center;gap:var(--fms-space-1,4px);font-size:var(--fms-text-sm,14px);font-weight:var(--fms-font-medium,500);opacity:0;transform:translateY(-2px);transition:opacity var(--fms-transition-fast,150ms ease-out),transform var(--fms-transition-fast,150ms ease-out);pointer-events:none}
.fms-field__message--visible{opacity:1;transform:translateY(0)}
.fms-field__message--error{color:var(--color-field-error-text)}
.fms-field__message--success{color:var(--color-field-success-text)}
.fms-field__message--hint{color:var(--color-hint-text);font-weight:var(--fms-font-normal,400)}
.fms-field__icon{flex:none;width:16px;height:16px}
.fms-field__sr{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-field-styles")) {
  const style = document.createElement("style");
  style.id = "fms-field-styles";
  style.textContent = FMS_FIELD_CSS;
  document.head.appendChild(style);
}

/* ------------------------------------------------------------------------- *
 * Non-visual helpers
 * ------------------------------------------------------------------------- */

/**
 * Safe, non-user-derived field id source: a module counter fallback ensures
 * ids can never carry user text that would leak into id/htmlFor/aria ids.
 */
let fieldSeq = 0;

/** Run validators in order; return the first failing message, else null. */
function runValidators(value: string, validators: Validator[]): string | null {
  for (const validate of validators) {
    const message = validate(value);
    if (message) return message;
  }
  return null;
}

function CheckIcon({ variant, className }: { variant: "error" | "success"; className?: string }): ReactElement {
  const stroke = variant === "error" ? "var(--color-field-error-text)" : "var(--color-field-success-text)";
  const body =
    variant === "error" ? (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 8v4" />
        <path d="M12 16h.01" />
      </>
    ) : (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M8.5 12.5l2.5 2.5 4.5-5" />
      </>
    );
  return (
    <svg
      className={className || "fms-field__icon"}
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke={stroke}
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {body}
    </svg>
  );
}

/**
 * Accessible form field wrapper. Owns the generated child id and binds the
 * <label> via htmlFor; wires hint/error through aria-describedby; runs inline
 * blur validation (per field, not only on submit). Error/success are always
 * icon + text, never color alone. All message text is rendered as escaped
 * text content.
 */
export interface FieldProps {
  /** Caller-supplied id; when absent one is auto-generated and injected. */
  id?: string;
  label: string;
  required?: boolean;
  /** Server error mapped through validation.ts; never a raw API payload. */
  error?: string | null;
  hint?: string;
  disabled?: boolean;
  children?: ReactNode;
  validate?: Validator[];
}

const CONTROL_TAGS = new Set(["INPUT", "SELECT", "TEXTAREA"]);

export function Field({
  id,
  label,
  required = false,
  error = null,
  hint,
  disabled,
  children,
  validate = [],
}: FieldProps): ReactElement | null {
  // Stable ids created once per mount; field id never derives from user text.
  const idsRef = useRef<{ field: string; hint: string; error: string } | null>(null);
  if (idsRef.current === null) {
    const base = id ?? `fms-field-${++fieldSeq}`;
    idsRef.current = { field: base, hint: `${base}-hint`, error: `${base}-error` };
  }
  const ids = idsRef.current;

  const child = isValidElement(children) ? (children as ReactElement) : null;
  const childProps = child ? (child.props as Record<string, unknown>) : {};
  const childId = typeof childProps.id === "string" ? childProps.id : undefined;
  const controlId = childId ?? ids.field;
  const hasChildOnBlur = typeof childProps.onBlur === "function";
  const hasChildOnChange = typeof childProps.onChange === "function";

  // Only clone controlled input/select/textarea-shaped children; anything else
  // renders untouched to avoid injecting event surfaces into arbitrary nodes.
  const canClone = child !== null && CONTROL_TAGS.has(String(child.type));

  const [value, setValue] = useState<string>(() => {
    if (!child) return "";
    const v = childProps.value ?? childProps.defaultValue;
    return typeof v === "string" || typeof v === "number" ? String(v) : "";
  });
  const [touched, setTouched] = useState<boolean>(false);

  const serverInvalid = typeof error === "string" && error.length > 0;
  const haveValidators = validate.length > 0;
  const localMessage = canClone && haveValidators ? runValidators(value, validate) : null;
  const invalid = serverInvalid || (touched && localMessage !== null);

  // Success only ever renders after a blur-touch on a now-valid field.
  const showSuccess = touched && !invalid && !serverInvalid && haveValidators && canClone;
  const hasError = invalid;

  // aria-describedby points only at ids present in the DOM: the hint always,
  // the error only while invalid. A visibility-hidden error is out of the
  // a11y tree, so excluding it while hidden is correct.
  const describedby = hasError ? ids.error : hint ? ids.hint : undefined;

  let hasChildBlurInline = false;
  if (child && typeof childProps.onBlur === "function") hasChildBlurInline = true;
  let hasChildChangeInline = false;
  if (child && typeof childProps.onChange === "function") hasChildChangeInline = true;

  let control: ReactElement | null = null;
  if (child && canClone) {
    const childClassName = typeof childProps.className === "string" ? childProps.className : "";
    const onBlur = hasChildBlurInline
      ? (e: FocusEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>): void => {
          setTouched(true);
          (childProps.onBlur as (ev: FocusEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => void)(e);
        }
      : (e: FocusEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>): void => {
          setTouched(true);
        };
    const onChange = hasChildChangeInline
      ? (e: ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>): void => {
          setValue(e.target.value);
          (childProps.onChange as (ev: ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => void)(e);
        }
      : (e: ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>): void => {
          setValue(e.target.value);
        };
    control = cloneElement<Record<string, unknown>>(child, {
      id: controlId,
      value,
      disabled,
      required,
      onBlur,
      onChange,
      "aria-invalid": hasError ? true : undefined,
      "aria-required": required ? true : undefined,
      "aria-describedby": describedby,
      className: `fms-field__control${childClassName ? ` ${childClassName}` : ""}`,
    });
  } else if (child && !canClone) {
    // Non-controllable child: render untouched, still bound to the label.
    control = child;
  }

  let messageClass: "error" | "success" | "hint" = "hint";
  let messageBody: ReactNode = null;
  let messageVisible = false;

  if (hasError) {
    messageClass = "error";
    messageBody = (
      <>
        <CheckIcon variant="error" className="fms-field__icon" />
        <span>{serverInvalid ? error : localMessage}</span>
      </>
    );
    messageVisible = true;
  } else if (showSuccess) {
    messageClass = "success";
    messageBody = (
      <>
        <CheckIcon variant="success" className="fms-field__icon" />
        <span>Valid</span>
      </>
    );
    messageVisible = true;
  } else if (hint) {
    messageClass = "hint";
    messageBody = <span>{hint}</span>;
    messageVisible = true;
  }

  const wrapperClass = `fms-field${hasError ? " fms-field--invalid" : ""}${showSuccess ? " fms-field--success" : ""}`;

  return (
    <div className={wrapperClass}>
      <label className="fms-field__label" htmlFor={controlId}>
        {label}
        {required ? <span className="fms-field__required">Required</span> : null}
      </label>
      {control}
      <div
        className={`fms-field__message fms-field__message--${messageClass}${messageVisible ? " fms-field__message--visible" : ""}`}
        id={ids.error}
        role={hasError ? "alert" : undefined}
      >
        {messageBody}
      </div>
    </div>
  );
}