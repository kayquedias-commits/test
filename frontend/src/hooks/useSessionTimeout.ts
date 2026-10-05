/**
 * useSessionTimeout
 *
 * 30-minute inactivity countdown + "Continue session" hook (Blueprint §8.1 / §9.1).
 * Drives a ≤60s warning window before server-side session expiry, offers an extend
 * escape hatch, and routes to /login?reason=session-timeout on expiry. This is a
 * UX-only mirror of the server policy; the authoritative 30-minute enforcement
 * lives server-side on refresh, so this hook never grants access on its own.
 *
 * The deadline is derived from the session store (`selectExpiresAtMs`: the earlier
 * of the JWT `exp` and `lastActivityAt + VITE_SESSION_TIMEOUT_MS`), so the client
 * warning always tracks the authoritative server sliding window without re-reading
 * the env var or replicating the store's timer math.
 */
import { useEffect, useRef, useState } from "react";
import { useSession, selectExpiresAtMs } from "../app/stores/session";

/** 60-second live countdown window before the session expires. */
const WARNING_WINDOW_MS = 60_000;
/** Static reason — never constructed from env or user input (open-redirect safe). */
const REDIRECT_REASON = "session-timeout";
/** Relative login route; static so the reason is always truthful and un-injectable. */
const TIMEOUT_REASON_URL = `/login?reason=${REDIRECT_REASON}`;

const RETRY_MESSAGE = "Could not renew your session. Check your connection and try again.";
const EXPIRY_MESSAGE =
  "Your session has expired due to inactivity. You have been signed out. Please sign in again.";
const WARNING_MESSAGE = "Your session will expire soon.";
const EXTENDED_MESSAGE = "Session extended.";

/** Tracks a live region the module created so it can be removed on unmount. */
let selfCreatedRegion: HTMLElement | null = null;

/**
 * Publish an announcement to a visually-hidden live region (`role=status` polite,
 * `role=alert` assertive). The region is created on first use so a consumer can
 * announce before its own container renders. Text is set via `textContent` only —
 * never innerHTML — so screen-reader output can never carry executable markup.
 */
function announce(message: string, assertive: boolean): void {
  let node = document.querySelector<HTMLElement>("[data-session-live-region]");
  if (!node) {
    node = document.createElement("div");
    node.dataset.sessionLiveRegion = "true";
    node.style.position = "absolute";
    node.style.width = "1px";
    node.style.height = "1px";
    node.style.overflow = "hidden";
    node.style.clip = "rect(0 0 0 0)";
    node.style.whiteSpace = "nowrap";
    document.body.appendChild(node);
    selfCreatedRegion = node;
  }
  node.setAttribute("role", assertive ? "alert" : "status");
  node.setAttribute("aria-live", assertive ? "assertive" : "polite");
  node.setAttribute("aria-atomic", "true");
  node.textContent = message;
}

/** True when a deadline value is actionable (non-null, finite). */
function hasDeadline(value: number | null): value is number {
  return value !== null && Number.isFinite(value);
}

export interface SessionTimeout {
  /** Milliseconds until expiry (Infinity while anonymous); a consumer formats this as MM:SS. */
  remainingMs: number;
  /** True while within the 60-second warning window (and before any successful extend). */
  showWarning: boolean;
  /** True while an extend (token refresh) call is in flight — drives a button-level loading state. */
  isExtending: boolean;
  /**
   * Refresh the token and re-arm the countdown. Returns `{ ok, expired }` so the
   * consumer can render the right feedback: a transient failure must not trigger a
   * logout, while an auth failure falls through to the normal expiry path.
   */
  extend: () => Promise<{ ok: boolean; expired: boolean }>;
  /** Normal, non-timeout sign-out — no reason query param (login message stays accurate). */
  logout: () => void;
}

/**
 * Session inactivity countdown hook. Subscribes to the session store's computed
 * expiry deadline and re-arms a one-shot warning timer plus a 1s countdown interval
 * only once the warning window is reached (CPU-friendly). User activity resets the
 * sliding window through the store's `touch()` (which already debounces ≥150ms).
 */
