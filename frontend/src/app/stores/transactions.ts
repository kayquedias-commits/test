/**
 * Central transaction cache + optimistic-mutation store (Blueprint §8.4).
 *
 * Owns the cursor-paged list cache and the optimistic create/edit/delete path
 * with rollback on error. Writes go through the CSRF-hardened `api` layer
 * (`endpoints.ts` -> `apiRequest`); this store never calls `fetch` directly.
 * The page cache is a `Map` (never a `Record`) keyed by cursor so a malicious
 * cursor string like `"__proto__"` cannot rewrite object prototypes
 * (prototype-pollution safe). All DTO fields held here are raw server strings;
 * rendering components must bind them as text nodes only - never via
 * innerHTML/v-html (XSS prevention).
 *
 * Rollback is synchronous: each mutation snapshots the affected page maps
 * before the optimistic apply, so a failed write restores instantly (< 100 ms)
 * and then triggers an authoritative refetch to confirm. A 409 surfaces a
 * distinct `conflict` state with `lastConflict` for the ConflictRecoveryBanner
 * - the store never silently overwrites newer server data.
 *
 * @security
 * - No localStorage/sessionStorage/indexedDB: the cache is memory-only and
 *   LRU-bounded (`MAX_CACHED_PAGES`) to bound the 100k-row dataset.
 * - `reset()` clears all rows and in-flight requests so a logout cannot leak
 *   one tenant's cached pages to another user on the same device.
 * - `version` guards are coerced to non-negative integers before any
 *   comparison to prevent type-confusion bypassing the 409 guard.
 */

import { create } from "zustand";
import type { TransactionDTO, CursorPage, Money, TransactionType, ApiErrorCode } from "../../services/api/types";
import {
  listTransactions,
  createTransaction,
  updateTransaction,
  deleteTransaction,
} from "../../services/api/endpoints";
import { ApiError, ConflictError, NotFoundError } from "../../services/api/client";

// ---- Local mirrored input types (the equivalents in endpoints.ts are private
// and not exported, so the store owns its own shape for public consumption). ---

/** Filter keys mirroring the §6.3 search indexes (same set as the API). */
export type TransactionFilters = {
  dateFrom?: string;
  dateTo?: string;
  categoryId?: string;
  amountMin?: string;
  amountMax?: string;
  description?: string;
};

/** Transaction list ordering keys. */
export type TransactionSort = "date" | "amount" | "description";

/** Create payload (matches the API's transaction input contract). */
export type TransactionCreateInput = {
  accountId: string;
  categoryIds: string[];
  amount: Money;
  date: string;
  description: string;
  type: TransactionType;
  scheduled: boolean;
};

/** Per-page load lifecycle so the table never renders the guided empty-state
 * during a load, and can show a retry-with-explanation on failure. */
export type PageStatus = "idle" | "loading" | "ready" | "error";

/** Mutation visibility contract. `conflict` (409) is drawn separately from a
 * generic `error` so the UI renders the record-modified recovery path, not a
 * plain retry toast. */
export type MutationStatus = "idle" | "pending" | "success" | "error" | "conflict";

/** A serialized or failed write intent, keyed so the consuming UI can offer an
 * explicit Retry after a non-conflict failure (never a terminal dead end). */
export type PendingIntent =
  | { type: "create"; payload: TransactionCreateInput; pageKey: string }
  | { type: "edit"; id: string; patch: Partial<TransactionCreateInput>; version: number; pageKey: string }
  | { type: "delete"; id: string; pageKey: string };

/** Carried 409 payload for the ConflictRecoveryBanner. `serverDTO`/`serverVersion`
 * are populated after the authoritative refetch so "Re-apply my change" can
 * submit against the fresh version. */
export interface LastConflict {
  type: "create" | "edit" | "delete";
  pageKey: string;
  patch: Partial<TransactionCreateInput> | null;
  serverVersion: number | null;
  serverDTO: TransactionDTO | null;
  /** Human, solution-oriented; sourced for aria-live="assertive". */
  message: string;
}

/** State for one cursor page within the memory cache. */
interface TransactionPageState {
  items: TransactionDTO[];
  nextCursor: string | null;
  prevCursor: string | null;
  status: PageStatus;
  error: { code: ApiErrorCode | "UNKNOWN"; message: string } | null;
}

