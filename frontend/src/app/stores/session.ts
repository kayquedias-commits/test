/**
 * Central session/identity store (Blueprint §8.1 sessions, §5.1).
 *
 * Holds `anonymous | mfa_pending | authenticated` status, the current user/role,
 * and in-memory tokens (never localStorage — reduces the XSS theft surface).
 * Every user-facing state transition pushes a toast via the notifications store.
 *
 * @security Tokens are memory-only by design. Do NOT add persistence to
 *   localStorage/sessionStorage/IndexedDB; if persistent sessions are ever needed,
 *   the refresh token must live in an HttpOnly; Secure; SameSite=Strict cookie.
 */
import { create } from "zustand";
import type { UserRole, UserDTO } from "../../services/api/types";
import {
  login as apiLogin,
  completeMfa as apiCompleteMfa,
  logout as apiLogout,
  refresh as apiRefresh,
} from "../../services/api/endpoints";
import {
  ForbiddenError,
  RateLimitedError,
  RetryableError,
  SessionExpiredError,
  TimeoutError,
  setSessionHandlers,
} from "../../services/api/client";
import { useNotifications } from "./notifications";

const DEFAULT_TIMEOUT_MS = 30 * 60 * 1000; // Blueprint §5.1 / §8.1
const TOUCH_DEBOUNCE_MS = 150; // §8.2: debounce high-frequency input/activity events

// Pinned from .env.example; non-finite / <=0 falls back to the 30-minute default.
const CONFIGURED_TIMEOUT_MS = Number(import.meta.env.VITE_SESSION_TIMEOUT_MS ?? DEFAULT_TIMEOUT_MS);
const EFFECTIVE_TIMEOUT_MS =
  Number.isFinite(CONFIGURED_TIMEOUT_MS) && CONFIGURED_TIMEOUT_MS > 0
    ? CONFIGURED_TIMEOUT_MS
    : DEFAULT_TIMEOUT_MS;

/**
 * Decode the `exp` claim (seconds since epoch) from a JWT payload, or null if it
 * cannot be parsed. Payload is untrusted input, so every step is defensively
 * guarded; the value is never rendered, only used for the client-side countdown.
 */
function decodeJwtExpMs(token: string | null): number | null {
  if (!token) return null;
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64.padEnd(b64.length + ((4 - (b64.length % 4)) % 4), "=");
    const payload = JSON.parse(atob(padded)) as { exp?: unknown };
    return typeof payload.exp === "number" && Number.isFinite(payload.exp)
      ? payload.exp * 1000
      : null;
  } catch {
    return null;
  }
}

type SessionStatus = "anonymous" | "mfa_pending" | "authenticated";

/** Typed error surfaced to the UI; contains a solution-oriented message only. */
interface SessionError {
  code: string;
  message: string;
}

/** Session + role/permission state and actions. */
interface SessionState {
  status: SessionStatus;
  user: UserDTO | null;
  role: UserRole | null;
  /** Pending-intent context preserved while an MFA challenge is open. */
  username: string | null;
  accessToken: string | null;
  refreshToken: string | null;
  /** Epoch ms of last activity; null while anonymous. */
  lastActivityAt: number | null;
  isLoggingIn: boolean;
  isCompletingMfa: boolean;
  isRefreshing: boolean;
  isLoggingOut: boolean;
  loginError: SessionError | null;
  mfaError: SessionError | null;
  refreshError: SessionError | null;
  login: (username: string, password: string) => Promise<void>;
  completeMfa: (code: string) => Promise<void>;
  cancelMfa: () => void;
  logout: () => Promise<void>;
  touch: () => void;
  extendSession: () => Promise<void>;
  expireSession: (reason: "expired" | "api") => void;
}

/** Single-flight guard so a `touch()`/timer storm cannot fire parallel refreshes (rotation race). */
let inflightRefresh: Promise<void> | null = null;
/** Debounce timer for high-frequency `touch()` calls. */
let touchTimer: ReturnType<typeof setTimeout> | null = null;

const notifications = () => useNotifications.getState();

/**
 * Map any thrown error to a stable, user-facing code + message (never internal
 * state, stack, or correlation ids). MFA vs login failure are deliberately
 * distinct so the user can remediate in place.
 *
 * Order matters: network/transient errors are classified before auth failures so
 * a dropped connection is never reported as a wrong password.
 */
