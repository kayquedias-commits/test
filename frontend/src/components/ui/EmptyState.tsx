/**
 * Guided empty-state atom — converts a blank result into a single next action
 * (e.g. "Create your first account", "Set up a budget"). Copy is supplied by
 * feature pages, never embedded here, so messaging stays consistent across the
 * product. The optional action renders exactly one primary Button when BOTH
 * `actionLabel` and `onAction` are provided.
 *
 * View-only/no-action consumers simply omit the pair: the atom renders icon +
 * title + description with no button and no reserved space — the absence of the
 * affordance is itself the permission signal, so no disabled button is emitted.
 *
 * Post-load empty datasets announce politely: pass `role="status"` (or wrap the
 * component) on the consumer side. The atom never hard-codes the live region so
 * an empty state that is present at initial render does not double-announce.
 */
import type { ReactElement } from 'react';
import { Button } from './Button';
import type { ButtonOnClick } from './Button';

export type EmptyStateTone = 'neutral' | 'info';

export interface EmptyStateProps {
  title: string;
  description: string;
  actionLabel?: string;
  onAction?: ButtonOnClick;
  /** Decorative inline SVG icon. Rendered aria-hidden; never a raw HTML string. */
  icon?: ReactElement;
  /** Decorative illustration. Rendered aria-hidden; consumers supply alt="" on any <img>. */
  illustration?: ReactElement;
  /** Only the icon color shifts for 'info'; text stays neutral for readability. */
  tone?: EmptyStateTone;
  /** Restricted allowlist — className, id, role only (no prop spreading onto root). */
  className?: string;
  id?: string;
  /** e.g. role="status" for a post-load empty dataset (consumer-owned live region). */
  role?: string;
}

/**
 * Stylesheet colocated with the component, injected once as a <style> element
 * (no per-element inline `style=` attributes or event handlers) so the entry
 * animation and layout hold under a strict `style-src`/`script-src` CSP.
 */
const FMS_EMPTY_CSS = `
.empty-state{
  display:flex; flex-direction:column; align-items:center; justify-content:center;
  text-align:center; width:100%; min-height:18rem;
  padding:48px 24px;
  background:#fafafa; border:1px dashed #d4d4d8; border-radius:12px;
  animation:empty-state-enter 200ms ease-out;
}
.empty-state__icon{ width:3rem; height:3rem; margin-bottom:16px; }
.empty-state--tone-neutral .empty-state__icon{ color:#71717a; }
.empty-state--tone-info .empty-state__icon{ color:#2563eb; }
.empty-state__illustration{ max-width:12rem; height:auto; margin-bottom:24px; }
.empty-state__title{
  font-size:20px; font-weight:600; line-height:1.25; color:#18181b;
  margin:0 0 8px;
}
.empty-state__description{
  font-size:16px; font-weight:400; line-height:1.5; color:#52525b;
  max-width:65ch; margin:0 0 24px;
}
.empty-state__action{ margin-top:8px; width:100%; display:flex; justify-content:center; }
@keyframes empty-state-enter{
  from{ opacity:0; transform:translateY(8px); }
  to{ opacity:1; transform:translateY(0); }
}
@media (max-width:767px){
  .empty-state{ min-height:auto; padding:32px 16px; }
  /* Full-width action sits in the natural thumb zone on mobile. */
  .empty-state__action .fms-btn{ width:100%; }
}
@media (prefers-reduced-motion: reduce){
  .empty-state{ animation:none; }
}
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-empty-state-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = FMS_EMPTY_CSS;
    document.head.appendChild(el);
  }
}

/**
 * Renders the guided empty state: decorative illustration/icon, semantic <h2>
 * title (page owns its single <h1>), body description, and — when both
 * `actionLabel` and `onAction` are supplied — a single primary action Button.
 * All text is rendered via React text children only (auto-escaped); icons are
 * ReactElements, which eliminates URL-scheme injection entirely.
 */
export function EmptyState({
  title,
  description,
  actionLabel,
  onAction,
  icon,
  illustration,
  tone = 'neutral',
  className,
  id,
  role,
}: EmptyStateProps): ReactElement | null {
  if (process.env.NODE_ENV === 'development') {
    if (typeof title !== 'string' || title.trim() === '') {
      console.warn('EmptyState requires a non-empty unique `title` for heading semantics.');
    } else if (typeof description !== 'string' || description.trim() === '') {
      console.warn('EmptyState requires a non-empty `description`; use tone="info" and the icon to convey support guidance.');
    }
    if ((actionLabel && !onAction) || (!actionLabel && onAction)) {
      // Always supplied as a pair; a lone button label or handler would render
      // a dead control (Hick's Law / RBAC), so we drop the action entirely.
      console.warn('EmptyState requires BOTH `actionLabel` and `onAction`; the action is omitted when only one is present.');
    }
  }

  const hasAction = Boolean(actionLabel && onAction);
  const rootClass = ['empty-state', `empty-state--tone-${tone}`, className]
    .filter(Boolean)
    .join(' ');

  return (
    <div className={rootClass} id={id} role={role}>
      {illustration ? (
        <div className="empty-state__illustration" aria-hidden="true">
          {illustration}
        </div>
      ) : icon ? (
        <span className="empty-state__icon" aria-hidden="true">
          {icon}
        </span>
      ) : null}
      <h2 className="empty-state__title">{title}</h2>
      <p className="empty-state__description">{description}</p>
      {hasAction ? (
        <div className="empty-state__action">
          <Button variant="primary" size="lg" onClick={onAction}>
            {actionLabel}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

export default EmptyState;