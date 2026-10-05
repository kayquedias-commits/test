import { useEffect, useRef, useState } from 'react';
import { Button } from '../../components/ui/Button';
import { createExport, getExportStatus } from '../../services/api/endpoints';
import type { ExportFormat, ExportJobDTO } from '../../services/api/types';

const MAX_POLL_MS = 5000;

type ExportStatusPanelProps = {
  jobId: string;
  format: ExportFormat;
  range: { start: string; end: string };
  fileSizeBytes?: number;
  onJobCreated?: (newJobId: string) => void;
};

/**
 * Live export job status panel (Module H). Polls getExportStatus with
 * exponential backoff (3s start, 5s cap — a 2s start would exceed the 20
 * req/min export-endpoint rate-limit budget), stops on terminal READY/FAILED,
 * and tears down timers/requests on unmount or jobId change.
 */
export function ExportStatusPanel({
  jobId,
  format,
  range,
  fileSizeBytes,
  onJobCreated,
}: ExportStatusPanelProps) {
  const [activeJobId, setActiveJobId] = useState(jobId);
  const [phase, setPhase] = useState<'CHECKING' | 'PENDING' | 'RUNNING' | 'READY' | 'FAILED'>(
    'CHECKING',
  );
  const [transient, setTransient] = useState(false);
  const [job, setJob] = useState<ExportJobDTO | null>(null);
  const [notFound, setNotFound] = useState(false);
  const [errorReason, setErrorReason] = useState<string | null>(null);
  const [retrying, setRetrying] = useState(false);
  const [retryError, setRetryError] = useState<string | null>(null);
  const [toastVisible, setToastVisible] = useState(false);

  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const toastTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const sectionRef = useRef<HTMLElement | null>(null);
  const downloadRef = useRef<HTMLAnchorElement | null>(null);

  const terminal = phase === 'READY' || phase === 'FAILED';

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    let attempt = 0;
    let stopped = false;

    const stop = () => {
      stopped = true;
    };

    // Schedule the next poll unless a terminal state was reached. The stop
    // flag is read synchronously here (not from closure React state) so a
    // READY/FAILED response halts the chain in the same tick.
    const scheduleNext = (forcedMs?: number) => {
      if (cancelled || stopped) return;
      const base = forcedMs ?? 3000 * Math.pow(2, attempt);
      const delay = Math.min(base, MAX_POLL_MS);
      attempt += 1;
      pollTimerRef.current = setTimeout(poll, delay);
    };

    async function poll() {
      if (cancelled || stopped) return;
      try {
        const data = await getExportStatus(activeJobId, controller.signal);
        if (cancelled) return;

        if (data.status === 'READY') {
          stop();
          setJob(data);
          setPhase('READY');
          setTransient(false);
          setToastVisible(true);
          toastTimerRef.current = setTimeout(() => setToastVisible(false), 5000);
          return;
        }
        if (data.status === 'FAILED') {
          stop();
          setJob(data);
          setErrorReason(data.errorReason);
          setPhase('FAILED');
          setTransient(false);
          return;
        }
        setPhase(data.status === 'PENDING' ? 'PENDING' : 'RUNNING');
        setJob(data);
        setTransient(false);
        scheduleNext();
      } catch (err) {
        if (cancelled || controller.signal.aborted) return;
        const status = (err as { status?: number }).status;
        if (status === 404) {
          // A vanished job maps to the FAILED recovery path, not a stuck
          // non-terminal spinner.
          stop();
          setNotFound(true);
          setPhase('FAILED');
          setTransient(false);
          return;
        }
        // Network/5xx/429 failures are transient, never terminal: keep
        // polling but back off. A 429 (RateLimitedError) extends the interval.
        setTransient(true);
        scheduleNext(status === 429 ? MAX_POLL_MS : undefined);
      }
    }

    // First check resolves near-immediately so the mount is never silent.
    scheduleNext(0);

    return () => {
      cancelled = true;
      stop();
      if (pollTimerRef.current) clearTimeout(pollTimerRef.current);
      if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
      controller.abort();
    };
  }, [activeJobId]);

  // Landing on the sole primary action keeps keyboard users at the attention
  // point after terminal swaps (Fitts' Law).
  useEffect(() => {
    if (phase === 'READY') {
      downloadRef.current?.focus();
    } else if (phase === 'FAILED') {
      sectionRef.current
        ?.querySelector<HTMLButtonElement>('#fms-export-retry')
        ?.focus();
    }
  }, [phase]);

  async function handleRetry() {
    setRetrying(true);
    setRetryError(null);
    try {
      const newJob = await createExport({ format, range });
      setActiveJobId(newJob.id);
      setPhase('CHECKING');
      setTransient(false);
      setNotFound(false);
      onJobCreated?.(newJob.id);
    } catch {
      setRetryError('Could not re-create the export. Please try again.');
    } finally {
      setRetrying(false);
    }
  }

  const safeDownloadUrl =
    job?.status === 'READY' && job.downloadUrl ? isSafeDownloadUrl(job.downloadUrl) : null;
  const downloadUnavailable = job?.status === 'READY' && !safeDownloadUrl;

  return (
    <section
      ref={sectionRef}
      className="fms-exp-status"
      aria-label="Export status"
    >
      <style>{FMS_EXPORT_STATUS_CSS}</style>

      <div className="fms-exp-status__body" aria-live="polite">
        {!terminal && (
          <>
            <div className="fms-exp-status__head">
              <span
                aria-hidden="true"
                className={
                  phase === 'CHECKING'
                    ? 'fms-exp-status__spinner'
                    : 'fms-exp-status__spinner fms-exp-status__spinner--quiet'
                }
              />
              <div className="fms-exp-status__title">
                {phase === 'CHECKING' && <span>Checking export status…</span>}
                {phase === 'PENDING' && <span>Preparing export…</span>}
                {phase === 'RUNNING' && <span>Exporting your data…</span>}
              </div>
            </div>

            {/* Indeterminate: no percentage value exists in the DTO, so the
                bar carries aria-valuetext and deliberately no aria-valuenow. */}
            <div
              className="fms-exp-status__track"
              role="progressbar"
              aria-label="Export progress"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuetext={phase === 'PENDING' ? 'Waiting to start' : 'Processing rows'}
            >
              <div className="fms-exp-status__track-fill" />
            </div>
          </>
        )}

        {phase === 'READY' && (
          <>
            <div className="fms-exp-status__head">
              <span aria-hidden="true" className="fms-exp-status__icon fms-exp-status__icon--success">
                {successIcon}
              </span>
              <div className="fms-exp-status__title fms-exp-status__title--success">Export ready</div>
            </div>

            {downloadUnavailable ? (
              <p className="fms-exp-status__detail fms-exp-status__detail--error">
                Download link is invalid. Please retry the export.
              </p>
            ) : (
              <a
                ref={downloadRef}
                className="fms-btn fms-btn--primary fms-btn--lg fms-exp-status__action"
                href={safeDownloadUrl ?? undefined}
                download
              >
                Download
              </a>
            )}
          </>
        )}

        {phase === 'FAILED' && (
          <div aria-live="assertive">
            <div className="fms-exp-status__head">
              <span aria-hidden="true" className="fms-exp-status__icon fms-exp-status__icon--error">
                {errorIcon}
              </span>
              <div className="fms-exp-status__title fms-exp-status__title--error">Export failed</div>
            </div>
            <p className="fms-exp-status__detail">
              {notFound
                ? 'This export no longer exists. Please start a new export.'
                : errorReason || "We couldn't generate this export. Please try again."}
            </p>
            {retryControl()}
          </div>
        )}

        {transient && !terminal && (
          <p className="fms-exp-status__transient">Connection lost — retrying…</p>
        )}

        {!terminal && fileSizeBytes != null && (
          <p className="fms-exp-status__caption">
            {formatSize(fileSizeBytes)} · {formatLabel(format)}
          </p>
        )}
      </div>

      {toastVisible && phase === 'READY' && (
        <div className="fms-exp-status__toast" role="status" aria-live="polite">
          <span aria-hidden="true" className="fms-exp-status__icon fms-exp-status__icon--success">
            {successIcon}
          </span>
          <span className="fms-exp-status__toast-text">Your export is ready to download</span>
          <button
            type="button"
            className="fms-exp-status__toast-dismiss"
            onClick={() => setToastVisible(false)}
            aria-label="Dismiss notification"
          >
            ×
          </button>
        </div>
      )}
    </section>
  );

  function retryControl() {
    return (
      <>
        <Button
          id="fms-export-retry"
          variant="secondary"
          size="lg"
          loading={retrying}
          disabled={retrying}
          className="fms-exp-status__action"
          onClick={handleRetry}
        >
          {retrying ? 'Re-creating…' : 'Retry export'}
        </Button>
        {retryError && <p className="fms-exp-status__retry-error">{retryError}</p>}
      </>
    );
  }
}

