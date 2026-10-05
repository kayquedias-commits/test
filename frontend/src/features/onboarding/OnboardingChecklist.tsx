/**
 * Route content for /onboarding: a non-blocking, 3-step first-actions checklist
 * (Blueprint §6/§8.1) that guides a fresh user from an empty dashboard to a
 * working ledger. It is a checklist, not a wizard - the user can navigate
 * anywhere and each step is a direct internal link to its form.
 *
 * Status is never stored: each step derives its state from a lightweight,
 * read-only count fetched on mount and refreshed whenever the tab regains
 * focus (cross-tab consistency). A step is Completed when its count > 0; the
 * first non-completed step is "In progress" (the next actionable item, driving
 * the goal-gradient pull); later steps are "Pending".
 *
 * @security This page performs no mutations (GET counts only) and renders only
 * numeric aggregates - never entity names, amounts, or descriptions. All
 * fetched counts are coerced/validated via clampCount and the progress value is
 * clamped to [0,1], so a manipulated response can never corrupt the UI. nav
 * hrefs come exclusively from the static STEPS / DASHBOARD_PATH constants.
 */
import { useCallback, useEffect, useState } from 'react';
import type { ReactElement } from 'react';
import { Button } from '../../components/ui/Button';
import {
  listAccounts,
  listTransactions,
  listBudgets,
} from '../../services/api/endpoints';
import type { OffsetPage, CursorPage } from '../../services/api/types';

/** Static internal routes - never derived from API data (CWE-601/CWE-79). */
const STEPS = [
  {
    key: 'account',
    label: 'Create your first account',
    description:
      'Set up an account to track balances and transactions from day one.',
    href: '/accounts/new',
  },
  {
    key: 'transaction',
    label: 'Add your first transaction',
    description:
      'Record an income or expense to start building your financial ledger.',
    href: '/transactions/new',
  },
  {
    key: 'budget',
    label: 'Set up a budget',
    description:
      'Allocate spending per category so you can monitor against a plan.',
    href: '/budgets/new',
  },
] as const;

const DASHBOARD_PATH = '/';
const TOTAL_STEPS = 3;

type StepKey = (typeof STEPS)[number]['key'];
type Counts = Record<StepKey, number>;
type Phase = 'loading' | 'ready' | 'error';

/** Validate an untrusted count: only a non-negative integer passes; anything
 * else (string, NaN, negative, fractional) coerces to 0 so a malformed API
 * response can never poison status derivation. */
function clampCount(value: unknown): number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : 0;
}

