import {
  useEffect,
  useId,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
} from 'react';
import { Button } from '../../components/ui/Button';
import { Field } from '../../components/ui/Field';
import { Modal } from '../../components/ui/Modal';
import {
  listCategories,
  listTransactions,
  mergeCategories,
} from '../../services/api/endpoints';
import type { CategoryDTO } from '../../services/api/types';

/**
 * MergeCategoryDialog — Module C merge boundary.
 *
 * In-use categories cannot be deleted; merging into a same-type top-level
 * category is the supported path and propagates historically (moves every
 * transaction, updates budgets/reports). Because the operation is irreversible
 * and history-spanning, consent is explicit: a quantified consequence summary,
 * then a second confirmation dialog that restates the exact target and count.
 * The caller (`onMerged`) owns cache invalidation and the success toast, and
 * must gate the trigger behind the `category:write` permission (view-only roles
 * render no affordance).
 */

export interface MergeCategoryDialogProps {
  /** The in-use category being merged. */
  source: CategoryDTO;
  /** Whether the dialog is open/mounted. */
  open: boolean;
  /** Close without merging. */
  onClose: () => void;
  /** Called after a successful merge so the caller can invalidate
   *  category/transaction/budget queries and show the success toast. */
  onMerged: () => void;
}

const FMS_MERGE_CATEGORY_CSS = `
.fms-mcd-summary{background:#F9FAFB;border:1px solid #F3F4F6;border-radius:8px;padding:16px;font-size:14px;line-height:1.5;color:#374151;font-weight:400;margin:0 0 16px;max-width:65ch}
.fms-mcd-summary strong{color:#B91C1C;font-weight:600}
.fms-mcd-banner{display:flex;gap:12px;align-items:flex-start;background:#FEF2F2;border:1px solid #FECACA;border-radius:8px;padding:12px;margin:16px 0 0}
.fms-mcd-banner-icon{flex:none;width:20px;height:20px;color:#B91C1C;margin:1px 0 0}
.fms-mcd-banner-text{font-size:14px;color:#991B1B;line-height:1.5;margin:0}
.fms-mcd-banner-text strong{font-weight:600}
.fms-mcd-banner-actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:8px}
.fms-mcd-skeleton{height:44px;border-radius:8px;background:#E5E7EB;margin-bottom:8px}
.fms-mcd-skeleton-2{height:14px;width:300px;border-radius:4px;background:#F3F4F6}
.fms-mcd-critical{font-size:14px;line-height:1.5;color:#374151;margin:0 0 16px;max-width:65ch}
.fms-mcd-critical strong{color:#B91C1C;font-weight:600}
.fms-mcd-actions{display:flex;justify-content:flex-end;gap:12px;margin-top:24px}
.fms-mcd-sr{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}
@media (max-width:767px){
  .fms-mcd-actions{flex-direction:column}
  .fms-mcd-actions button{width:100%}
}
@media (prefers-reduced-motion: reduce){
  .fms-mcd-actions{transition:none}
}
`;

const FMS_MERGE_CATEGORY_STYLE_ID = 'fms-mergecategory-styles';

function styleElements(): ReactNode {
  if (typeof document === 'undefined' || document.getElementById(FMS_MERGE_CATEGORY_STYLE_ID)) {
    return null;
  }
  return (
    <style id={FMS_MERGE_CATEGORY_STYLE_ID} data-fms>
      {FMS_MERGE_CATEGORY_CSS}
    </style>
  );
}

