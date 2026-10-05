import {
  useEffect,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
} from 'react';
import { ConfirmDialog } from '../../components/ui/ConfirmDialog';
import type { TransactionDTO } from '../../services/api/types';
import { useTransactionsStore } from '../../app/stores/transactions';
import { formatMinorUnits } from '../../utils/currency';
import { formatDate } from '../../utils/date';

export interface DeleteTransactionDialogProps {
  /** The transaction being deleted. */
  transaction: TransactionDTO;
  /** Whether the dialog is mounted/open. */
  open: boolean;
  /** Close without deleting. Also used for the 409 (conflict) close so the
   *  caller surfaces ConflictRecoveryBanner after focus is restored. */
  onClose: () => void;
  /** Called only after a successful delete so the caller fires the success
   *  toast and refreshes the list. */
  onDeleted: () => void;
}

/**
 * Stylesheet colocated with the component (matched to ConfirmDialog's own
 * pattern). Injected once under a strict `style-src` CSP - the error line uses
 * the class system only, never an inline `style=`. The dialog surface, scrim,
 * focus trap, danger fill, fade, and focus restoration all belong to the
 * composed ConfirmDialog/Modal; this component only styles its own assertive
 * error line so it stays adjacent and visually tiered.
 */
const FMS_DELETE_TX_CSS = `
.fms-dtd-error{
  margin:0 0 12px; font-size:14px; font-weight:500; line-height:1.5;
  color:#B91C1C; background:#FEF2F2; border-left:3px solid #FECACA;
  border-radius:4px; padding:12px;
}
`;

const FMS_DELETE_TX_STYLE_ID = 'fms-deletetransaction-styles';

/** Inject the co-located stylesheet once; return a node only when needed. */
function styleElements(): ReactNode {
  if (typeof document === 'undefined' || document.getElementById(FMS_DELETE_TX_STYLE_ID)) {
    return null;
  }
  return (
    <style id={FMS_DELETE_TX_STYLE_ID} data-fms>
      {FMS_DELETE_TX_CSS}
    </style>
  );
}

const LOCALE = 'en-US';

/** Format the amount to plain display text via Intl (never markup). */
function amountText(t: TransactionDTO): string {
  return formatMinorUnits(t.amount.amount, t.amount.currency, LOCALE);
}

/** Format the date to plain display text via Intl (never markup). */
function dateText(t: TransactionDTO): string {
  return formatDate(t.date, LOCALE);
}

/**
 * The mandated consequence sentence. All three transaction facts (description,
 * amount, date) are backend-supplied and bound as escaped JSX text so no value
 * is ever interpreted as HTML. The amount and date are always fully visible.
 */
function consequenceFor(t: TransactionDTO): string {
  const amount = amountText(t);
  const date = dateText(t);
  return `Deleting ${t.description} (${amount}, ${date}) permanently removes it and updates the account balance and budget amounts.`;
}

/** Map a store mutation error code to a specific, solution-oriented message. */
function describeError(code: string | undefined): string {
  if (code === 'NOT_FOUND') return 'This transaction no longer exists.';
  if (code === 'RATE_LIMITED') return 'Too many attempts. Please wait a moment and try again.';
  return 'Unable to delete. Please check your connection and try again.';
}

/**
 * DeleteTransactionDialog - Module B deletion boundary (Blueprint §4.1, §5.11).
 *
 * Composes ConfirmDialog (which owns the modal surface, focus trap, initial
 * focus on Cancel, focus restoration, and the 44x44 danger button). The
 * deletion itself is enqueued through the transactions store (`applyDelete`),
 * which performs the optimistic apply and synchronous rollback against the
 * CSRF-hardened API layer. Completion is correlated via the store's
 * `isMutating` transition:
 *   - success -> call onDeleted (toast + list refresh)
 *   - conflict (409) -> close via onClose so the parent surfaces
 *     ConflictRecoveryBanner (never silently overwrite newer data)
 *   - error -> stay open, keep buttons enabled, show an assertive inline
 *     message, and allow retry
 * The confirm button is disabled + its label switches to "Deleting…" while an
 * attempt is in flight (double-submit guard + Doherty feedback).
 */
export function DeleteTransactionDialog({
  transaction,
  open,
  onClose,
  onDeleted,
}: DeleteTransactionDialogProps): ReactElement | null {
  const [submitting, setSubmitting] = useState(false);
  const [showError, setShowError] = useState<string | null>(null);

  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const onDeletedRef = useRef(onDeleted);
  onDeletedRef.current = onDeleted;
  const submittingRef = useRef(false);

  const handleConfirm = (): void => {
    if (submittingRef.current) return;
    submittingRef.current = true;
    setSubmitting(true);
    setShowError(null);
    // Optimistic delete through the store; the store rolls back on failure and
    // serializes with any other in-flight mutation.
    useTransactionsStore.getState().applyDelete(transaction.id);
  };

  const handleCancel = (): void => {
    if (submittingRef.current) return;
    setShowError(null);
    onCloseRef.current();
  };

  /* Correlate completion of this dialog's delete with the store's mutation
   * lifecycle. The store serializes mutations, and success bumps dataVersion,
   * so watching the isMutating true->false transition for our active attempt
   * is a reliable single-shot signal (and never double-deletes keeps the
   * "confirm fires exactly once" contract). */
  useEffect(() => {
    if (!open || !submitting) return;
    const baselineVersion = useTransactionsStore.getState().dataVersion;
    const unsub = useTransactionsStore.subscribe((state, prevState) => {
      if (!prevState.isMutating || state.isMutating) return;
      if (state.mutationStatus === 'success' && state.dataVersion > baselineVersion) {
        submittingRef.current = false;
        setSubmitting(false);
        setShowError(null);
        onDeletedRef.current();
      } else if (state.mutationStatus === 'conflict') {
        submittingRef.current = false;
        setSubmitting(false);
        setShowError(null);
        // Close; the parent surfaces ConflictRecoveryBanner with fresh data.
        onCloseRef.current();
      } else if (state.mutationStatus === 'error') {
        const code = state.lastError?.code;
        if (code === 'NOT_FOUND') {
          // Idempotent DELETE returned 404 - already gone; treat as deleted.
          submittingRef.current = false;
          setSubmitting(false);
          setShowError(null);
          onDeletedRef.current();
        } else {
          submittingRef.current = false;
          setSubmitting(false);
          setShowError(describeError(code));
        }
      }
    });
    return unsub;
  }, [open, submitting]);

  return (
    <>
      {styleElements()}
      {showError && (
        <p className="fms-dtd-error" role="alert" aria-live="assertive">
          {showError}
        </p>
      )}
      <ConfirmDialog
        open={open}
        title="Delete transaction?"
        consequence={consequenceFor(transaction)}
        confirmLabel={submitting ? 'Deleting…' : 'Delete transaction'}
        cancelLabel="Cancel"
        danger
        loading={submitting}
        onConfirm={handleConfirm}
        onCancel={handleCancel}
      />
    </>
  );
}

export default DeleteTransactionDialog;