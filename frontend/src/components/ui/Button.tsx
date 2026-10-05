/**
 * Shared accessible action control (primary/secondary/danger/ghost).
 *
 * Renders a native <button>, or an <a> when `href` is provided. The `danger`
 * variant is reserved for destructive-only confirmations and is the sole
 * variant using the semantic danger color (Von Restorff). The `loading` guard
 * is a UX/double-submit assist only — the server-side Idempotency-Key and
 * CSRF protections remain authoritative.
 */
import { useEffect } from 'react';
import type {
  AnchorHTMLAttributes,
  ButtonHTMLAttributes,
  KeyboardEvent,
  MouseEvent,
  ReactElement,
  ReactNode,
} from 'react';

export type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost';
export type ButtonSize = 'md' | 'lg';
export type ButtonOnClick = (
  e: MouseEvent<HTMLElement> | KeyboardEvent<HTMLElement>,
) => void;

export interface ButtonProps {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  disabled?: boolean;
  /** Inline SVG node (ReactElement) only. Never pass a raw HTML string. */
  icon?: ReactElement;
  children?: ReactNode;
  onClick?: ButtonOnClick;
  /** Defaults to 'button' so a shared control can never submit unexpectedly. */
  type?: 'button' | 'submit' | 'reset';
  /** Required when `children` is absent (icon-only button). */
  ariaLabel?: string;
  /** When provided the component renders an <a> instead of a <button>. */
  href?: string;
  target?: string;
  title?: string;
  id?: string;
  className?: string;
}

/**
 * Stylesheet colocated with the component. Injected once as a <style> element
 * (no per-element inline `style=` attributes) so the CSS-only spinner and
 * transform-only animation hold under a strict `style-src` CSP context.
 */