/** Generate a fresh idempotency key so a retried merge can never fire twice. */
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `mcd-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

interface ErrorLike {
  name?: string;
  message?: string;
}

/** Map a thrown value to a stable, solution-oriented user message. */
function describeError(err: unknown): string {
  const e = err as ErrorLike | null;
  const name = e?.name ?? '';
  if (name === 'ConflictError') return 'This category was modified by another user.';
  if (name === 'ForbiddenError') return "You don't have permission to merge categories.";
  if (name === 'NotFoundError') return 'The source or target category no longer exists.';
  return 'Something went wrong. Please try again.';
}

/** Count transactions under a category by cursor-walking the page stream. */
async function countTransactions(categoryId: string, signal: AbortSignal): Promise<number> {
  let total = 0;
  let cursor: string | undefined;
  let safety = 0;
  do {
    const page = await listTransactions(
      { filters: { categoryId }, cursor, limit: 100 },
      signal,
    );
    total += page.items.length;
    cursor = page.next_cursor ?? undefined;
    safety += 1;
  } while (cursor && safety < 2000);
  return total;
}

interface TargetOption {
  category: CategoryDTO;
  count: number;
}

export function MergeCategoryDialog({
  source,
  open,
  onClose,
  onMerged,
}: MergeCategoryDialogProps): ReactElement | null {
  const [targets, setTargets] = useState<TargetOption[]>([]);
  const [loadState, setLoadState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [retryNonce, setRetryNonce] = useState(0);
  const [targetId, setTargetId] = useState<string>('');
  const [sourceCount, setSourceCount] = useState<number | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [sourceStale, setSourceStale] = useState(false);

  const titleId = useId();
  const confirmTitleId = useId();
  const descId = useId();
  const fieldId = useId();

  const onClosedRef = useRef(onClose);
  onClosedRef.current = onClose;
  const onMergedRef = useRef(onMerged);
  onMergedRef.current = onMerged;

  /* Load valid target options (with counts) + the source count while open. */
  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    let cancelled = false;
    setLoadState('loading');
    setTargets([]);
    setTargetId('');
    setSourceCount(null);
    setConfirming(false);
    setConflict(false);
    setErrorMsg(null);
    setSourceStale(false);

    async function load(): Promise<void> {
      try {
        const page = await listCategories({ pageSize: 100 }, controller.signal);
        if (cancelled) return;

        // Source-stale detection: if the source no longer exists, surface the
        // recovery state instead of attempting a doomed merge.
        const present = (page.items ?? []).some((c) => c.id === source.id);
        if (!present) {
          setSourceStale(true);
          setLoadState('ready');
          return;
        }

        // Valid targets: same type, top-level (parentId null), excluding source.
        const candidates = (page.items ?? []).filter(
          (c) => c.id !== source.id && c.type === source.type && c.parentId === null,
        );

        const withCounts = await Promise.all(
          candidates.map(async (c): Promise<TargetOption> => {
            const count = await countTransactions(c.id, controller.signal);
            return { category: c, count };
          }),
        );
        if (cancelled) return;

        const srcCount = await countTransactions(source.id, controller.signal);
        if (cancelled) return;

        setTargets(withCounts);
        setSourceCount(srcCount);
        setLoadState('ready');
      } catch (err) {
        if (cancelled || (err as ErrorLike | null)?.name === 'AbortError') return;
        setLoadState('error');
      }
    }
    void load();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [open, source.id, retryNonce]);

  /* Close is suppressed while a merge is in flight (cannot abort the op). */
  const handleRequestClose = (): void => {
    if (submitting) return;
    onClosedRef.current();
  };

  /* Inner confirmation close returns to the outer step (Back / Keep source). */
  const handleConfirmRequestClose = (): void => {
    if (submitting) return;
    setConfirming(false);
  };

  const selected = targets.find((t) => t.category.id === targetId) ?? null;
  const canConfirm =
    loadState === 'ready' &&
    targetId !== '' &&
    !submitting &&
    !conflict &&
    !sourceStale &&
    targets.length > 0;

  const runMerge = async (): Promise<void> => {
    if (submitting) return;
    setSubmitting(true);
    setErrorMsg(null);
    try {
      await mergeCategories(source.id, targetId, { idempotencyKey: newIdempotencyKey() });
      onMergedRef.current();
    } catch (err) {
      const name = (err as ErrorLike | null)?.name;
      // Return to the outer step so the 409 banner / inline error renders in
      // context instead of leaving the user inside the inner confirm dialog.
      setConfirming(false);
      if (name === 'ConflictError') {
        setConflict(true);
      } else if (name !== 'AbortError') {
        setErrorMsg(describeError(err));
      }
    } finally {
      setSubmitting(false);
    }
  };

  if (!open) return null;

  if (loadState === 'loading') {
    return (
      <Modal
        open={open}
        onClose={handleRequestClose}
        labelledBy={titleId}
        disableEsc={submitting}
        closeOnBackdrop={false}
      >
        {styleElements()}
        <h2 id={titleId} className="fms-mcd-critical">
          Merge category
        </h2>
        <div aria-hidden="true" role="presentation">
          <div className="fms-mcd-skeleton" />
          <div className="fms-mcd-skeleton-2" />
        </div>
        <span className="fms-mcd-sr" role="status">
          Loading merge targets
        </span>
        <div className="fms-mcd-actions">
          <Button variant="secondary" onClick={handleRequestClose} disabled={submitting}>
            Cancel
          </Button>
        </div>
      </Modal>
    );
  }

  if (loadState === 'error') {
    return (
      <Modal
        open={open}
        onClose={handleRequestClose}
        labelledBy={titleId}
        closeOnBackdrop={false}
      >
        {styleElements()}
        <h2 id={titleId} className="fms-mcd-critical">
          Merge category
        </h2>
        <p className="fms-mcd-critical">
          We could not load the merge targets. Retrying will confirm which categories are
          available before you can merge.
        </p>
        <div className="fms-mcd-actions">
          <Button variant="secondary" onClick={handleRequestClose}>
            Cancel
          </Button>
          <Button variant="secondary" onClick={() => setRetryNonce((n) => n + 1)}>
            Retry
          </Button>
        </div>
      </Modal>
    );
  }

  /* Outer step: consequence summary + target select + optional banners. */
  const outer = (
    <Modal
      open={open && !confirming}
      onClose={handleRequestClose}
      labelledBy={titleId}
      descriptionId={descId}
      closeOnBackdrop={false}
    >
      <h2 id={titleId} className="fms-mcd-critical">
        Merge category
      </h2>

      {sourceStale ? (
        <div className="fms-mcd-banner" role="alert">
          <span className="fms-mcd-banner-icon" aria-hidden="true">
            ⚠
          </span>
          <div>
            <p className="fms-mcd-banner-text">
              <strong>{source.name}</strong> no longer exists. Reload to see the latest
              categories.
            </p>
            <div className="fms-mcd-banner-actions">
              <Button variant="secondary" onClick={handleRequestClose}>
                Close
              </Button>
            </div>
          </div>
        </div>
      ) : (
        <>
          <p id={descId} className="fms-mcd-summary">
            Merging <strong>{source.name}</strong> into{' '}
            <strong>{selected ? selected.category.name : '…'}</strong> will move{' '}
            <strong>
              {sourceCount !== null
                ? `${sourceCount} transaction${sourceCount === 1 ? '' : 's'}`
                : 'all transactions'}
            </strong>{' '}
            and update budgets and reports. This cannot be undone.
          </p>

          <Field
            id={`${fieldId}-target`}
            label="Merge into target category"
            required
            hint={
              targets.length === 0
                ? 'No valid merge targets remain. Create a new top-level or same-type category first.'
                : 'Top-level categories of the same type, excluding the source.'
            }
            error={errorMsg}
          >
            <select
              value={targetId}
              onChange={(e) => {
                setTargetId(e.target.value);
                setErrorMsg(null);
              }}
              disabled={submitting}
            >
              <option value="">
                {targets.length === 0 ? 'No valid targets' : 'Select a category…'}
              </option>
              {targets.map((t) => (
                <option key={t.category.id} value={t.category.id}>
                  {t.category.name} · {t.count} transaction{t.count === 1 ? '' : 's'}
                </option>
              ))}
            </select>
          </Field>

          {conflict && (
            <div className="fms-mcd-banner" role="alert">
              <span className="fms-mcd-banner-icon" aria-hidden="true">
                ⚠
              </span>
              <div>
                <p className="fms-mcd-banner-text">
                  <strong>This category was modified by another user.</strong>
                </p>
                <div className="fms-mcd-banner-actions">
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setConflict(false);
                      setRetryNonce((n) => n + 1);
                    }}
                  >
                    Refresh &amp; retry
                  </Button>
                </div>
              </div>
            </div>
          )}

          <div className="fms-mcd-actions">
            <Button variant="secondary" onClick={handleRequestClose} disabled={submitting}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={!canConfirm}
              onClick={() => setConfirming(true)}
            >
              Merge &amp; move transactions
            </Button>
          </div>
        </>
      )}
    </Modal>
  );

  /* Final step: explicit consent restating target + source count. */
  const inner = (
    <Modal
      open={open && confirming}
      onClose={handleConfirmRequestClose}
      labelledBy={confirmTitleId}
      closeOnBackdrop={false}
    >
      <h2 id={confirmTitleId} className="fms-mcd-critical">
        Confirm merge
      </h2>
      <p className="fms-mcd-critical">
        Merge <strong>{source.name}</strong> into <strong>{selected?.category.name}</strong> —{' '}
        <strong>
          {sourceCount !== null
            ? `${sourceCount} transaction${sourceCount === 1 ? '' : 's'}`
            : 'all transactions'}
        </strong>{' '}
        will be moved and budgets and reports updated. This cannot be undone.
      </p>
      <div className="fms-mcd-actions">
        <Button
          variant="secondary"
          onClick={() => setConfirming(false)}
          disabled={submitting}
        >
          Back
        </Button>
        <Button variant="danger" loading={submitting} onClick={() => void runMerge()}>
          Merge &amp; move transactions
        </Button>
      </div>
    </Modal>
  );

  return (
    <>
      {styleElements()}
      {outer}
      {inner}
    </>
  );
}

export default MergeCategoryDialog;