function mapError(e: unknown, ctx: "login" | "mfa" | "refresh"): SessionError {
  if (ctx === "mfa") {
    if (e instanceof RateLimitedError) {
      return { code: "MFA_RATE_LIMITED", message: "Too many attempts. Wait a moment and try again." };
    }
    return { code: "MFA_INVALID", message: "Incorrect code. Try again, or request a new code." };
  }

  const unavailable =
    ctx === "refresh"
      ? "Could not refresh your session. Check your connection."
      : "Could not sign in. Check your connection and try again.";

  if (e instanceof RetryableError || e instanceof TimeoutError) {
    return { code: "UNAVAILABLE", message: unavailable };
  }
  if (e instanceof RateLimitedError) {
    return { code: "RATE_LIMITED", message: "Too many sign-in attempts. Wait a moment and try again." };
  }
  if (e instanceof SessionExpiredError || e instanceof ForbiddenError) {
    if (ctx === "refresh") {
      return { code: "SESSION_EXPIRED", message: "Your session has expired. Please sign in again." };
    }
    return {
      code: "INVALID_CREDENTIALS",
      message: "The username or password is incorrect, or you lack access.",
    };
  }
  // Any remaining/unclassified error (including a generic ApiError) stays generic — never leak internal detail.
  return { code: "UNAVAILABLE", message: unavailable };
}

export const useSession = create<SessionState>()((set, get) => {
  /** Commit a successful auth response; clears challenge context and all errors/flags. */
  const applyAuthenticated = (session: { accessToken: string; refreshToken: string; user: UserDTO }) => {
    set({
      status: "authenticated",
      user: session.user,
      role: session.user.role,
      accessToken: session.accessToken,
      refreshToken: session.refreshToken,
      username: null,
      lastActivityAt: Date.now(),
      isLoggingIn: false,
      isCompletingMfa: false,
      isRefreshing: false,
      isLoggingOut: false,
      loginError: null,
      mfaError: null,
      refreshError: null,
    });
  };

  /** Atomically reset to a blank, anonymous session. Consumers must tolerate user === null. */
  const toAnonymous = () => {
    set({
      status: "anonymous",
      user: null,
      role: null,
      username: null,
      accessToken: null,
      refreshToken: null,
      lastActivityAt: null,
      isLoggingIn: false,
      isCompletingMfa: false,
      isRefreshing: false,
      isLoggingOut: false,
      loginError: null,
      mfaError: null,
      refreshError: null,
    });
  };

  return {
    status: "anonymous",
    user: null,
    role: null,
    username: null,
    accessToken: null,
    refreshToken: null,
    lastActivityAt: null,
    isLoggingIn: false,
    isCompletingMfa: false,
    isRefreshing: false,
    isLoggingOut: false,
    loginError: null,
    mfaError: null,
    refreshError: null,

    async login(username, password) {
      set({ isLoggingIn: true, loginError: null });
      try {
        const dto = await apiLogin({ username, password });
        if (dto.mfa_required) {
          set({ status: "mfa_pending", username, isLoggingIn: false });
          notifications().push({ severity: "info", title: "Enter your verification code" });
          return;
        }
        applyAuthenticated({
          accessToken: dto.access_token,
          refreshToken: dto.refresh_token,
          user: dto.user,
        });
        notifications().push({ severity: "success", title: "Signed in" });
      } catch (e) {
        set({ isLoggingIn: false, loginError: mapError(e, "login") });
      }
    },

    async completeMfa(code) {
      set({ isCompletingMfa: true, mfaError: null });
      try {
        const dto = await apiCompleteMfa({ mfaCode: code });
        applyAuthenticated({
          accessToken: dto.access_token,
          refreshToken: dto.refresh_token,
          user: dto.user,
        });
        notifications().push({ severity: "success", title: "Signed in" });
      } catch (e) {
        // Stay in mfa_pending so the user can retry; never flash back to anonymous.
        set({ isCompletingMfa: false, mfaError: mapError(e, "mfa") });
      }
    },

    cancelMfa() {
      toAnonymous();
    },

    async logout() {
      set({ isLoggingOut: true });
      try {
        await apiLogout();
      } catch {
        // Revocation failure is non-fatal: local identity MUST clear regardless.
      }
      toAnonymous();
      set({ isLoggingOut: false });
      notifications().push({ severity: "info", title: "Signed out" });
    },

    touch() {
      if (get().status !== "authenticated") return;
      if (touchTimer) clearTimeout(touchTimer);
      touchTimer = setTimeout(() => set({ lastActivityAt: Date.now() }), TOUCH_DEBOUNCE_MS);
    },

    async extendSession() {
      if (inflightRefresh) return inflightRefresh;
      const { status, refreshToken } = get();
      if (status !== "authenticated" || !refreshToken) return undefined;
      set({ isRefreshing: true, refreshError: null });
      inflightRefresh = (async () => {
        try {
          const dto = await apiRefresh(refreshToken);
          applyAuthenticated({
            accessToken: dto.access_token,
            refreshToken: dto.refresh_token,
            user: dto.user,
          });
          notifications().push({ severity: "success", title: "Session extended" });
        } catch (e) {
          const err = mapError(e, "refresh");
          if (err.code === "SESSION_EXPIRED") {
            toAnonymous();
            notifications().push({
              severity: "warning",
              title: "Your session has expired. Please sign in again.",
            });
          } else {
            set({ refreshError: err });
          }
        } finally {
          set({ isRefreshing: false });
          inflightRefresh = null;
        }
      })();
      return inflightRefresh;
    },

    expireSession(reason) {
      toAnonymous();
      notifications().push(
        reason === "api"
          ? { severity: "warning", title: "Your session has expired. Please sign in again." }
          : { severity: "warning", title: "Session expired" },
      );
    },
  };
});