function formatLabel(format: ExportFormat): string {
  return format === 'PDF' ? 'PDF' : 'CSV';
}

// The download URL is opaque, TTL-limited, backend data and is treated as
// untrusted input: only https — plus http for localhost dev — is accepted,
// rejecting javascript:/data:/vbscript: before the value reaches href.
function isSafeDownloadUrl(url: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return null;
  }
  if (parsed.protocol === 'https:') return url;
  if (parsed.protocol === 'http:' && /^localhost$|^127\./.test(parsed.hostname)) return url;
  return null;
}

// Format megabyte/kilobyte sizes with en-US units; returns a plain string that
// React escapes at render time.
function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  if (bytes < 1024 * 1024) {
    return `${Math.round(bytes / 1024)} KB`;
  }
  const mb = bytes / (1024 * 1024);
  return `${new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 }).format(mb)} MB`;
}

const successIcon = (
  <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
    <path
      fill="currentColor"
      d="M6.5 11.5 3 8l1.4-1.4 2.1 2.1 5.1-5.1L13 5l-6.5 6.5z"
    />
  </svg>
);

const errorIcon = (
  <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
    <path
      fill="currentColor"
      d="M8 1.5 14.5 13h-13L8 1.5zM8.75 6v3.5h-1.5V6h1.5zM8 12.25a.9.9 0 1 1 0-1.8.9.9 0 0 1 0 1.8z"
    />
  </svg>
);