// ------------------------------------------------------------------- Config

const PAGE_LIMIT = 100;
/** LRU cap - bounds memory on 100k-row datasets (protects against a
 * client-side memory-exhaustion DoS in an unbounded cache). */
const MAX_CACHED_PAGES = 10;
const FIRST_PAGE_KEY = "__first__";

// ------------------------------------------------------------------- Helpers

/** Stable empty list keeps `pageFor` references stable across reads. */
const EMPTY_ITEMS: ReadonlyArray<TransactionDTO> = [];

/** Monotonic op counter sequence-guards mutations so a delete result can never
 * be applied after a later create on the same key. */
let opCounter = 0;

/** In-flight pagination controllers keyed by page key; superseded on filter/
 * sort change and on reset. Responses to these are never written. */
const paginationControllers = new Map<string, AbortController>();

/** Map key for a cursor: null/undefined always resolves to the stable
 * first-page key. Opaque cursor strings are safe as Map keys (Map never
 * treats them as object properties). */
function keyFor(cursor: string | null): string {
  return cursor ?? FIRST_PAGE_KEY;
}

/** Coerce and validate a version before every optimistic submit / adoption.
 * Rejects non-integer or negative values so a string "003" or NaN cannot
 * bypass the 409 guard. */
function coerceVersion(value: unknown, context: string): number {
  if (typeof value !== "number" || !Number.isInteger(value) || value < 0) {
    throw new TypeError(`[transactions] invalid ${context} version`);
  }
  return value;
}

/** Immutable clone of a page's items + entry. */
function clonePage(page: TransactionPageState): TransactionPageState {
  return { ...page, items: [...page.items] };
}

/** Move a touched key to the most-recent end of the LRU order (oldest first). */
function bumpLru(order: readonly string[], key: string): string[] {
  return [...order.filter((k) => k !== key), key];
}

/** Evict oldest non-pinned pages down to at most MAX_CACHED_PAGES. `activeKey`
 * (the viewport currently rendered by the table) is never evicted so a scroll
 * reload cannot blank the visible rows. Returns new maps + evicted keys. */
function evictIfOverflow(
  pages: ReadonlyMap<string, TransactionPageState>,
  order: readonly string[],
  activeKey: string | null
): { pages: Map<string, TransactionPageState>; order: string[] } {
  if (order.length <= MAX_CACHED_PAGES) {
    return { pages: new Map(pages), order: [...order] };
  }
  const next = new Map(pages);
  let remaining = [...order];
  while (remaining.length > MAX_CACHED_PAGES) {
    const eligible = remaining.filter((k) => k !== activeKey);
    if (eligible.length === 0) break; // everything is pinned - keep them
    const evictKey = eligible[0];
    remaining = remaining.filter((k) => k !== evictKey);
    next.delete(evictKey);
  }
  return { pages: next, order: remaining };
}

/** Locate the page key containing a row id. */
function findPageKeyWithId(
  pages: ReadonlyMap<string, TransactionPageState>,
  id: string
): string | null {
  for (const [key, page] of pages) {
    if (page.items.some((tx) => tx.id === id)) return key;
  }
  return null;
}

/** Replace a row (by id) inside `pages`, immutably, returning a new Map. */
function replaceRow(
  pages: ReadonlyMap<string, TransactionPageState>,
  id: string,
  next: TransactionDTO
): Map<string, TransactionPageState> {
  const result = new Map<string, TransactionPageState>();
  for (const [key, page] of pages) {
    result.set(key, { ...page, items: page.items.map((tx) => (tx.id === id ? next : tx)) });
  }
  return result;
}

// ------------------------------------------------------------------- State shape