const ONBOARDING_CSS = `
.obc{ width:100%; max-width:520px; margin-inline:auto; background:var(--color-bg-surface,#ffffff);
  border-radius:var(--radius-lg,12px); box-shadow:var(--shadow-md,0 4px 6px -1px rgba(0,0,0,.1));
  padding:var(--space-6,24px); }
.obc__title{ font-size:var(--text-2xl,24px); font-weight:var(--font-semibold,600);
  line-height:var(--leading-tight,1.25); color:var(--color-text-primary,#1f2937);
  margin:0 0 var(--space-1,4px); }
.obc__intro{ font-size:var(--text-sm,14px); line-height:var(--leading-normal,1.5);
  color:var(--color-text-secondary,#4b5563); margin:0 0 var(--space-4,16px); }
.obc__progress{ display:flex; flex-direction:column; gap:var(--space-2,8px);
  margin-bottom:var(--space-6,24px); }
.obc__progress-bar{ position:relative; height:8px; border-radius:var(--radius-full,9999px);
  background:var(--color-bg-secondary,#e2e8f0); overflow:hidden; }
.obc__progress-fill{ position:absolute; inset:0; transform-origin:left center;
  background:var(--color-primary,#4f46e5); border-radius:var(--radius-full,9999px);
  transition:transform var(--transition-base,200ms) ease-out; }
.obc__progress-label{ display:flex; justify-content:space-between; gap:var(--space-2,8px);
  font-size:var(--text-sm,14px); font-weight:var(--font-medium,500);
  color:var(--color-text-secondary,#4b5563); }
.obc__list{ list-style:none; margin:0; padding:0; display:flex; flex-direction:column;
  gap:var(--space-4,16px); }
.obc__step{ display:flex; align-items:center; gap:var(--space-4,16px); }
.obc__step-body{ flex:1 1 auto; min-width:0; }
.obc__step-title{ font-size:var(--text-base,16px); font-weight:var(--font-semibold,600);
  line-height:var(--leading-normal,1.5); color:var(--color-text-primary,#1f2937);
  margin:0; }
.obc__step-desc{ font-size:var(--text-sm,14px); line-height:var(--leading-normal,1.5);
  color:var(--color-text-secondary,#4b5563); margin:var(--space-1,4px) 0 0; max-width:65ch; }
.obc__status{ font-size:var(--text-sm,14px); font-weight:var(--font-medium,500);
  color:var(--color-text-secondary,#4b5563); display:inline-flex; align-items:center;
  gap:var(--space-1,4px); margin-top:var(--space-1,4px); }
.obc__status--done{ color:var(--color-success-dark,#276749); }
.obc__status--next{ color:var(--color-warning-dark,#b7791f); }
.obc__icon{ flex:none; width:24px; height:24px; }
.obc__step-action{ flex:none; }
.obc__error{ text-align:center; padding:var(--space-6,24px) 0; }
.obc__error p{ font-size:var(--text-sm,14px); line-height:var(--leading-normal,1.5);
  color:var(--color-text-secondary,#4b5563); margin:0 0 var(--space-4,16px); }
.obc__done{ text-align:center; padding:var(--space-8,32px) var(--space-4,16px);
  opacity:1; transition:opacity var(--transition-slow,300ms) ease-out; }
.obc__done-icon{ width:48px; height:48px; color:var(--color-success,#38a169);
  margin-inline:auto; display:block; margin-bottom:var(--space-4,16px); }
.obc__done-title{ font-size:var(--text-2xl,24px); font-weight:var(--font-semibold,600);
  line-height:var(--leading-tight,1.25); color:var(--color-text-primary,#1f2937);
  margin:0 0 var(--space-2,8px); }
.obc__done-text{ font-size:var(--text-base,16px); line-height:var(--leading-normal,1.5);
  color:var(--color-text-secondary,#4b5563); margin:0 0 var(--space-6,24px); }
.obc__skeleton{ width:100%; max-width:520px; margin-inline:auto; background:var(--color-bg-surface,#ffffff);
  border-radius:var(--radius-lg,12px); box-shadow:var(--shadow-md,0 4px 6px -1px rgba(0,0,0,.1));
  padding:var(--space-6,24px); display:flex; flex-direction:column; gap:var(--space-4,16px); }
.obc__sk{ background:var(--color-bg-secondary,#e2e8f0); border-radius:var(--radius-sm,4px); }
.obc__sk--title{ width:60%; height:28px; margin-bottom:var(--space-2,8px); }
.obc__sk--intro{ width:90%; height:14px; }
.obc__sk--bar{ width:100%; height:8px; border-radius:var(--radius-full,9999px);
  margin:var(--space-2,8px) 0 var(--space-6,24px); }
.obc__sk--row{ width:100%; height:48px; }
.obc__sr-only{ position:absolute; width:1px; height:1px; padding:0; margin:-1px;
  overflow:hidden; clip:rect(0 0 0 0); white-space:nowrap; border:0; }
@media (max-width:767px){
  .obc__step{ flex-direction:column; align-items:stretch; }
  .obc__step-action{ width:100%; }
  .obc__step-action button, .obc__step-action a { width:100%; }
}
@media (prefers-reduced-motion:reduce){
  .obc *{ transition:none !important; animation:none !important; }
}
`;

/** Check icon for a completed step (decorative; text carries the meaning). */
function CheckIcon(): ReactElement {
  return (
    <svg
      className="obc__icon"
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="12" cy="12" r="10" />
      <path d="m9 12 2 2 4-4" />
    </svg>
  );
}

/** Arrow icon for the in-progress (next actionable) step. */
function ArrowIcon(): ReactElement {
  return (
    <svg
      className="obc__icon"
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="12" cy="12" r="10" />
      <path d="M12 8v8M8 12l4 4 4-4" />
    </svg>
  );
}

/** Circle icon for a pending (not yet actionable) step. */
function CircleIcon(): ReactElement {
  return (
    <svg
      className="obc__icon"
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
    >
      <circle cx="12" cy="12" r="10" />
    </svg>
  );
}

/** Progress bar: transform-only width via scaleX, ARIA-native progress role,
 * and a visible label pairing the step count with the percentage. */
function Progress({ now, label }: { now: number; label: string }): ReactElement {
  const fraction = Math.min(1, Math.max(0, now / TOTAL_STEPS));
  const percent = Math.round(fraction * 100);
  return (
    <div
      className="obc__progress"
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={TOTAL_STEPS}
      aria-valuenow={now}
      aria-label={label}
    >
      <div className="obc__progress-bar">
        <span
          className="obc__progress-fill"
          style={{ transform: `scaleX(${fraction})` }}
        />
      </div>
      <div className="obc__progress-label">
        <span>{label}</span>
        <span>{`${percent}%`}</span>
      </div>
    </div>
  );
}

/**
 * Renders the onboarding first-actions checklist. Exported for the route and
 * component tests. The checklist never blocks: every pending/in-progress step
 * links straight to its form, and the completed state offers a single
 * "Go to dashboard" action (Hick's law, peak-end payoff).
 */
