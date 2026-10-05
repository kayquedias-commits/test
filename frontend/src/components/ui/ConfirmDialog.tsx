import { useEffect, useId, useRef } from 'react';

import { Button } from './Button';
import { Modal } from './Modal';

export interface ConfirmDialogProps {
  open: boolean;
  title: string;
  /** Explicit consequence of the operation, stated in the caller's language. */
  consequence: string;
  confirmLabel?: string;
  cancelLabel?: string;
  onConfirm: () => void;
  onCancel: () => void;
  loading?: boolean;
  /** When false the confirm appears in the primary color for non-destructive confirms. */
  danger?: boolean;
}

/**
 * Stylesheet colocated with the component, matched to Button's pattern.
 * Injected once (no per-element inline `style=`), so the layout, danger fill,
 * and fade use only the class system and hold under a strict `style-src` CSP.
 */
const FMS_CONFIRM_CSS = `
.fms-confirm{ padding:20px 16px; }
.fms-confirm__title{
  margin:0 20px 12px 0; font-size:18px; line-height:1.25;
  font-weight:600; color:#111827;
}
.fms-confirm__description{
  margin:0 0 24px; font-size:16px; line-height:1.5; color:#4b5563;
  max-width:65ch;
}
.fms-confirm__actions{ display:flex; flex-direction:column; gap:12px; }
.fms-confirm__actions .fms-btn{ width:100%; }
.fms-confirm__fade{ animation:fms-confirm-fade 200ms ease-out; }
.fms-modal-content .fms-confirm__fade{ animation-fill-mode:backwards; }
@media (min-width:768px){
  .fms-confirm__actions{ flex-direction:row; justify-content:flex-end; }
  .fms-confirm__actions .fms-btn{ width:auto; }
}
@keyframes fms-confirm-fade{ from{ opacity:0; transform:translateY(8px); } to{ opacity:1; transform:none; } }
@media (prefers-reduced-motion: reduce){
  .fms-confirm__fade{ animation:none; }
}
`;

if (typeof document !== 'undefined') {
  const STYLE_ID = 'fms-confirmdialog-styles';
  if (!document.getElementById(STYLE_ID)) {
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = FMS_CONFIRM_CSS;
    document.head.appendChild(el);
  }
}

/**
 * Final human gate before an irreversible operation. Built over Modal + Button:
 * it states the explicit consequence (aria-describedby), keeps the danger
 * confirm as the sole saturated control (Von Restorff) while Cancel stays
 * neutral and default-first, and blocks double-submit with both a local lock
 * and the parent-driven `loading` state. Focus is restored to the original
 * trigger by Modal; initial focus lands on Cancel, never on the destructive
 * action. All props are rendered as auto-escaped JSX text children — no string
 * is ever interpreted as HTML.
 */
export function ConfirmDialog({
  open,
  title,
  consequence,
  confirmLabel = 'Delete',
  cancelLabel = 'Cancel',
  onConfirm,
  onCancel,
  loading = false,
  danger = true,
}: ConfirmDialogProps) {
  const titleId = useId();
  const descId = useId();
  const cancelId = useId();
  const confirmId = useId();

  const onCancelRef = useRef(onCancel);
  onCancelRef.current = onCancel;
  const onConfirmRef = useRef(onConfirm);
  onConfirmRef.current = onConfirm;
  const loadingRef = useRef(loading);
  loadingRef.current = loading;

  const submittedRef = useRef(false);

  useEffect(() => {
    if (process.env.NODE_ENV !== 'development') return;
    if (consequence.trim() === '') {
      console.warn(
        '[ConfirmDialog] A destructive confirmation must state its consequence explicitly.',
      );
    }
  }, [consequence]);

  // Modal auto-focuses its own non-destructive close button, which for this
  // dialog is a cancel-equivalent. Re-focus Cancel so an incidental Enter or
  // Space can never trigger the destructive action (runs after Modal's trap).
  useEffect(() => {
    if (!open) return;
    const cancel = document.getElementById(cancelId);
    cancel?.focus({ preventScroll: true });
  }, [open, cancelId]);

  // Arm the local double-submit lock for the next open; guard an in-flight
  // dismissal while the parent `loading` flag is propagating.
  useEffect(() => {
    if (open) submittedRef.current = false;
  }, [open]);

  const handleCancel = () => {
    if (loadingRef.current) return;
    onCancelRef.current();
  };

  const handleConfirm = () => {
    if (submittedRef.current) return;
    if (loadingRef.current) return;
    submittedRef.current = true;
    onConfirmRef.current();
  };

  return (
    <Modal
      open={open}
      onClose={handleCancel}
      labelledBy={titleId}
      descriptionId={descId}
      size="sm"
      disableEsc={loading}
      closeOnBackdrop={false}
    >
      <div className="fms-confirm fms-confirm__fade">
        <h2 id={titleId} className="fms-confirm__title">
          {title}
        </h2>
        <p id={descId} className="fms-confirm__description">
          {consequence}
        </p>
        <div className="fms-confirm__actions">
          <Button
            id={cancelId}
            variant="secondary"
            disabled={loading}
            onClick={handleCancel}
            ariaLabel={cancelLabel}
          >
            {cancelLabel}
          </Button>
          <Button
            id={confirmId}
            variant={danger ? 'danger' : 'primary'}
            loading={loading}
            disabled={loading}
            onClick={handleConfirm}
            ariaLabel={confirmLabel}
          >
            {confirmLabel}
          </Button>
        </div>
      </div>
    </Modal>
  );
}

export default ConfirmDialog;