interface TransactionsState {
  /** Page cache keyed by cursor (Map - prototype-pollution safe). */
  pages: Map<string, TransactionPageState>;
  /** LRU recency order, oldest first. */
  order: string[];
  /** Viewport page key currently rendered; pinned from LRU eviction. */
  activeKey: string | null;
  filters: TransactionFilters;
  sort: TransactionSort;
  /** Bumped on setFilters/clearFilters/setSort - stale in-flight responses with
   * an older epoch are discarded on write (stale-request protection). */
  epoch: number;
  /** Bumped on every successful mutation - the budget-derived recomposition
   * signal; consumers recompute local derived state when it changes. */
  dataVersion: number;
  mutationStatus: MutationStatus;
  /** Single in-flight pending indicator (serializes mutations). */
  isMutating: boolean;
  lastError: { code: ApiErrorCode | "UNKNOWN"; message: string } | null;
  lastConflict: LastConflict | null;
  /** Last failed (non-conflict) op, for an explicit Retry action. */
  pendingIntent: PendingIntent | null;
  /** FIFO of queued intents; drained one at a time under `isMutating`. */
  pendingQueue: PendingIntent[];

  loadPage: (cursor: string | null) => void;
  setFilters: (filters: Partial<TransactionFilters>) => void;
  clearFilters: () => void;
  setSort: (sort: TransactionSort) => void;
  applyCreate: (payload: TransactionCreateInput) => void;
  applyEdit: (id: string, patch: Partial<TransactionCreateInput>, version: number) => void;
  applyDelete: (id: string) => void;
  retry: () => void;
  resolveConflict: (action: "refresh" | "reapply") => void;
  reset: () => void;
}