export function OnboardingChecklist(): ReactElement {
  const [phase, setPhase] = useState<Phase>('loading');
  const [counts, setCounts] = useState<Counts>({
    account: 0,
    transaction: 0,
    budget: 0,
  });

  /** Fetches the three lightweight counts. Rejected/garbled responses land in
   * the neutral error state rather than a misleading "Pending" list. */
  const load = useCallback(async () => {
    setPhase((p) => (p === 'ready' ? 'ready' : 'loading'));
    const results = await Promise.allSettled([
      listAccounts(),
      listBudgets(),
      listTransactions({ limit: 100 }),
    ]);
    if (results.some((r) => r.status === 'rejected')) {
      setPhase('error');
      return;
    }
    const accounts = results[0] as PromiseFulfilledResult<OffsetPage<unknown>>;
    const budgets = results[1] as PromiseFulfilledResult<OffsetPage<unknown>>;
    const transactions = results[2] as PromiseFulfilledResult<CursorPage<unknown>>;
    setCounts({
      account: clampCount(accounts.value.total),
      budget: clampCount(budgets.value.total),
      transaction: clampCount(transactions.value.items.length),
    });
    setPhase('ready');
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Cross-tab consistency (H5/H6): a user may complete a step in another tab
  // or via the dashboard; refresh status when this tab is refocused.
  useEffect(() => {
    const refetch = () => {
      void load();
    };
    window.addEventListener('focus', refetch);
    document.addEventListener('visibilitychange', refetch);
    return () => {
      window.removeEventListener('focus', refetch);
      document.removeEventListener('visibilitychange', refetch);
    };
  }, [load]);

  const completedCount = STEPS.filter((s) => counts[s.key] > 0).length;
  const inProgressIndex = STEPS.findIndex((s) => counts[s.key] === 0);
  const allDone = completedCount === TOTAL_STEPS;
  const progressLabel = `${completedCount} of ${TOTAL_STEPS} steps completed`;

  if (phase === 'loading') {
    return (
      <main>
        <div className="obc__skeleton" aria-hidden="true">
          <div className="obc__sk obc__sk--title" />
          <div className="obc__sk obc__sk--intro" />
          <div className="obc__sk obc__sk--bar" />
          <div className="obc__sk obc__sk--row" />
          <div className="obc__sk obc__sk--row" />
          <div className="obc__sk obc__sk--row" />
        </div>
        <style>{ONBOARDING_CSS}</style>
      </main>
    );
  }

  if (phase === 'error') {
    return (
      <main>
        <section className="obc" aria-label="Onboarding">
          <div className="obc__error">
            <h1 className="obc__title">Get started with your finances</h1>
            <p>Couldn&apos;t load your progress.</p>
            <Button variant="secondary" onClick={() => void load()}>
              Retry
            </Button>
          </div>
        </section>
        <style>{ONBOARDING_CSS}</style>
      </main>
    );
  }

  if (allDone) {
    return (
      <main>
        <section className="obc" aria-label="Onboarding complete">
          <div className="obc__done">
            <svg
              className="obc__done-icon"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
              <path d="m9 11 3 3L22 4" />
            </svg>
            <h1 className="obc__done-title">You&apos;re all set!</h1>
            <p className="obc__done-text">
              Your finances are ready to track. Head to your dashboard to start
              reviewing your money.
            </p>
            <div className="obc__step-action">
              <Button
                variant="primary"
                size="lg"
                href={DASHBOARD_PATH}
                aria-label="Go to dashboard"
              >
                Go to dashboard
              </Button>
            </div>
          </div>
          <p className="obc__sr-only" role="status" aria-live="polite">
            You have completed all {TOTAL_STEPS} onboarding steps.
          </p>
        </section>
        <style>{ONBOARDING_CSS}</style>
      </main>
    );
  }

  return (
    <main>
      <section className="obc" aria-label="Onboarding">
        <h1 className="obc__title">Get started with your finances</h1>
        <p className="obc__intro">
          A few quick steps to set up your financial workspace.
        </p>

        <Progress now={completedCount} label={progressLabel} />

        <ul className="obc__list">
          {STEPS.map((step, index) => {
            const isDone = counts[step.key] > 0;
            const isNext = index === inProgressIndex;
            const statusClass = isDone
              ? 'obc__status--done'
              : isNext
                ? 'obc__status--next'
                : '';
            const icon = isDone ? (
              <CheckIcon />
            ) : isNext ? (
              <ArrowIcon />
            ) : (
              <CircleIcon />
            );
            const statusText = isDone
              ? 'Completed'
              : isNext
                ? 'In progress'
                : 'Pending';
            return (
              <li className="obc__step" key={step.key}>
                <span
                  className={`obc__status ${statusClass}`}
                  aria-hidden="true"
                >
                  {icon}
                </span>
                <div className="obc__step-body">
                  <h2 className="obc__step-title">{step.label}</h2>
                  <p className="obc__step-desc">{step.description}</p>
                  <span className={`obc__status ${statusClass}`}>
                    {statusText}
                  </span>
                </div>
                <div className="obc__step-action">
                  {isDone ? null : (
                    <Button
                      variant={isNext ? 'primary' : 'ghost'}
                      size="md"
                      href={step.href}
                    >
                      {step.label}
                    </Button>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
        <style>{ONBOARDING_CSS}</style>
      </section>
    </main>
  );
}

export default OnboardingChecklist;