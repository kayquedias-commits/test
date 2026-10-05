import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';

export type ModalSize = 'sm' | 'md' | 'lg' | 'xl';

/**
 * Focusable-descendant selector scoped to the dialog subtree. Excludes
 * disabled controls, tabindex="-1" nodes, and hidden inputs so the Tab cycle
 * never lands on an element the user cannot operate.
 */
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

/** Exit fade duration, matching the fms-modal exit token (opacity-only). */
const EXIT_MS = 100;

/** Owner of the currently active focus trap (nested modals are unsupported). */
let activeTrapCleanup: (() => void) | null = null;

/**
 * Focusable descendants of `root` that are both visible and exposed to the
 * accessibility tree. `hidden`, `display:none`, `visibility:hidden` and
 * `aria-hidden="true"` elements are excluded so focus is never placed on an
 * element the user cannot see or reach.
 */
function getFocusables(root: HTMLElement): HTMLElement[] {
  return Array.from(
    root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR),
  ).filter((el) => {
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') return false;
    const style = window.getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden';
  });
}

/** True when the user has asked for reduced motion. */
function prefersReducedMotion(): boolean {
  return (
    typeof window !== 'undefined' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  );
}

export interface ModalProps {
  open: boolean;
  onClose: () => void;
  /** Id of the element (rendered by the caller inside `children`) that names the dialog. */
  labelledBy: string;
  /** Id of an element that describes the dialog content, when present. */
  descriptionId?: string;
  children: React.ReactNode;
  size?: ModalSize;
  /** When true, ESC does not close the dialog. */
  disableEsc?: boolean;
  /** When true, pressing the backdrop closes the dialog. */
  closeOnBackdrop?: boolean;
  /**
   * Resolves the mount node. Defaults to `#root`; provide only when the app
   * mounts content elsewhere. Resolved by constant reference.
   */
  appRoot?: () => Element | null;
}

/**
 * Accessible modal dialog. Portals into #root, traps Tab/Shift+Tab among its
 * own focusable children, marks background siblings inert (aria-hidden
 * fallback) while open, locks body scroll, and restores focus to the element
 * focused before open. Implements the Blueprint §8.4 modal pattern (focus
 * trap, aria-modal, focus restoration) shared by every confirm dialog and
 * multi-step workflow. It is a pure presentational shell: all content is
 * passed through `children` as auto-escaped JSX, so no internal string is ever
 * interpreted as HTML.
 *
 * Escape-hatch contract: `disableEsc` and `closeOnBackdrop` both default to
 * `false`. ESC is available by default; backdrop-close is deliberately opt-in
 * so destructive confirmations cannot be dismissed by an accidental click.
 */