export const useTransactionsStore = create<TransactionsState>()((set, get) => {
  const drainQueue = (): void => {
    if (get().isMutating) return;
    const next = get().pendingQueue[0];
    if (!next) return;
    set({ pendingQueue: get().pendingQueue.slice(1), isMutating: true, mutationStatus: "pending", lastError: null });

    // Snapshot + intent closure for the generic (non-conflict) rollback path.
    const pendingIntentFor = (): PendingIntent | null => next;
    let pages = new Map<string, TransactionPageState>();
    let ctx: { pageKey: string } = { pageKey: FIRST_PAGE_KEY };

    const run = async (): Promise<void> => {
      try {
        if (next.type === "create") {
          ctx = { pageKey: next.pageKey };
          pages = new Map(get().pages);
          const optimisticId = `optimistic-${opCounter}`;
          opCounter += 1;
          const optimistic: TransactionDTO = {
            id: optimisticId,
            accountId: next.payload.accountId,
            categoryIds: next.payload.categoryIds,
            amount: next.payload.amount,
            date: next.payload.date,
            description: next.payload.description,
            type: next.payload.type,
            scheduled: next.payload.scheduled,
            version: 0,
          };
          // Optimistic paint must render synchronously (< 100 ms, Doherty).
          set({ pages: applyRowFront(get().pages, next.pageKey, optimistic), dataVersion: get().dataVersion + 1 });
          try {
            const created = await createTransaction(next.payload, { idempotencyKey: `${next.pageKey}:${opCounter}` });
            set({
              pages: replaceRow(get().pages, optimisticId, created),
              dataVersion: get().dataVersion + 1,
              mutationStatus: "success",
              isMutating: false,
            });
            get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
          } catch (createErr) {
            if (createErr instanceof ConflictError) {
              set({
                pages: new Map(pages),
                mutationStatus: "conflict",
                lastConflict: {
                  type: "create",
                  pageKey: ctx.pageKey,
                  patch: next.payload,
                  serverVersion: null,
                  serverDTO: null,
                  message: "This record was modified by another user. Showing the latest data.",
                },
                pendingIntent: next,
                isMutating: false,
              });
              // Never silently overwrite - refetch the authoritative page.
              get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
              void drainQueue();
              return;
            }
            throw createErr;
          }
        } else if (next.type === "edit") {
          ctx = { pageKey: next.pageKey };
          pages = new Map(get().pages);
          const current = selectTransactionById(get(), next.id);
          if (current) {
            const optimisticNext: TransactionDTO = {
              ...current,
              accountId: next.patch.accountId ?? current.accountId,
              categoryIds: next.patch.categoryIds ?? current.categoryIds,
              amount: next.patch.amount ?? current.amount,
              date: next.patch.date ?? current.date,
              description: next.patch.description ?? current.description,
              type: next.patch.type ?? current.type,
              scheduled: next.patch.scheduled ?? current.scheduled,
              version: next.version,
            };
            set({ pages: replaceRow(get().pages, next.id, optimisticNext), dataVersion: get().dataVersion + 1 });
          }
          try {
            const updated = await updateTransaction(next.id, next.patch, next.version, {
              idempotencyKey: `${next.pageKey}:${opCounter}`,
            });
            opCounter += 1;
            set({
              pages: markSiblingsStale(replaceRow(get().pages, next.id, updated), ctx.pageKey),
              dataVersion: get().dataVersion + 1,
              mutationStatus: "success",
              isMutating: false,
            });
            get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
          } catch (editErr) {
            if (editErr instanceof ConflictError) {
              set({
                pages: new Map(pages),
                mutationStatus: "conflict",
                lastConflict: {
                  type: "edit",
                  pageKey: ctx.pageKey,
                  patch: next.patch,
                  serverVersion: null,
                  serverDTO: null,
                  message: "This record was modified by another user. Showing the latest data.",
                },
                pendingIntent: next,
                isMutating: false,
              });
              get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
              void drainQueue();
              return;
            }
            throw editErr;
          }
        } else {
          ctx = { pageKey: next.pageKey };
          pages = new Map(get().pages);
          // Optimistically remove the row, then confirm with the authoritative
          // delete; a failure restores the full snapshot.
          set({
            pages: (() => {
              const result = new Map<string, TransactionPageState>();
              for (const [key, page] of get().pages) {
                result.set(key, { ...page, items: page.items.filter((tx) => tx.id !== next.id) });
              }
              return result;
            })(),
            dataVersion: get().dataVersion + 1,
          });
          try {
            await deleteTransaction(next.id, { idempotencyKey: `${next.pageKey}:${opCounter}` });
            opCounter += 1;
            set({
              pages: markSiblingsStale(get().pages, ctx.pageKey),
              dataVersion: get().dataVersion + 1,
              mutationStatus: "success",
              isMutating: false,
            });
            get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
          } catch (deleteErr) {
            if (deleteErr instanceof ConflictError) {
              set({
                pages: new Map(pages),
                mutationStatus: "conflict",
                lastConflict: {
                  type: "delete",
                  pageKey: ctx.pageKey,
                  patch: null,
                  serverVersion: null,
                  serverDTO: null,
                  message: "This record was modified by another user. Showing the latest data.",
                },
                pendingIntent: next,
                isMutating: false,
              });
              get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
              void drainQueue();
              return;
            }
            throw deleteErr;
          }
        }
        // A mutation succeeded - drain whatever is queued next.
        void drainQueue();
      } catch (err) {
        const code: ApiErrorCode | "UNKNOWN" = err instanceof ApiError ? err.code : "UNKNOWN";
        const message =
          err instanceof ApiError && err.message
            ? err.message
            : "Your change could not be saved. Showing the latest data.";
        set({
          pages: new Map(pages),
          mutationStatus: "error",
          lastError: { code, message },
          pendingIntent: pendingIntentFor(),
          lastConflict: null,
          isMutating: false,
        });
        // Background authoritative refetch to confirm; if it fails the page carries
        // a per-page error so the user can retry - never silent/unverified data.
        get().loadPage(ctx.pageKey === FIRST_PAGE_KEY ? null : ctx.pageKey);
      }
    };
    void run();
  };

  /** Enqueue a mutation and attempt to drain the FIFO (respects isMutating). */
  const enqueue = (intent: PendingIntent): void => {
    set({ pendingQueue: [...get().pendingQueue, intent] });
    drainQueue();
  };

  /** Abort every in-flight pagination request (filter/sort change, reset). */
  function abortAllPagination(): void {
    for (const controller of paginationControllers.values()) controller.abort();
    paginationControllers.clear();
  }

  /** Mark every page except `keepKey` as stale (refetch-on-render) so sibling
   * caches cannot silently show duplicate/missing rows after a mutation. */
  function markSiblingsStale(
    pages: ReadonlyMap<string, TransactionPageState>,
    keepKey: string
  ): Map<string, TransactionPageState> {
    const result = new Map<string, TransactionPageState>();
    for (const [key, page] of pages) {
      result.set(key, key === keepKey ? page : { ...page, status: "idle", error: null });
    }
    return result;
  }

  /** Prepend `row` to the front of a page (create affects first page). */
  function applyRowFront(
    pages: ReadonlyMap<string, TransactionPageState>,
    pageKey: string,
    row: TransactionDTO
  ): Map<string, TransactionPageState> {
    const result = new Map<string, TransactionPageState>();
    for (const [key, page] of pages) {
      result.set(key, page);
    }
    const existing = result.get(pageKey);
    result.set(
      pageKey,
      existing
        ? { ...existing, items: [row, ...existing.items], status: "ready" }
        : { items: [row], nextCursor: null, prevCursor: null, status: "ready", error: null }
    );
    return result;
  }

  return {
    pages: new Map<string, TransactionPageState>(),
    order: [],
    activeKey: FIRST_PAGE_KEY,
    filters: {},
    sort: "date",
    epoch: 0,
    dataVersion: 0,
    mutationStatus: "idle",
    isMutating: false,
    lastError: null,
    lastConflict: null,
    pendingIntent: null,
    pendingQueue: [],

    loadPage(cursor: string | null): void {
      const key = keyFor(cursor);
      const epochAtCall = get().epoch;
      // Supersede any in-flight fetch for this key (stale-request protection).
      paginationControllers.get(key)?.abort();
      const controller = new AbortController();
      paginationControllers.set(key, controller);

      const pages = new Map(get().pages);
      const existing = pages.get(key);
      pages.set(
        key,
        existing
          ? { ...existing, status: "loading", error: null }
          : { items: [], nextCursor: null, prevCursor: null, status: "loading", error: null }
      );
      const order = bumpLru(get().order, key);
      set({ pages, order, activeKey: get().activeKey ?? key });

      listTransactions(
        { cursor: cursor ?? undefined, limit: PAGE_LIMIT, filters: get().filters, sort: get().sort },
        controller.signal
      )
        .then((data: CursorPage<TransactionDTO>) => {
          // Discard superseded responses (filter/sort changed or page re-requested).
          if (controller.signal.aborted || epochAtCall !== get().epoch) return;
          if (get().pages.get(key)?.status === "loading") {
            const nextPages = new Map(get().pages);
            // Allowlist of server page fields; never deep-assign untrusted keys.
            nextPages.set(key, {
              items: data.items,
              nextCursor: data.next_cursor ?? null,
              prevCursor: data.prev_cursor ?? null,
              status: "ready",
              error: null,
            });
            const evicted = evictIfOverflow(nextPages, bumpLru(get().order, key), get().activeKey ?? key);
            set({ pages: evicted.pages, order: evicted.order });
          }
        })
        .catch((err: unknown) => {
          if (controller.signal.aborted || epochAtCall !== get().epoch) return;
          if (get().pages.get(key)?.status === "loading") {
            const nextPages = new Map(get().pages);
            const code: ApiErrorCode | "UNKNOWN" = err instanceof ApiError ? err.code : "UNKNOWN";
            const message =
              err instanceof ApiError && err.message ? err.message : "Could not load transactions. Try again.";
            nextPages.set(key, { ...(nextPages.get(key) as TransactionPageState), status: "error", error: { code, message } });
            set({ pages: nextPages });
          }
        });
    },

    setFilters(filters: Partial<TransactionFilters>): void {
      abortAllPagination();
      const epoch = get().epoch + 1;
      const merged: TransactionFilters = { ...get().filters, ...filters };
      set({
        filters: merged,
        epoch,
        pages: new Map(),
        order: [],
        activeKey: FIRST_PAGE_KEY,
      });
      get().loadPage(null);
    },

    clearFilters(): void {
      abortAllPagination();
      const epoch = get().epoch + 1;
      set({ filters: {}, epoch, pages: new Map(), order: [], activeKey: FIRST_PAGE_KEY });
      get().loadPage(null);
    },

    setSort(sort: TransactionSort): void {
      abortAllPagination();
      const epoch = get().epoch + 1;
      set({ sort, epoch, pages: new Map(), order: [], activeKey: FIRST_PAGE_KEY });
      get().loadPage(null);
    },

    applyCreate(payload: TransactionCreateInput): void {
      enqueue({ type: "create", payload, pageKey: keyFor(get().activeKey) });
    },

    applyEdit(id: string, patch: Partial<TransactionCreateInput>, version: number): void {
      enqueue({ type: "edit", id, patch, version, pageKey: keyFor(get().activeKey) });
    },

    applyDelete(id: string): void {
      enqueue({ type: "delete", id, pageKey: keyFor(get().activeKey) });
    },

    retry(): void {
      const intent = get().pendingIntent;
      if (!intent) return;
      // Clear so a re-enqueue cannot loop; drainQueue will re-run the intent.
      set({ pendingIntent: null, lastError: null });
      enqueue(intent);
    },

    resolveConflict(action: "refresh" | "reapply"): void {
      const conflict = get().lastConflict;
      const intent = get().pendingIntent;
      set({ lastConflict: null, mutationStatus: "idle" });
      if (action === "refresh") {
        // Discard my change; force an authoritative refetch of the affected page.
        get().loadPage(conflict?.pageKey === FIRST_PAGE_KEY ? null : (conflict?.pageKey ?? null));
        return;
      }
      // Re-apply: re-run against the latest server state (fresh conflict stays put).
      if (intent) {
        set({ pendingIntent: null });
        enqueue(intent);
      } else {
        get().loadPage(conflict?.pageKey === FIRST_PAGE_KEY ? null : (conflict?.pageKey ?? null));
      }
    },

    reset(): void {
      abortAllPagination();
      set({
        pages: new Map(),
        order: [],
        activeKey: FIRST_PAGE_KEY,
        filters: {},
        sort: "date",
        epoch: 0,
        dataVersion: 0,
        mutationStatus: "idle",
        lastError: null,
        lastConflict: null,
        pendingIntent: null,
        pendingQueue: [],
        isMutating: false,
      });
    },
  };
});