/** Pure role/user/token selectors the UI must use — the single source for permission gating (§8.1). */
export const selectRole = (s: SessionState): UserRole | null => s.role;
export const selectUser = (s: SessionState): UserDTO | null => s.user;
/** Access token for the API client's bearer header; the only token consumers may read. */
export const selectToken = (s: SessionState): string | null => s.accessToken;
export const selectAccessToken = (s: SessionState): string | null => s.accessToken;
/** Refresh token — should only be read by the refresh action itself, never rendered. */
export const selectRefreshToken = (s: SessionState): string | null => s.refreshToken;
export const selectIsAuthenticated = (s: SessionState): boolean => s.status === "authenticated";
export const selectIsAuthenticating = (s: SessionState): boolean =>
  s.isLoggingIn || s.isCompletingMfa;
export const selectLastActivityAt = (s: SessionState): number | null => s.lastActivityAt;
export const selectError = (s: SessionState): SessionError | null =>
  s.loginError ?? s.mfaError ?? s.refreshError;

/**
 * Effective expiry: the earlier of the JWT `exp` and the inactivity window from
 * `.env`. Returns null while anonymous. Client-side countdown is UX only — the
 * 30-minute policy is enforced server-side on refresh.
 */
const deadlineMs = (s: SessionState): number | null => {
  if (s.status !== "authenticated" || s.lastActivityAt === null) return null;
  const jwtExp = decodeJwtExpMs(s.accessToken);
  const inactivityDeadline = s.lastActivityAt + EFFECTIVE_TIMEOUT_MS;
  return jwtExp === null ? inactivityDeadline : Math.min(jwtExp, inactivityDeadline);
};

// Derived, non-mutating snapshots so an expiry never flips state mid-render; the
// timeout hook consumes these to drive the countdown warning (Blueprint §8.1).
export const selectExpiresAtMs = (s: SessionState): number | null => deadlineMs(s);
export const selectIsExpired = (s: SessionState): boolean => {
  const deadline = deadlineMs(s);
  return deadline !== null && Date.now() >= deadline;
};
export const selectMsUntilExpiry = (s: SessionState): number => {
  const deadline = deadlineMs(s);
  return deadline === null ? Infinity : Math.max(0, deadline - Date.now());
};

// Wire the bearer token into the API client and clean up the store when the
// client's internal 401 flow redirects to the login screen (client assigns /login).
setSessionHandlers({
  getAccessToken: () => useSession.getState().accessToken,
  onSessionExpired: () => useSession.getState().expireSession("api"),
});