export function Modal({
  open,
  onClose,
  labelledBy,
  descriptionId,
  children,
  size = 'md',
  disableEsc = false,
  closeOnBackdrop = false,
  appRoot,
}: ModalProps) {
  const overlayRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  const [closing, setClosing] = useState(false);
  const closingRef = useRef(false);

  // Single-fire close: run the exit fade, then notify the parent. When motion
  // is reduced the fade is skipped and onClose fires immediately.
  const requestClose = () => {
    if (closingRef.current) return;
    closingRef.current = true;
    setClosing(true);
    if (prefersReducedMotion()) {
      onCloseRef.current();
    } else {
      window.setTimeout(() => onCloseRef.current(), EXIT_MS);
    }
  };

  useFocusTrap({
    enabled: open && !closing,
    panelRef,
    requestClose,
    disableEsc,
  });

  // Reset the close guard whenever the dialog is reopened by the parent.
  useEffect(() => {
    if (open) {
      closingRef.current = false;
      setClosing(false);
    }
  }, [open]);

  if (!open) return null;

  const mountNode =
    (typeof document !== 'undefined' &&
      (appRoot?.() ?? document.getElementById('root'))) ||
    null;
  if (!mountNode) return null;

  return createPortal(
    <div
      ref={overlayRef}
      className="fms-modal-overlay"
      onMouseDown={(e) => {
        if (closeOnBackdrop && e.target === overlayRef.current) {
          requestClose();
        }
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelledBy}
        aria-describedby={descriptionId}
        tabIndex={-1}
        className={`fms-modal-panel fms-modal-panel--${size}`}
      >
        <button
          type="button"
          className="fms-modal-close"
          aria-label="Close dialog"
          onClick={requestClose}
        >
          <svg
            aria-hidden="true"
            width="20"
            height="20"
            viewBox="0 0 20 20"
            focusable="false"
          >
            <path
              d="M5 5l10 10M15 5L5 15"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.8"
              strokeLinecap="round"
            />
          </svg>
        </button>
        <div className="fms-modal-content">{children}</div>
      </div>
    </div>,
    mountNode,
  );
}

interface UseFocusTrapOptions {
  enabled: boolean;
  panelRef: React.RefObject<HTMLDivElement | null>;
  requestClose: () => void;
  disableEsc: boolean;
}

/**
 * Drives the modal focus lifecycle. On enable it stores the focused trigger,
 * moves focus to the first focusable element (the panel when there are none,
 * so focus never escapes to browser chrome), makes sibling content inert /
 * aria-hidden, locks body scroll, and attaches ESC/Tab/focusin handlers. On
 * disable or unmount it detaches those handlers, restores every sibling and
 * body value it touched, and returns focus to the stored trigger after the
 * exit fade (or immediately under reduced motion). Only one trap is active at
 * a time; opening a second modal releases the first.
 */
function useFocusTrap({
  enabled,
  panelRef,
  requestClose,
  disableEsc,
}: UseFocusTrapOptions) {
  const requestCloseRef = useRef(requestClose);
  requestCloseRef.current = requestClose;
  const disableEscRef = useRef(disableEsc);
  disableEscRef.current = disableEsc;

  useEffect(() => {
    if (!enabled) return undefined;

    const panel = panelRef.current;
    if (!panel) return undefined;

    const trigger =
      (typeof document !== 'undefined' &&
        (document.activeElement as HTMLElement | null)) ||
      null;

    // Release any previously active trap so a new modal owns the cycle.
    activeTrapCleanup?.();

    const focusables = getFocusables(panel);
    (focusables[0] ?? panel).focus({ preventScroll: true });

    const portalParent = panel.parentElement as HTMLElement | null;
    const priorSiblingState: Array<{
      node: HTMLElement;
      inert: boolean;
      ariaHidden: string | null;
    }> = [];
    if (portalParent) {
      for (const child of Array.from(portalParent.children)) {
        if (child === panel) continue;
        const el = child as HTMLElement;
        priorSiblingState.push({
          node: el,
          inert: 'inert' in el ? el.inert : false,
          ariaHidden: el.getAttribute('aria-hidden'),
        });
        if ('inert' in el) {
          el.inert = true;
        } else {
          el.setAttribute('aria-hidden', 'true');
        }
      }
    }

    const body = typeof document !== 'undefined' ? document.body : null;
    const priorOverflow = body ? body.style.overflow : '';
    const priorPaddingRight = body ? body.style.paddingRight : '';
    const scrollbarWidth =
      body && typeof window !== 'undefined'
        ? window.innerWidth - body.clientWidth
        : 0;
    if (body) {
      body.style.overflow = 'hidden';
      if (scrollbarWidth > 0) body.style.paddingRight = `${scrollbarWidth}px`;
    }

    const focusinHandler = (e: FocusEvent) => {
      const dialogEl = panelRef.current;
      if (!dialogEl || dialogEl.contains(e.target as Node)) return;
      const list = getFocusables(dialogEl);
      (list[0] ?? dialogEl).focus();
    };

    const keydown = (e: KeyboardEvent) => {
      const dialogEl = panelRef.current;
      if (!dialogEl) return;

      if (e.key === 'Escape') {
        // Only honor ESC when focus is still inside the dialog; otherwise let
        // the event fall through rather than closing on a leaked target.
        if (disableEscRef.current) return;
        if (!dialogEl.contains(e.target as Node)) return;
        e.preventDefault();
        e.stopPropagation();
        requestCloseRef.current();
        return;
      }

      if (e.key === 'Tab') {
        const list = getFocusables(dialogEl);
        const active = document.activeElement;
        if (list.length === 0) {
          e.preventDefault();
          dialogEl.focus();
          return;
        }
        const first = list[0];
        const last = list[list.length - 1];
        if (e.shiftKey && (active === first || !dialogEl.contains(active))) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && (active === last || !dialogEl.contains(active))) {
          e.preventDefault();
          first.focus();
        }
      }
    };
    document.addEventListener('keydown', keydown);
    document.addEventListener('focusin', focusinHandler);

    const cleanup = () => {
      document.removeEventListener('keydown', keydown);
      document.removeEventListener('focusin', focusinHandler);
      for (const s of priorSiblingState) {
        if ('inert' in s.node) {
          (s.node as HTMLElement).inert = s.inert;
        } else {
          if (s.ariaHidden === null) s.node.removeAttribute('aria-hidden');
          else s.node.setAttribute('aria-hidden', s.ariaHidden);
        }
      }
      if (body) {
        body.style.overflow = priorOverflow;
        body.style.paddingRight = priorPaddingRight;
      }
      const restoreFocus = () => {
        if (trigger && trigger.isConnected) trigger.focus();
      };
      if (prefersReducedMotion()) {
        restoreFocus();
      } else {
        window.setTimeout(restoreFocus, EXIT_MS);
      }
    };
    activeTrapCleanup = cleanup;
    return () => {
      if (activeTrapCleanup === cleanup) activeTrapCleanup = null;
      cleanup();
    };
  }, [enabled, panelRef]);
}