export function useSessionTimeout(): SessionTimeout {
  const expiresAtMs = useSession(selectExpiresAtMs);
  const storeExtend = useSession((s) => s.extendSession);
  const storeLogout = useSession((s) => s.logout);

  const [remainingMs, setRemainingMs] = useState<number>(Infinity);
  const [showWarning, setShowWarning] = useState(false);
  const [isExtending, setIsExtending] = useState(false);

  /** Warns-once-per-window guard; reset whenever a fresh/sliding deadline re-arms a warning. */
  const warnedRef = useRef(false);
  /** Expiry fires exactly once; prevents duplicate redirects and duplicate assertive announcements. */
  const expiredHandledRef = useRef(false);
  /** Latest deadline mirrored for timer closures so they never read stale values. */
  const expiryRef = useRef<number | null>(null);

  const warningTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const expiryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const countdownIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  /** Stop every timer/interval the hook owns. Safe to call repeatedly. */
  const clearTimers = () => {
    if (warningTimerRef.current !== null) {
      clearTimeout(warningTimerRef.current);
      warningTimerRef.current = null;
    }
    if (expiryTimerRef.current !== null) {
      clearTimeout(expiryTimerRef.current);
      expiryTimerRef.current = null;
    }
    if (countdownIntervalRef.current !== null) {
      clearInterval(countdownIntervalRef.current);
      countdownIntervalRef.current = null;
    }
  };

  /** The one shared expiry path: announce assertively, then logout + redirect. */
  const handleExpiry = () => {
    if (expiredHandledRef.current) return;
    expiredHandledRef.current = true;
    clearTimers();
    setShowWarning(false);
    setRemainingMs(0);
    // Assertive announcement fires BEFORE the redirect so it is not cut off by
    // navigation; the login page renders + the reason message is announced there.
    announce(EXPIRY_MESSAGE, true);
    void storeLogout().finally(() => {
      window.location.assign(TIMEOUT_REASON_URL);
    });
  };

  /** 1s live countdown during the warning window; fires expiry when it reaches zero. */
  const startOneSecondCountdown = () => {
    if (countdownIntervalRef.current !== null) return;
    const tick = () => {
      const deadline = expiryRef.current;
      if (!hasDeadline(deadline)) {
        clearTimers();
        return;
      }
      const remaining = Math.max(0, deadline - Date.now());
      setRemainingMs(remaining);
      if (remaining <= 0) handleExpiry();
    };
    tick();
    countdownIntervalRef.current = setInterval(tick, 1000);
  };

  /**
   * Schedule the one-shot warning timer and the 1s countdown from a deadline. A
   * fresh/sliding deadline resets the warn-once-per-window guard so every new
   * inactivity cycle produces a warning — never a silent expiry after one extend.
   */
  const armTimers = (deadline: number | null) => {
    clearTimers();
    if (!hasDeadline(deadline)) {
      setShowWarning(false);
      setRemainingMs(Infinity);
      warnedRef.current = false;
      expiredHandledRef.current = false;
      return;
    }
    expiryRef.current = deadline;
    const remaining = Math.max(0, deadline - Date.now());

    if (remaining > WARNING_WINDOW_MS) {
      // Outside the warning window: hide it, reset the guard, arm both timers.
      setShowWarning(false);
      warnedRef.current = false;
      setRemainingMs(remaining);
      warningTimerRef.current = setTimeout(
        () => {
          setShowWarning(true);
          warnedRef.current = true;
          announce(WARNING_MESSAGE, false);
          startOneSecondCountdown();
        },
        remaining - WARNING_WINDOW_MS,
      );
      expiryTimerRef.current = setTimeout(handleExpiry, remaining);
      return;
    }

    if (remaining <= 0) {
      handleExpiry();
      return;
    }

    // Already inside the warning window (background-tab return, or resuming after a
    // transient refresh failure): show it and start the countdown immediately.
    setShowWarning(true);
    setRemainingMs(remaining);
    if (!warnedRef.current) {
      warnedRef.current = true;
      announce(WARNING_MESSAGE, false);
    }
    startOneSecondCountdown();
    expiryTimerRef.current = setTimeout(handleExpiry, remaining);
  };

  const extend = async (): Promise<{ ok: boolean; expired: boolean }> => {
    if (isExtending) return { ok: false, expired: false };
    // Optimistic pause: the countdown visibly stops and Continue enters a loading
    // state synchronously (Doherty's <100ms feedback) before the async call lands.
    clearTimers();
    setShowWarning(false);
    setIsExtending(true);

    await storeExtend(); // resolves always (never rejects, per the store contract)

    const s = useSession.getState();
    if (s.status !== "authenticated") {
      // Token genuinely invalid/expired — the store already reset identity. Fall
      // through to the normal expiry path (no second logout needed).
      setIsExtending(false);
      handleExpiry();
      return { ok: false, expired: true };
    }
    setIsExtending(false);

    if (s.refreshError !== null) {
      // Transient network failure: do NOT logout. Resume the countdown from the
      // unchanged deadline so the user can retry Continue.
      armTimers(expiryRef.current);
      announce(RETRY_MESSAGE, false);
      return { ok: false, expired: false };
    }

    // Success: the store reset `lastActivityAt`, so `expiresAtMs` moves later and
    // the effect below re-arms the timers for the fresh sliding window.
    announce(EXTENDED_MESSAGE, false);
    return { ok: true, expired: false };
  };

  // Re-arm timer scheduling whenever the deadline moves (fresh login or extend).
  useEffect(() => {
    armTimers(expiresAtMs);
    return clearTimers;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expiresAtMs]);

  // Activity + visibility listeners (mount once). Activity resets the sliding
  // window via the store's debounced `touch()`; tab-return re-syncs so a stale
  // background countdown can never outlive the real expiry.
  useEffect(() => {
    const { touch } = useSession.getState();
    const onActivity = () => touch();
    const onVisible = () => {
      if (document.visibilityState !== "visible") return;
      const deadline = expiryRef.current;
      if (deadline === null) return;
      const remaining = Math.max(0, deadline - Date.now());
      if (remaining <= 0) {
        handleExpiry();
      } else if (remaining > WARNING_WINDOW_MS) {
        setShowWarning(false);
        warnedRef.current = false;
      }
    };

    window.addEventListener("pointerdown", onActivity);
    window.addEventListener("keydown", onActivity);
    window.addEventListener("mousemove", onActivity);
    window.addEventListener("scroll", onActivity, { passive: true });
    window.addEventListener("focus", onVisible);
    document.addEventListener("visibilitychange", onVisible);

    return () => {
      window.removeEventListener("pointerdown", onActivity);
      window.removeEventListener("keydown", onActivity);
      window.removeEventListener("mousemove", onActivity);
      window.removeEventListener("scroll", onActivity);
      window.removeEventListener("focus", onVisible);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, []);

  // Tear down owned timers and any module-created live region on unmount.
  useEffect(() => {
    return () => {
      clearTimers();
      if (selfCreatedRegion) {
        selfCreatedRegion.remove();
        selfCreatedRegion = null;
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const logout = (): void => {
    clearTimers();
    void storeLogout();
  };

  return { remainingMs, showWarning, isExtending, extend, logout };
}