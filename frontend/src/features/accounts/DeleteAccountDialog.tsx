import {
  useEffect,
  useId,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
} from 'react';
import { ConfirmDialog } from '../../components/ui/ConfirmDialog';
import { Field } from '../../components/ui/Field';
import {
  deleteAccount,
  deleteTransaction,
  listAccounts,
  listTransactions,
  updateTransaction,
} from '../../services/api/endpoints';
import type { AccountDTO, TransactionDTO } from '../../services/api/types';

/**
 * DeleteAccountDialog — Module A deletion boundary.
 *
 * Guards account deletion behind a quantified consequence. When the account has
 * no transactions it renders the standard ConfirmDialog. When transactions exist
 * the user must either move every transaction to another account first
 * (reassign-and-delete) or explicitly acknowledge an irreversible cascade
 * (delete-and-cascade). Both paths are orchestrated client-side with the
 * existing endpoints (the API contract has no server-side ?cascade=true flag or
 * dedicated reassign endpoint); the server's authoritative conflict boundary
 * (409 on accounts with transactions) remains the defense-in-depth fallback.
 */

export interface DeleteAccountDialogProps {
  /** The account being deleted. */
  account: AccountDTO;
  /** Whether the dialog is mounted. */
  open: boolean;
  /** Close without deleting. */
  onCancel: () => void;
  /** Called after a successful delete so the caller can invalidate
   *  account/transaction queries. */
  onDeleted: () => void;
}

const FMS_DELETE_ACCOUNT_CSS = `
.fms-dda-overlay{position:fixed;inset:0;z-index:60;display:flex;align-items:center;justify-content:center;padding:16px;background:rgba(15,23,42,0.5)}
.fms-dda-card{width:min(calc(100vw - 32px),480px);max-height:calc(100vh - 32px);overflow:auto;background:#FFFFFF;border-radius:12px;box-shadow:0 10px 15px -3px rgba(0,0,0,0.1),0 4px 6px -2px rgba(0,0,0,0.05);padding:24px;opacity:1}
.fms-dda-title{font-size:18px;line-height:1.25;font-weight:600;color:#111827;margin:0 0 8px}
.fms-dda-desc{font-size:14px;line-height:1.5;color:#4B5563;max-width:65ch;margin:0 0 24px;font-weight:400}
.fms-dda-count{color:#111827;font-weight:600}
.fms-dda-legend{font-size:14px;font-weight:600;color:#111827;padding:0;margin:0 0 12px}
.fms-dda-fieldset{border:0;padding:0;margin:0;min-inline-size:0}
.fms-dda-option{display:flex;gap:12px;align-items:flex-start;justify-content:flex-start;width:100%;background:#FFFFFF;border:1px solid #E5E7EB;border-radius:8px;padding:16px;margin:0;text-align:left;cursor:pointer;accent-color:#2563EB;transition:border-color 200ms ease-out,background-color 200ms ease-out}
.fms-dda-option:focus-visible{outline:2px solid #2563EB;outline-offset:2px;box-shadow:0 0 0 3px rgba(37,99,235,0.4)}
.fms-dda-option[aria-checked="true"]{border-color:#2563EB}
.fms-dda-option-cascade[aria-checked="true"]{border-color:#DC2626}
.fms-dda-radio{width:20px;height:20px;flex:none;margin:2px 0 0}
.fms-dda-option-label{display:block;font-size:14px;font-weight:600;color:#111827;line-height:1.4}
.fms-dda-option-sub{display:block;font-size:12px;color:#6B7280;line-height:1.5;margin-top:2px}
.fms-dda-spacer{height:16px;flex:none}
.fms-dda-reveal{opacity:0;transform:translateY(4px);transition:opacity 150ms ease-out,transform 150ms ease-out}
.fms-dda-reveal[data-open="true"]{opacity:1;transform:translateY(0)}
.fms-dda-step{font-size:12px;color:#6B7280;font-weight:500;margin:8px 0 0}
.fms-dda-warning{margin:16px 0 0;background:#FEF2F2;border-left:3px solid #FECACA;border-radius:4px;padding:12px;display:flex;gap:10px;align-items:flex-start}
.fms-dda-warning-icon{flex:none;width:20px;height:20px;color:#B91C1C;margin:1px 0 0}
.fms-dda-warning-text{font-size:14px;font-weight:500;color:#991B1B;line-height:1.5;margin:0}
.fms-dda-ack{margin:16px 0 0;display:flex;gap:10px;align-items:flex-start;font-size:14px;color:#111827;line-height:1.5;cursor:pointer}
.fms-dda-checkbox{width:20px;height:20px;flex:none;margin:0;accent-color:#B91C1C}
.fms-dda-error-msg{font-size:14px;font-weight:500;color:#B91C1C;line-height:1.5;margin:16px 0 0}
.fms-dda-actions{display:flex;justify-content:flex-end;gap:12px;margin:24px 0 0}
.fms-dda-banner{display:flex;gap:12px;align-items:flex-start;background:#FFFBEB;border-left:3px solid #FDE68A;border-radius:4px;padding:12px;margin:0 0 24px}
.fms-dda-banner-icon{flex:none;width:20px;height:20px;color:#92400E;margin:1px 0 0}
.fms-dda-banner-text{font-size:14px;color:#92400E;line-height:1.5;margin:0}
.fms-dda-banner-actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:12px}
.fms-dda-banner-link{min-height:44px;background:none;border:0;padding:0 4px;font-size:14px;font-weight:600;color:#92400E;text-decoration:underline;cursor:pointer}
.fms-dda-banner-link:focus-visible{outline:2px solid #92400E;outline-offset:2px}
.fms-dda-skeleton{height:14px;width:240px;border-radius:4px;background:#E5E7EB}
.fms-dda-skeleton-2{height:14px;width:320px;border-radius:4px;background:#F3F4F6;margin-top:8px}
.fms-dda-sr{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}
@media (max-width:767px){
  .fms-dda-actions{flex-direction:column}
  .fms-dda-actions button{width:100%}
}
@media (prefers-reduced-motion: reduce){
  .fms-dda-option,.fms-dda-reveal{transition:none}
}
`;

