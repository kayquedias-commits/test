import { useEffect, useRef, useState } from 'react';

/** Default debounce floor (ms) for search/filter inputs, per Blueprint §8.2. */
const DEBOUNCE_DEFAULT_MS = 150;

/** A structured, solution-oriented error value safe to render in the UI. */
export interface DebouncedQueryError {
  code: string;
  message: string;
}

/** Result of a debounced async query, exposing data, pending and error states. */
export interface DebouncedQueryResult<T> {
  data: T | null;
  pending: boolean;
  error: DebouncedQueryError | null;
}

/** True when the thrown value is an AbortError, i.e. a normal supersede, not a failure. */
function isAbortError(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError';
}

/**
 * Returns a trailing-edge debounced copy of `value`, updating `delay` ms after the
 * final change. The timer lives in a stable ref so unrelated re-renders never reset it.
 */
export function useDebounce<T>(value: T, delay = DEBOUNCE_DEFAULT_MS): T {
  const [debounced, setDebounced] = useState<T>(value);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
    }
    timerRef.current = setTimeout(() => setDebounced(value), delay);
    return () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [value, delay]);

  return debounced;
}

/**
 * Runs `fn` after the value in `deps` settles for `delay` ms, aborting any in-flight
 * request before the next call so the last query always wins. AbortError is swallowed
 * as a routine supersede; genuine errors are surfaced via `error`. On unmount the
 * pending timer is cleared and the in-flight request aborted. `pending` reflects the
 * debounce-and-fetch window and returns to `false` once the settled result resolves.
 */
export function useDebouncedQuery<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  deps: readonly unknown[],
  delay = DEBOUNCE_DEFAULT_MS,
): DebouncedQueryResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [pending, setPending] = useState(true);
  const [error, setError] = useState<DebouncedQueryError | null>(null);

  // Always invoke the latest callback so a stale closure never fires an old request.
  const fnRef = useRef(fn);
  fnRef.current = fn;

  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    // Abort the prior request before launching the next so results cannot arrive out of order.
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
    }
    abortRef.current?.abort();
    setError(null);
    setPending(true);

    timerRef.current = setTimeout(() => {
      const controller = new AbortController();
      abortRef.current = controller;
      fnRef.current(controller.signal).then(
        (result) => {
          if (!controller.signal.aborted) {
            setData(result);
            setPending(false);
          }
        },
        (err: unknown) => {
          if (isAbortError(err) || controller.signal.aborted) {
            return;
          }
          const message =
            err instanceof Error && err.message ? err.message : 'The request could not be completed.';
          setError({ code: 'QUERY_FAILED', message });
          setPending(false);
        },
      );
    }, delay);

    return () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      abortRef.current?.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, pending, error };
}