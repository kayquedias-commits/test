import type { ReactElement } from 'react';
import { useNavigate } from 'react-router-dom';

import { EmptyState } from '../../components/ui/EmptyState';

/** Client-side onboarding destination; hardcoded so no open-redirect surface exists. */
const ONBOARDING_PATH = '/onboarding';

/**
 * First-run dashboard guidance. Renders the guided EmptyState atom with the
 * onboarding first steps and a single primary action that navigates to the
 * onboarding checklist. role="status" is passed to the atom so the welcome
 * copies are announced politely by assistive tech only when this mounts.
 */
export function EmptyDashboardState(): ReactElement {
  const navigate = useNavigate();

  return (
    <EmptyState
      role="status"
      title="Welcome to your financial dashboard"
      description="Start in three simple steps: create your first account → add a transaction → set up a budget."
      actionLabel="Get started"
      onAction={() => {
        navigate(ONBOARDING_PATH);
      }}
    />
  );
}

export default EmptyDashboardState;