const FMS_DELETE_ACCOUNT_STYLE_ID = 'fms-deleteaccount-styles';

function styleElements(): ReactNode {
  if (typeof document === 'undefined' || document.getElementById(FMS_DELETE_ACCOUNT_STYLE_ID)) {
    return null;
  }
  return (
    <style id={FMS_DELETE_ACCOUNT_STYLE_ID} data-fms>
      {FMS_DELETE_ACCOUNT_CSS}
    </style>
  );
}

/** Generate a fresh idempotency key so write retries never double-post. */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `dda-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

interface ErrorLike {
  name?: string;
  message?: string;
}

/** Map a thrown value to a stable, solution-oriented user message. */
function describeError(err: unknown): string {
  const e = err as ErrorLike | null;
  const name = e?.name ?? '';
  if (name === 'ConflictError') return 'This account was modified by another user.';
  if (name === 'ForbiddenError') return "You don't have permission to delete this account.";
  if (name === 'NotFoundError') return 'This account no longer exists.';
  return 'Something went wrong. Please try again.';
}

export function DeleteAccountDialog({
  account,
  open,
  onCancel,
  onDeleted,
}: DeleteAccountDialogProps): ReactElement | null {
  const [count, setCount] = useState<number | null>(null);
  const [transactions, setTransactions] = useState<TransactionDTO[]>([]);
  const [otherAccounts, setOtherAccounts] = useState<AccountDTO[]>([]);
  const [loadState, setLoadState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [retryNonce, setRetryNonce] = useState(0);
  const [mode, setMode] = useState<'reassign' | 'cascade' | null>(null);
  const [targetId, setTargetId] = useState<string>('');
  const [acknowledged, setAcknowledged] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [reassignError, setReassignError] = useState<string | null>(null);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);

  const countId = useId();
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const restoreFocusRef = useRef<Element | null>(null);

  const onDeletedRef = useRef(onDeleted);
  onDeletedRef.current = onDeleted;
  const onCancelRef = useRef(onCancel);
  onCancelRef.current = onCancel;

  /* Load transaction count + reassign pool while open. */
  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    let cancelled = false;
    setLoadState('loading');
    setCount(null);
    setTransactions([]);
    setOtherAccounts([]);
    setMode(null);
    setTargetId('');
    setAcknowledged(false);
    setReassignError(null);
    setSubmitError(null);
    setConflict(false);

    async function load(): Promise<void> {
      try {
        const [accountsPage, txPage] = await Promise.all([
          listAccounts({ pageSize: 100 }, controller.signal),
          listTransactions({ filters: { accountId: account.id } }, controller.signal),
        ]);
        if (cancelled) return;
        const others = (accountsPage.items ?? []).filter((a) => a.id !== account.id);
        const txItems = [...(txPage.items ?? [])];
        let cursor = txPage.next_cursor;
        let safety = 0;
        while (cursor && safety < 1000) {
          safety += 1;
          const more = await listTransactions(
            { filters: { accountId: account.id }, cursor },
            controller.signal,
          );
          if (cancelled) return;
          txItems.push(...(more.items ?? []));
          cursor = more.next_cursor;
        }
        if (cancelled) return;
        setOtherAccounts(others);
        setTransactions(txItems);
        setCount(txItems.length);
        setLoadState('ready');
      } catch (err) {
        if (cancelled || (err as ErrorLike | null)?.name === 'AbortError') return;
        setLoadState('error');
      }
    }
    void load();

    restoreFocusRef.current = document.activeElement;
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [open, account.id, retryNonce]);

  /* Focus trap + ESC for the branching/loading/error modal. */
  useEffect(() => {
    if (!open || loadState !== 'ready' || !count || count <= 0) return;
    const node = dialogRef.current;
    if (!node) return;
    const focusable = (): HTMLElement[] =>
      Array.from(
        node.querySelectorAll<HTMLElement>('input, select, button'),
      ).filter((el) => !el.hasAttribute('disabled'));
    const first = focusable()[0];
    first?.focus();

    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onCancelRef.current();
        return;
      }
      if (e.key !== 'Tab') return;
      const els = focusable();
      if (els.length === 0) return;
      const firstEl = els[0];
      const lastEl = els[els.length - 1];
      if (e.shiftKey) {
        if (document.activeElement === firstEl) {
          e.preventDefault();
          lastEl.focus();
        }
      } else if (document.activeElement === lastEl) {
        e.preventDefault();
        firstEl.focus();
      }
    };
    node.addEventListener('keydown', onKey);
    return () => node.removeEventListener('keydown', onKey);
  }, [open, loadState, count, mode, conflict]);

  /* Return focus to the trigger on unmount. */
  useEffect(() => {
    return () => {
      if (restoreFocusRef.current instanceof HTMLElement) {
        restoreFocusRef.current.focus();
      }
    };
  }, []);

  /* Shared mutation runner: maps 409 to the recovery banner, others to a
   * stable message. */
  const runDelete = async (work: () => Promise<void>): Promise<void> => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      await work();
      onDeletedRef.current();
    } catch (err) {
      if ((err as ErrorLike | null)?.name === 'ConflictError') {
        setConflict(true);
      } else if ((err as ErrorLike | null)?.name !== 'AbortError') {
        setSubmitError(describeError(err));
      }
    } finally {
      setSubmitting(false);
    }
  };

  const handleCascade = async (): Promise<void> => {
    await runDelete(async () => {
      const key = newIdempotencyKey();
      for (const tx of transactions) {
        await deleteTransaction(tx.id, { idempotencyKey: `${key}-tx-${tx.id}` });
      }
      await deleteAccount(account.id, { idempotencyKey: `${key}-acct` });
    });
  };

  const handleReassign = async (): Promise<void> => {
    if (!targetId) {
      setReassignError('Select an account to receive the transactions.');
      return;
    }
    await runDelete(async () => {
      const key = newIdempotencyKey();
      for (const tx of transactions) {
        await updateTransaction(tx.id, { accountId: targetId }, tx.version, {
          idempotencyKey: `${key}-tx-${tx.id}`,
        });
      }
      await deleteAccount(account.id, { idempotencyKey: `${key}-acct` });
    });
  };

  if (!open) return null;

  if (loadState === 'loading') {
    return (
      <div ref={dialogRef} className="fms-dda-overlay" role="presentation" aria-busy="true">
        {styleElements()}
        <div className="fms-dda-card" role="dialog" aria-modal="true">
          <h2 className="fms-dda-title">Delete account</h2>
          <div aria-hidden="true">
            <div className="fms-dda-skeleton" />
            <div className="fms-dda-skeleton-2" />
          </div>
          <span className="fms-dda-sr" role="status">
            Loading account details
          </span>
          <div className="fms-dda-actions">
            <button type="button" className="fms-btn fms-btn--secondary" onClick={onCancel}>
              Cancel
            </button>
          </div>
        </div>
      </div>
    );
  }

  if (loadState === 'error') {
    return (
      <div ref={dialogRef} className="fms-dda-overlay" role="presentation">
        {styleElements()}
        <div className="fms-dda-card" role="dialog" aria-modal="true">
          <h2 className="fms-dda-title" id={`${countId}-title`}>Delete account</h2>
          <p className="fms-dda-desc" role="alert">
            We could not check this account&apos;s activity. Retrying will confirm how many
            transactions would be affected before you can delete.
          </p>
          <div className="fms-dda-actions">
            <button type="button" className="fms-btn fms-btn--secondary" onClick={onCancel}>
              Cancel
            </button>
            <button
              type="button"
              className="fms-btn fms-btn--secondary"
              onClick={() => setRetryNonce((n) => n + 1)}
            >
              Retry
            </button>
          </div>
        </div>
      </div>
    );
  }

  /* Zero-transaction path: standard ConfirmDialog (own modal + focus mgmt). */
  if (count === 0) {
    return (
      <ConfirmDialog
        open={open}
        title="Delete account"
        consequence="This account has no recorded transactions. Deleting it is permanent and cannot be undone."
        confirmLabel="Delete account"
        danger
        loading={submitting}
        onConfirm={() =>
          void runDelete(async () => {
            await deleteAccount(account.id, { idempotencyKey: newIdempotencyKey() });
          })
        }
        onCancel={onCancel}
      />
    );
  }

  /* With-transactions path: explicit reassign-or-cascade before delete. */
  const canDelete =
    (mode === 'reassign' && targetId !== '') || (mode === 'cascade' && acknowledged);

  const confirmAction = (): void => {
    if (mode === 'reassign') void handleReassign();
    else if (mode === 'cascade') void handleCascade();
  };

  return (
    <div ref={dialogRef} className="fms-dda-overlay" role="presentation">
      {styleElements()}
      <div
        className="fms-dda-card"
        role="dialog"
        aria-modal="true"
        aria-labelledby={`${countId}-title`}
      >
        <h2 id={`${countId}-title`} className="fms-dda-title">Delete account</h2>
        <p className="fms-dda-desc">
          This will permanently delete the account and{' '}
          <strong className="fms-dda-count">
            {count} transaction{count === 1 ? '' : 's'}
          </strong>{' '}
          recorded in it. This cannot be undone.
        </p>

        <fieldset className="fms-dda-fieldset">
          <legend className="fms-dda-legend">How do you want to delete this account?</legend>

          {otherAccounts.length > 0 && (
            <>
              <label className="fms-dda-option" aria-checked={mode === 'reassign'}>
                <input
                  type="radio"
                  name={`${countId}-mode`}
                  className="fms-dda-radio"
                  checked={mode === 'reassign'}
                  onChange={() => {
                    setMode('reassign');
                    setReassignError(null);
                    setSubmitError(null);
                  }}
                />
                <span>
                  <span className="fms-dda-option-label">Reassign transactions first</span>
                  <span className="fms-dda-option-sub">
                    Move all recorded transactions to another account, then delete.
                  </span>
                </span>
              </label>

              <div className="fms-dda-reveal" data-open={mode === 'reassign'}>
                <Field
                  id={`${countId}-target-field`}
                  label="Move transactions to"
                  required
                  error={reassignError}
                  hint="The account being deleted is excluded from this list."
                >
                  <select
                    value={targetId}
                    disabled={mode !== 'reassign'}
                    onChange={(e) => {
                      setTargetId(e.target.value);
                      setReassignError(null);
                      setSubmitError(null);
                    }}
                  >
                    <option value="">Select an account…</option>
                    {otherAccounts.map((a) => (
                      <option key={a.id} value={a.id}>
                        {a.name}
                      </option>
                    ))}
                  </select>
                </Field>
                <p className="fms-dda-step">
                  Step 1 · Move transactions → Step 2 · Delete account
                </p>
              </div>

              <div className="fms-dda-spacer" />
            </>
          )}

          <label
            className="fms-dda-option fms-dda-option-cascade"
            aria-checked={mode === 'cascade'}
          >
            <input
              type="radio"
              name={`${countId}-mode`}
              className="fms-dda-radio"
              checked={mode === 'cascade'}
              onChange={() => {
                setMode('cascade');
                setReassignError(null);
                setSubmitError(null);
              }}
            />
            <span>
              <span className="fms-dda-option-label">
                Delete the account and all its transactions
              </span>
              <span className="fms-dda-option-sub">Irreversible permanent deletion.</span>
            </span>
          </label>

          <div className="fms-dda-reveal" data-open={mode === 'cascade'}>
            <div className="fms-dda-warning" role="note">
              <span className="fms-dda-warning-icon" aria-hidden="true">⚠</span>
              <p className="fms-dda-warning-text">
                This will permanently delete the account and all {count} transaction
                {count === 1 ? '' : 's'} recorded in it. This cannot be undone.
              </p>
            </div>
            <label className="fms-dda-ack">
              <input
                type="checkbox"
                className="fms-dda-checkbox"
                checked={acknowledged}
                onChange={(e) => {
                  setAcknowledged(e.target.checked);
                  setSubmitError(null);
                }}
              />
              <span>I understand this is permanent and cannot be undone.</span>
            </label>
          </div>
        </fieldset>

        {submitError && (
          <p className="fms-dda-error-msg" role="alert">
            {submitError}
          </p>
        )}

        <div className="fms-dda-actions">
          <button
            type="button"
            className="fms-btn fms-btn--secondary"
            onClick={onCancel}
            disabled={submitting}
          >
            Cancel
          </button>
          <button
            type="button"
            className="fms-btn fms-btn--danger"
            disabled={!canDelete || submitting}
            onClick={confirmAction}
          >
            {submitting ? 'Deleting…' : 'Delete account'}
            {submitting && <span className="fms-dda-sr">Working</span>}
          </button>
        </div>
      </div>
    </div>
  );
}

export default DeleteAccountDialog;