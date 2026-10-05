import { create } from 'zustand';

/**
 * Toast/alert state for the aria-live messaging channel (Blueprint §8.3).
 * No DOM here: Toast.tsx subscribes and renders one `aria-live="polite"`
 * region (info/success/warning) and one `aria-live="assertive"` region
 * (submit-blocking errors) so announcements queue correctly. Each item keeps
 * `severity` (the single source for color + icon + text — never color alone),
 * `live` (the ARIA channel), and `ttlMs` (errors dwell longer). Bodies come
 * exclusively from the typed error mapping in client.ts — no stack traces.
 */

export type Severity = 'info' | 'success' | 'warning' | 'error';
export type LiveChannel = 'polite' | 'assertive';

/** Caller-supplied shape. `live` is derived from `severity`, never accepted. */
export interface NotificationInput {
  severity: Severity;
  title: string;
  body?: string;
  dismissible?: boolean;
  ttlMs?: number;
}

export interface Notification {
  id: string;
  severity: Severity;
  title: string;
  body?: string;
  live: LiveChannel;
  dismissible: boolean;
  ttlMs: number;
}

// -- Tunables (contract for the state) --------------------------------------

const DEFAULT_TTL_MS = 6000;
const ERROR_TTL_MS = 10000;
const MAX_VISIBLE_PER_CHANNEL = 5;
const MAX_TITLE_LENGTH = 255;
const MAX_BODY_LENGTH = 1000;
const READING_CHARS_PER_SEC = 25;
const READ_BUFFER_MS = 1000;

/** Lower severity may be evicted to make room for higher; never the reverse. */
const SEVERITY_WEIGHT: Record<Severity, number> = {
  info: 0,
  success: 1,
  warning: 2,
  error: 3,
};

/**
 * Hard text-only gate at the push boundary: rejects non-strings and strips
 * any markup characters so body/title can never reach a text node as HTML.
 * Truncates to `maxLen` to cap oversized payloads.
 */
function sanitizeText(raw: unknown, maxLen: number): string {
  if (typeof raw !== 'string') return '';
  const stripped = raw.replace(/</g, '').replace(/>/g, '').trim();
  return stripped.slice(0, maxLen);
}

const SENSITIVE_PATTERNS = [
  /\bauthorization\b/gi,
  /\bbearer\s+\S+/gi,
  /\bapi[_-]?key\b/gi,
  /\bpassword\b/gi,
  /\bsecret\b/gi,
  /\btoken\b/gi,
];

const STACK_TRACE_PATTERNS = [/\n\s+at\s+\S+/g, /\bError:\s*\S+:\d+:\d+/g];

/**
 * Drops common secret labels and stack-trace markers before a message is
 * stored, so a screenshotted/shared view never leaks credentials or traces.
 */
function redactSensitive(text: string): string {
  let cleaned = text;
  for (const pattern of STACK_TRACE_PATTERNS) cleaned = cleaned.replace(pattern, '');
  for (const pattern of SENSITIVE_PATTERNS) cleaned = cleaned.replace(pattern, '[REDACTED]');
  return cleaned;
}

/** error -> assertive; every other severity is hard-coded polite. */
function channelFor(severity: Severity): LiveChannel {
  return severity === 'error' ? 'assertive' : 'polite';
}

/**
 * TTL is the floor set by severity, raised to fit the message's reading time
 * (~25 chars/sec); caller may raise it further but never below the floor.
 */
function ttlFor(severity: Severity, title: string, body: string, requested?: number): number {
  const floor = severity === 'error' ? ERROR_TTL_MS : DEFAULT_TTL_MS;
  const longest = Math.max(title.length, body.length);
  const readTime = Math.ceil(longest / READING_CHARS_PER_SEC) * 1000 + READ_BUFFER_MS;
  const byContent = Math.max(floor, readTime);
  return requested != null && Number.isFinite(requested) && requested > floor ? requested : byContent;
}

// -- Store ------------------------------------------------------------------

export interface NotificationsState {
  notifications: Notification[];
  push: (input: NotificationInput) => void;
  dismiss: (id: string) => void;
  clear: () => void;
  pause: (id: string) => void;
  resume: (id: string) => void;
}

type TimerEntry = { timeout: ReturnType<typeof setTimeout>; expiresAt: number };

// Out-of-store timer ledger so TTLs can pause/resume/dismiss without racing
// React renders. Not part of state — subscribers must not observe timer churn.
const timers = new Map<string, TimerEntry>();

let idCounter = 0;
const nextId = (): string => `toast-${++idCounter}`;

function dismissTimer(id: string): void {
  const entry = timers.get(id);
  if (entry) {
    clearTimeout(entry.timeout);
    timers.delete(id);
  }
}

function scheduleAutoDismiss(id: string, ttlMs: number): void {
  dismissTimer(id);
  const expiresAt = Date.now() + ttlMs;
  const timeout = setTimeout(() => {
    timers.delete(id);
    useNotifications.getState().dismiss(id);
  }, ttlMs);
  timers.set(id, { timeout, expiresAt });
}