const FMS_BTN_CSS = `
:root {
  --color-primary:#4f46e5; --color-primary-hover:#4338ca; --color-primary-active:#3730a3;
  --color-danger:#dc2626; --color-danger-hover:#b91c1c; --color-danger-active:#991b1b;
  --color-secondary-bg:#ffffff; --color-secondary-border:#d1d5db;
  --color-secondary-hover:#f9fafb; --color-secondary-active:#f3f4f6;
  --color-text:#1f2937; --color-ghost-hover:#f3f4f6; --color-focus-ring:#4f46e5;
  --fms-space-2:8px; --fms-space-3:12px; --fms-space-4:16px; --fms-space-6:24px;
  --fms-text-sm:14px; --fms-text-base:16px; --fms-font-medium:500; --fms-font-semibold:600;
  --fms-leading-normal:1.5; --fms-radius-md:8px; --fms-radius-full:9999px;
  --fms-focus-width:3px; --fms-focus-offset:2px;
  --fms-transition-fast:150ms ease-out; --fms-transition-press:100ms ease-out;
}
.fms-btn{
  display:inline-flex; align-items:center; justify-content:center; gap:var(--fms-space-2);
  min-height:44px; padding:var(--fms-space-3) var(--fms-space-4); border:none;
  border-radius:var(--fms-radius-md); font-size:var(--fms-text-sm); line-height:var(--fms-leading-normal);
  font-weight:var(--fms-font-medium); cursor:pointer; user-select:none; text-decoration:none;
  white-space:nowrap;
  transition:background-color var(--fms-transition-fast),border-color var(--fms-transition-fast),
    color var(--fms-transition-fast),transform var(--fms-transition-press);
}
.fms-btn--lg{ min-height:48px; padding:var(--fms-space-3) var(--fms-space-6); font-size:var(--fms-text-base); }
.fms-btn--primary{ background-color:var(--color-primary); color:#ffffff; font-weight:var(--fms-font-semibold); }
.fms-btn--primary:hover{ background-color:var(--color-primary-hover); }
.fms-btn--primary:active{ background-color:var(--color-primary-active); transform:translateY(1px); }
.fms-btn--danger{ background-color:var(--color-danger); color:#ffffff; }
.fms-btn--danger:hover{ background-color:var(--color-danger-hover); }
.fms-btn--danger:active{ background-color:var(--color-danger-active); transform:translateY(1px); }
.fms-btn--secondary{ background-color:var(--color-secondary-bg); color:var(--color-text); border:1px solid var(--color-secondary-border); }
.fms-btn--secondary:hover{ background-color:var(--color-secondary-hover); }
.fms-btn--secondary:active{ background-color:var(--color-secondary-active); transform:translateY(1px); }
.fms-btn--ghost{ background-color:transparent; color:var(--color-text); }
.fms-btn--ghost:hover{ background-color:var(--color-ghost-hover); }
.fms-btn--ghost:active{ background-color:var(--color-ghost-hover); transform:translateY(1px); }
.fms-btn:focus-visible{ outline:var(--fms-focus-width) solid var(--color-focus-ring); outline-offset:var(--fms-focus-offset); }
.fms-btn--disabled{ opacity:0.5; cursor:not-allowed; }
.fms-btn--loading{ pointer-events:none; cursor:default; }
.fms-btn__icon{ display:inline-flex; flex:none; width:16px; height:16px; }
.fms-btn__spinner{ flex:none; width:16px; height:16px; border:2px solid transparent; border-top-color:currentColor; border-radius:var(--fms-radius-full); animation:fms-spin 700ms linear infinite; }
@keyframes fms-spin{ to{ transform:rotate(360deg); } }
.fms-btn__sr{ position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; border:0; }
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-button-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = FMS_BTN_CSS;
    document.head.appendChild(el);
  }
}

const ALLOWED_HREF_PROTOCOLS = ['http:', 'https:', 'mailto:', 'tel:'];

/**
 * Rejects `javascript:`, `data:`, and `vbscript:` URLs before rendering.
 * Relative (/), hash (#), and query (?) hrefs are always accepted.
 */
export function isSafeHref(href: string): boolean {
  if (/^[/#?]/.test(href)) return true;
  const colon = href.indexOf(':');
  if (colon === -1) return true;
  const scheme = href.slice(0, colon).toLowerCase();
  return ALLOWED_HREF_PROTOCOLS.indexOf(scheme) !== -1;
}

function classNames(parts: Array<string | false | undefined>): string {
  return parts.filter(Boolean).join(' ');
}

export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  disabled = false,
  icon,
  children,
  onClick,
  type = 'button',
  ariaLabel,
  href,
  target,
  title,
  id,
  className,
}: ButtonProps): ReactElement | null {
  const inactive = disabled || loading;
  const hasLabel = childCount(children) > 0;

  useEffect(() => {
    if (process.env.NODE_ENV !== 'development') return;
    if (icon !== undefined && typeof icon !== 'object') {
      console.warn(
        'Button icon must be a ReactElement; raw strings are not rendered to avoid XSS.',
      );
    }
    if (!hasLabel && !ariaLabel) {
      console.warn('Button with no visible label requires an ariaLabel prop.');
    }
  }, [icon, hasLabel, ariaLabel]);

  const cls = classNames([
    'fms-btn',
    `fms-btn--${variant}`,
    size === 'lg' ? 'fms-btn--lg' : '',
    disabled && !loading ? 'fms-btn--disabled' : '',
    loading ? 'fms-btn--loading' : '',
    className,
  ]);

  // Spinner swaps the icon slot (same 16px footprint) so geometry never shifts.
  const deco = loading ? (
    <span className="fms-btn__spinner" aria-hidden="true" />
  ) : icon ? (
    <span className="fms-btn__icon" aria-hidden="true">
      {icon}
    </span>
  ) : null;

  // Live region sits OUTSIDE any aria-hidden ancestor so it is announced.
  const liveStatus = loading ? (
    <span className="fms-btn__sr" role="status">
      Loading
    </span>
  ) : null;

  // Label is kept accessible/visible during loading so the control keeps its
  // stable accessible name; the sr-only live region adds the busy disclosure.
  const label = hasLabel ? <span>{children}</span> : null;

  const commonProps = {
    id,
    title,
    'aria-label': ariaLabel,
    'aria-busy': loading ? true : undefined,
    className: cls,
    onClick: (e: MouseEvent<HTMLElement>) => {
      if (inactive) {
        e.preventDefault();
        e.stopPropagation();
        return;
      }
      onClick?.(e);
    },
  };

  if (href && isSafeHref(href)) {
    const isBlank = target === '_blank';
    return (
      <a
        href={href}
        target={target}
        rel={isBlank ? 'noopener noreferrer' : undefined}
        tabIndex={disabled && !loading ? -1 : undefined}
        aria-disabled={inactive ? true : undefined}
        {...(commonProps as AnchorHTMLAttributes<HTMLAnchorElement>)}
      >
        {deco}
        {label}
        {liveStatus}
      </a>
    );
  }

  return (
    <button
      type={type}
      disabled={disabled && !loading}
      aria-disabled={loading ? true : undefined}
      {...(commonProps as ButtonHTMLAttributes<HTMLButtonElement>)}
    >
      {deco}
      {label}
      {liveStatus}
    </button>
  );
}

/** Number of children rendered, counting fragments/arrays recursively. */
function childCount(node: ReactNode): number {
  if (node === null || node === undefined || node === false) return 0;
  if (Array.isArray(node)) {
    return node.reduce((sum, child) => sum + childCount(child), 0);
  }
  if (typeof node === 'object' && 'props' in node && node.props?.children) {
    return childCount(node.props.children);
  }
  return 1;
}

export default Button;