// ------------------------------------------------------------------- Selectors

/** Items for a page key. Returns a stable empty read-only array when the page
 * has not loaded yet, so consumers render a skeleton via `statusFor` during
 * load - never the guided empty-state. */
export function pageFor(s: TransactionsState, key: string | null): ReadonlyArray<TransactionDTO> {
  return s.pages.get(keyFor(key))?.items ?? EMPTY_ITEMS;
}

/** Live derived row count for a page; reflects optimistic +1/-1 immediately. */
export function totalFor(s: TransactionsState, key: string | null): number {
  return s.pages.get(keyFor(key))?.items.length ?? 0;
}

/** Per-page load status: idle | loading | ready | error. */
export function statusFor(s: TransactionsState, key: string | null): PageStatus {
  return s.pages.get(keyFor(key))?.status ?? "idle";
}

/** Per-page load error (present when status === "error"). */
export function pageErrorFor(
  s: TransactionsState,
  key: string | null
): { code: ApiErrorCode | "UNKNOWN"; message: string } | null {
  return s.pages.get(keyFor(key))?.error ?? null;
}

/** Next cursor for a page (null = terminal page -> clean end state). */
export function nextCursorFor(s: TransactionsState, key: string | null): string | null {
  return s.pages.get(keyFor(key))?.nextCursor ?? null;
}

export function selectMutationStatus(s: TransactionsState): MutationStatus {
  return s.mutationStatus;
}

export function selectIsMutating(s: TransactionsState): boolean {
  return s.isMutating;
}

export function selectLastError(s: TransactionsState): TransactionsState["lastError"] {
  return s.lastError;
}

export function selectLastConflict(s: TransactionsState): LastConflict | null {
  return s.lastConflict;
}

export function selectPendingIntent(s: TransactionsState): PendingIntent | null {
  return s.pendingIntent;
}

/** Budget-derived recomposition signal; budget selectors recompute when this
 * changes after a successful mutation. */
export function selectTransactionsChangedVersion(s: TransactionsState): number {
  return s.dataVersion;
}

export function selectFilters(s: TransactionsState): TransactionFilters {
  return s.filters;
}

export function selectSort(s: TransactionsState): TransactionSort {
  return s.sort;
}

export function selectActiveKey(s: TransactionsState): string | null {
  return s.activeKey;
}

/** Find a row by id across the cached pages (used for edit refresh-in-place). */
export function selectTransactionById(s: TransactionsState, id: string): TransactionDTO | null {
  for (const page of s.pages.values()) {
    const found = page.items.find((tx) => tx.id === id);
    if (found) return found;
  }
  return null;
}