/**
 * Non-error empty summary for a report whose dataset is empty (Blueprint §4.3
 * Module E boundary: empty datasets return empty summaries, never errors).
 * Composes the tone-neutral EmptyState so the blank result reads calm and
 * informational — a polite `role="status"` live region plus a single optional
 * next action. A report-scoped slice of a page must own the page's single <h1>;
 * this renders the EmptyState's <h2> title and preserves its heading hierarchy.
 *
 * `title` and `hint` come from feature pages and are intentionally rendered as
 * escaped text nodes only — they are never interpreted as HTML, and the single
 * CTA is a handler callback, never a prop-supplied URL, so no injection surface
 * is introduced here. The action button renders only when BOTH `actionLabel`
 * and `onAction` are supplied (mirrors EmptyState's pair contract; avoids a
 * dead control under view-only permissions).
 */
import type { ReactElement } from 'react';
import { EmptyState } from '../../components/ui/EmptyState';

export interface EmptyReportSummaryProps {
  /** Heading shown when the report has no rows in the selected date range. */
  title: string;
  /** Imperative guidance telling the user what to do next (e.g. widen the range). */
  hint: string;
  /** Label for the single next-best action; pair with onAction to render the CTA. */
  actionLabel?: string;
  /** Handler for the single CTA (a callback, never a URL string). */
  onAction?: () => void;
}

/** Decorative neutral document glyph; styled and rendered aria-hidden by EmptyState. */
const NEUTRAL_DOC_ICON = (
  <svg
    xmlns="http://www.w3.org/2000/svg"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth={1.5}
    strokeLinecap="round"
    strokeLinejoin="round"
    width={48}
    height={48}
  >
    <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
    <path d="M14 2v6h6" />
    <path d="M8 13h8" />
    <path d="M8 17h5" />
  </svg>
);

/**
 * Renders the tone-neutral empty-summary state for a report with no data:
 * polite status announcement, guiding hint, and a single CTA when supplied.
 */
export function EmptyReportSummary({
  title,
  hint,
  actionLabel,
  onAction,
}: EmptyReportSummaryProps): ReactElement {
  return (
    <EmptyState
      title={title}
      description={hint}
      tone="neutral"
      role="status"
      icon={NEUTRAL_DOC_ICON}
      actionLabel={actionLabel}
      onAction={onAction}
    />
  );
}

export default EmptyReportSummary;