const FMS_EXPORT_STATUS_CSS = `
:root {
  --fms-color-success: #16a34a;
  --fms-color-error: #dc2626;
  --fms-color-primary: #4f46e5;
  --fms-color-text-primary: #1f2937;
  --fms-color-text-secondary: #525252;
  --fms-color-text-muted: #6b7280;
  --fms-color-neutral-200: #e5e7eb;
  --fms-color-surface: #ffffff;
  --fms-space-3: 12px;
  --fms-space-4: 16px;
  --fms-space-6: 24px;
  --fms-radius-lg: 12px;
  --fms-radius-full: 9999px;
  --fms-shadow-sm: 0 1px 2px rgba(0,0,0,0.05);
  --fms-focus-width: 2px;
}

.fms-exp-status {
  background: var(--fms-color-surface);
  border: 1px solid var(--fms-color-neutral-200);
  border-radius: var(--fms-radius-lg);
  box-shadow: var(--fms-shadow-sm);
  padding: var(--fms-space-4);
  display: flex;
  flex-direction: column;
  gap: var(--fms-space-4);
  min-height: 96px;
}
@media (min-width: 768px) {
  .fms-exp-status { padding: var(--fms-space-6); }
}

.fms-exp-status__body {
  display: flex;
  flex-direction: column;
  gap: var(--fms-space-3);
}

.fms-exp-status__head {
  display: flex;
  align-items: center;
  gap: var(--fms-space-3);
}
.fms-exp-status__title {
  font-size: 16px;
  font-weight: 600;
  color: var(--fms-color-text-primary);
  line-height: 1.5;
}
.fms-exp-status__title--success { color: var(--fms-color-success); }
.fms-exp-status__title--error { color: var(--fms-color-error); }

/* 12px spinner; rotate only so geometry stays stable (no layout thrash). */
.fms-exp-status__spinner {
  flex: 0 0 auto;
  width: 12px;
  height: 12px;
  border-radius: var(--fms-radius-full);
  border: 2px solid var(--fms-color-neutral-200);
  border-top-color: var(--fms-color-primary);
  animation: fms-exp-st-spin 800ms linear infinite;
}
@keyframes fms-exp-st-spin {
  to { transform: rotate(360deg); }
}

.fms-exp-status__icon--success { color: var(--fms-color-success); display: inline-flex; }
.fms-exp-status__icon--error { color: var(--fms-color-error); display: inline-flex; }

/* Fixed 8px track reserves layout so the READY/FAILED swap never shifts the
   page (CLS <= 0.1). Fill animates via transform only. */
.fms-exp-status__track {
  position: relative;
  height: 8px;
  border-radius: var(--fms-radius-full);
  background: var(--fms-color-neutral-200);
  overflow: hidden;
}
.fms-exp-status__track-fill {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  background: var(--fms-color-primary);
  transform: scaleX(0);
  transform-origin: left;
  transition: transform 200ms ease-out;
}

.fms-exp-status__detail {
  font-size: 14px;
  color: var(--fms-color-text-secondary);
  line-height: 1.5;
  margin: 0;
  display: flex;
  align-items: center;
  gap: var(--fms-space-3);
  flex-wrap: wrap;
}
.fms-exp-status__detail--error { color: var(--fms-color-error); }

.fms-exp-status__caption {
  font-size: 12px;
  color: var(--fms-color-text-muted);
  margin: 0;
}

.fms-exp-status__transient {
  font-size: 14px;
  color: var(--fms-color-text-secondary);
  margin: 0;
}

.fms-exp-status__action {
  width: 100%;
}
@media (min-width: 768px) {
  .fms-exp-status__action { width: auto; align-self: flex-end; }
}

.fms-exp-status__retry-error {
  font-size: 14px;
  color: var(--fms-color-error);
  margin: 0;
}

/* Toast enter uses opacity + translateY only (transform/opacity). */
.fms-exp-status__toast {
  display: flex;
  align-items: center;
  gap: var(--fms-space-3);
  background: var(--fms-color-surface);
  border: 1px solid var(--fms-color-neutral-200);
  border-radius: var(--fms-radius-lg);
  box-shadow: var(--fms-shadow-sm);
  padding: var(--fms-space-3) var(--fms-space-4);
  animation: fms-exp-toast-in 200ms ease-out;
}
@keyframes fms-exp-toast-in {
  from { opacity: 0; transform: translateY(8px); }
  to { opacity: 1; transform: translateY(0); }
}
.fms-exp-status__toast-text {
  font-size: 14px;
  font-weight: 500;
  color: var(--fms-color-text-primary);
  flex: 1;
}
.fms-exp-status__toast-dismiss {
  flex: 0 0 auto;
  min-width: 44px;
  min-height: 44px;
  border: none;
  background: transparent;
  color: var(--fms-color-text-muted);
  font-size: 20px;
  line-height: 1;
  cursor: pointer;
  border-radius: var(--fms-radius-sm, 4px);
}
.fms-exp-status__toast-dismiss:hover { color: var(--fms-color-text-primary); }

.fms-exp-status a:focus-visible,
.fms-exp-status button:focus-visible {
  outline: var(--fms-focus-width) solid var(--fms-color-primary);
  outline-offset: 2px;
}
`;