export const useNotifications = create<NotificationsState>((set, get) => ({
  notifications: [],

  push: (input) => {
    if (
      !input ||
      (input.severity !== 'info' &&
        input.severity !== 'success' &&
        input.severity !== 'warning' &&
        input.severity !== 'error')
    ) {
      return;
    }

    // This store only produces finite-TTL toasts, each requiring the WCAG
    // 2.2.1 "disable" escape hatch — a non-dismissible input is rejected.
    if (input.dismissible === false) return;

    const title = redactSensitive(sanitizeText(input.title, MAX_TITLE_LENGTH));
    const body = redactSensitive(sanitizeText(input.body, MAX_BODY_LENGTH));
    if (!title) return;

    const live = channelFor(input.severity);
    const ttlMs = ttlFor(input.severity, title, body, input.ttlMs);

    const state = get().notifications;
    const sameChannel = state.filter((item) => item.live === live);

    // Dedup by severity+title+body: replace in place, reset TTL, never stack.
    const key = `${input.severity}|${title}|${body}`;
    const existing = sameChannel.find(
      (item) => `${item.severity}|${item.title}|${item.body ?? ''}` === key,
    );
    if (existing) {
      const merged = { ...existing, ttlMs };
      set({ notifications: state.map((item) => (item.id === existing.id ? merged : item)) });
      scheduleAutoDismiss(existing.id, ttlMs);
      return;
    }

    const notification: Notification = {
      id: nextId(),
      severity: input.severity,
      title,
      body: body || undefined,
      live,
      dismissible: true,
      ttlMs,
    };

    // Assertive pushes preempt: placed at the front of the assertive channel,
    // never gated behind polite items. Polite stays FIFO within its channel.
    const channel = live === 'assertive'
      ? [notification, ...sameChannel]
      : [...sameChannel, notification];

    const evicted = evictOverflow(channel, notification.severity);
    const nonEvicted = channel.filter((item) => !evicted.includes(item.id));

    set({ notifications: [...state.filter((item) => item.live !== live), ...nonEvicted] });
    scheduleAutoDismiss(notification.id, ttlMs);
  },

  dismiss: (id) => {
    dismissTimer(id);
    const next = get().notifications.filter((item) => item.id !== id);
    if (next.length !== get().notifications.length) set({ notifications: next });
  },

  clear: () => {
    timers.forEach((entry) => clearTimeout(entry.timeout));
    timers.clear();
    set({ notifications: [] });
  },

  pause: (id) => {
    const entry = timers.get(id);
    if (entry) {
      clearTimeout(entry.timeout);
      timers.set(id, { timeout: entry.timeout, expiresAt: entry.expiresAt });
    }
  },

  resume: (id) => {
    const entry = timers.get(id);
    if (!entry) return;
    const remaining = entry.expiresAt - Date.now();
    dismissTimer(id);
    if (remaining <= 0) {
      get().dismiss(id);
    } else {
      scheduleAutoDismiss(id, remaining);
    }
  },
}));

/**
 * Enforces the per-channel capacity (max 5) with priority-aware eviction:
 * a new item may displace an older item of equal-or-lower severity, but never
 * evicts a higher-severity (unread critical) item to make room for a lower one.
 */
function evictOverflow(channel: Notification[], incoming: Severity): Notification[] {
  if (channel.length <= MAX_VISIBLE_PER_CHANNEL) return [];
  const weight = SEVERITY_WEIGHT[incoming];
  const displaceable = channel.filter((item) => SEVERITY_WEIGHT[item.severity] <= weight);
  const victim = displaceable.length > 0 ? displaceable[0] : channel[0];
  return [victim];
}

/** Stable shared empty array so empty-state identity never leaks. */
const EMPTY: Notification[] = [];

type Memo = { ref: Notification[] | null; slice: Notification[] };

function memoizedChannel(live: LiveChannel, all: Notification[]): Notification[] {
  return all.filter((item) => item.live === live);
}

// --- Selectors (memoized; stable identity until state reference changes) ---

let politeMemo: Memo = { ref: null, slice: EMPTY };
let assertiveMemo: Memo = { ref: null, slice: EMPTY };
let totalMemo: Memo = { ref: null, slice: EMPTY };

/**
 * Stable, memoized per-channel selectors. Each returns the same array
 * identity until `state.notifications` changes, preserving insertion order so
 * the screen-reader narrative matches the visual stack and the assertive
 * region never emits duplicate same-toast announcements.
 */
export function selectVisiblePolite(all: Notification[]): Notification[] {
  if (politeMemo.ref !== all) {
    politeMemo = { ref: all, slice: memoizedChannel('polite', all) };
  }
  return politeMemo.slice;
}

export function selectVisibleAssertive(all: Notification[]): Notification[] {
  if (assertiveMemo.ref !== all) {
    assertiveMemo = { ref: all, slice: memoizedChannel('assertive', all) };
  }
  return assertiveMemo.slice;
}

export function selectVisibleNotifications(all: Notification[]): Notification[] {
  if (totalMemo.ref !== all) {
    totalMemo = {
      ref: all,
      slice: all.length === 0 ? EMPTY : [...all],
    };
  }
  return totalMemo.slice;
}

/** Convenience for subscribing a single selector off the store state. */
export const selectPolite = (state: NotificationsState): Notification[] =>
  selectVisiblePolite(state.notifications);
export const selectAssertive = (state: NotificationsState): Notification[] =>
  selectVisibleAssertive(state.notifications);
export const selectVisible = (state: NotificationsState): Notification[] =>
  selectVisibleNotifications(state.notifications);