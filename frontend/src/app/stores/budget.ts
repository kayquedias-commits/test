import { create } from "zustand";
import { useTransactionsStore, selectTransactionsChangedVersion } from "./transactions";
import { vsActual } from "../../services/api/endpoints";

/**
 * Budget-progress derived store (BP §8.1 / §8.4).
 *
 * Pure, reactive projection over the transactions store: budget actuals recompute the moment
 * transactions change (add/edit/delete) so `BudgetVsActualCard` reflects the new standing
 * immediately, without a server round-trip.
 *
 * This store is a NON-AUTHORITATIVE client mirror. It is a display projection only — it never
 * gates a mutation or drives an audit/security decision. The authoritative figure, when a cold
 * cache needs a warm-up, comes from `GET /v1/budgets/{id}/vs-actual` and is flagged
 * `source === "authoritative"`; anything derived client-side is flagged `source === "derived"`.
 */

type Money = { amount: string; currency: string };
type BudgetPeriodType = "MONTHLY" | "QUARTERLY" | "ANNUAL";

/** Shape consumed by setAllocations; structurally matched from the budgets store response. */
type BudgetLike = {
  id: string;
  categoryId: string;
  periodType: BudgetPeriodType;
  periodStart: string;
  periodEnd: string;
  allocated: Money;
};

interface Allocation {
  categoryId: string;
  periodType: BudgetPeriodType;
  periodStart: string;
  periodEnd: string;
  allocated: Money;
}

interface Actuals {
  spent: Money;
  income: Money;
  transactionCount: number;
}

type BudgetSource = "derived" | "authoritative";

/**
 * Exhaustive, mutually exclusive visual states for the card. Each maps to an icon + text pairing
 * (color is never the sole indicator, WCAG 1.4.1 / spec §8.3). `unloaded` and `no-allocation`
 * always carry `pct: null` so the card renders a placeholder, never a misleading bar.
 */
type BudgetStateCode = "unloaded" | "no-allocation" | "within" | "approaching" | "overrun";

interface ProgressResult {
  /** True percentage (may exceed 100 on overrun); null when unloaded or no-allocation. */
  pct: number | null;
  state: BudgetStateCode;
  loaded: boolean;
  overrun: boolean;
  approaching: boolean;
  /** "derived" = client mirror, "authoritative" = confirmed by server vs-actual. */
  source: BudgetSource;
  allocated: Money | null;
  spent: Money | null;
  income: Money | null;
  transactionCount: number;
  refreshFailed: boolean;
}

interface BudgetState {
  allocations: Map<string, Allocation>;
  actuals: Map<string, Actuals>;
  loaded: Map<string, boolean>;
  source: Map<string, BudgetSource>;
  refreshFailed: Map<string, boolean>;
  setAllocations: (budgets: BudgetLike[]) => void;
  recompute: () => void;
  invalidateCache: (budgetId?: string) => void;
  refreshActuals: (budgetId: string) => Promise<void>;
  progressFor: (budgetId: string) => ProgressResult;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const CURRENCY_RE = /^[A-Z]{3}$/;
const AMOUNT_RE = /^[+-]?\d{1,15}(\.\d{1,2})?$/;
const MINOR_FACTOR = 100n;

function assertBudgetUuid(id: string, label: string): void {
  if (typeof id !== "string" || !UUID_RE.test(id)) {
    throw new TypeError(`${label} is not a valid budget id`);
  }
}

function isMoney(value: unknown): value is Money {
  if (typeof value !== "object" || value === null) return false;
  const m = value as Record<string, unknown>;
  if (typeof m.amount !== "string" || typeof m.currency !== "string") return false;
  if (!AMOUNT_RE.test(m.amount)) return false;
  if (!CURRENCY_RE.test(m.currency)) return false;
  return parseMinorUnitsSafe(m.amount) !== null;
}

/** Exact minor-units parser (never binary floats). Returns null for any non-conforming amount. */
function parseMinorUnitsSafe(amount: unknown): bigint | null {
  if (typeof amount !== "string" || !AMOUNT_RE.test(amount)) return null;
  const sign = amount.startsWith("-") ? -1n : 1n;
  const digits = amount.replace(/[+-]/g, "");
  const [wholeRaw = "0", fracRaw = ""] = digits.split(".");
  const whole = BigInt(wholeRaw || "0");
  const fracPadded = fracRaw.padEnd(2, "0");
  const minor = whole * MINOR_FACTOR + BigInt(fracPadded || "0");
  return sign * minor;
}

/** Format integer minor units as a fixed two-decimal string (e.g. "-1234.50"). */
function minorToString(units: bigint): string {
  const sign = units < 0n ? "-" : "";
  const abs = units < 0n ? -units : units;
  const whole = abs / MINOR_FACTOR;
  const frac = (abs % MINOR_FACTOR).toString().padStart(2, "0");
  return `${sign}${whole.toString()}.${frac}`;
}

function roundProgressPct(spentMinor: bigint, allocMinor: bigint): number {
  // Budgets are amount-bounded so both values sit well below 2^53; the BigInt ratio is exact.
  const pct = (Number(spentMinor) / Number(allocMinor)) * 100;
  return Math.round(pct * 100) / 100;
}

/** Budgets dirtied by a transaction mutation. "all" forces a full recompute; else per-budget. */
let dirtyBudgetIds: Set<string> | "all" = "all";

/** Per-budget AbortController so a cold-cache refresh can cancel a stale in-flight request. */
const refreshControllers = new Map<string, AbortController>();

export const useBudgetStore = create<BudgetState>()((set, get) => ({
  allocations: new Map<string, Allocation>(),
  actuals: new Map<string, Actuals>(),
  loaded: new Map<string, boolean>(),
  source: new Map<string, BudgetSource>(),
  refreshFailed: new Map<string, boolean>(),

  setAllocations: (budgets) => {
    const allocations = new Map(get().allocations);
    const loaded = new Map(get().loaded);
    const source = new Map(get().source);
    const refreshFailed = new Map(get().refreshFailed);
    for (const b of budgets) {
      if (!isMoney(b.allocated)) continue;
      allocations.set(b.id, {
        categoryId: b.categoryId,
        periodType: b.periodType,
        periodStart: b.periodStart,
        periodEnd: b.periodEnd,
        allocated: b.allocated,
      });
      loaded.set(b.id, true);
      source.set(b.id, "derived");
      refreshFailed.set(b.id, false);
    }
    set({ allocations, loaded, source, refreshFailed });
    idleRecompute();
  },

  recompute: () => {
    const { allocations, actuals, loaded, source } = get();
    const snap = useTransactionsStore.getState();
    const targetIds =
      dirtyBudgetIds === "all"
        ? [...allocations.keys()]
        : [...dirtyBudgetIds];

    const nextActuals = new Map(actuals);
    const nextSource = new Map(source);
    const rederived = new Map<string, Actuals>();

    for (const id of targetIds) {
      const alloc = allocations.get(id);
      if (!alloc || !loaded.get(id)) continue;
      // Strict period window; equal bounds are valid. A row just outside must not bleed in.
      const inWindow = (date: string) => alloc.periodStart <= date && date <= alloc.periodEnd;
      let spentMinor = 0n;
      let incomeMinor = 0n;
      let count = 0;
      for (const page of snap.pages.values()) {
        for (const row of page.items) {
          if (!inWindow(row.date)) continue;
          if (!row.categoryIds.includes(alloc.categoryId)) continue;
          const minor = parseMinorUnitsSafe(row.amount?.amount);
          if (minor === null) continue;
          if (row.type === "EXPENSE") {
            spentMinor += minor;
            count += 1;
          } else if (row.type === "INCOME") {
            incomeMinor += minor;
          }
        }
      }
      const currency = alloc.allocated.currency;
      rederived.set(id, {
        spent: { amount: minorToString(spentMinor), currency },
        income: { amount: minorToString(incomeMinor), currency },
        transactionCount: count,
      });
      nextSource.set(id, "derived");
    }

    for (const [id, actual] of rederived) nextActuals.set(id, actual);
    set({ actuals: nextActuals, source: nextSource });
    dirtyBudgetIds = new Set<string>();
  },

  // Per-budget scoping: a single expense recomputes only that budget, not every budget — the
  // INP <=200ms budget is preserved even at 100k-transaction scale. `invalidateCache` is a
  // data-only operation: no DOM writes, no executable serialization.
  invalidateCache: (budgetId) => {
    if (budgetId === undefined) {
      dirtyBudgetIds = "all";
      return;
    }
    assertBudgetUuid(budgetId, "budgetId");
    if (dirtyBudgetIds === "all") return;
    dirtyBudgetIds.add(budgetId);
  },

  /**
   * Non-blocking cold-cache warm-up. Strictly a GET (no mutation, no CSRF token needed). On
   * failure it retains the last-known-good derived actuals and exposes `refreshFailed` rather
   * than surfacing an error screen.
   */
  refreshActuals: async (budgetId) => {
    assertBudgetUuid(budgetId, "budgetId");
    refreshControllers.get(budgetId)?.abort();
    const controller = new AbortController();
    refreshControllers.set(budgetId, controller);

    set((s) => {
      const loaded = new Map(s.loaded).set(budgetId, true);
      return { loaded };
    });

    try {
      const dto = await vsActual(budgetId, controller.signal);
      if (controller.signal.aborted) return;
      if (!isMoney(dto.allocated) || !isMoney(dto.spent)) return;
      if (typeof dto.pct !== "number" || !Number.isFinite(dto.pct)) return;
      const allocations = new Map(get().allocations);
      const actuals = new Map(get().actuals);
      const source = new Map(get().source);
      const refreshFailed = new Map(get().refreshFailed);
      // Replace, never merge — stale/tampered client math must not persist.
      allocations.set(budgetId, {
        categoryId: allocations.get(budgetId)?.categoryId ?? "",
        periodType: allocations.get(budgetId)?.periodType ?? "MONTHLY",
        periodStart: allocations.get(budgetId)?.periodStart ?? "",
        periodEnd: allocations.get(budgetId)?.periodEnd ?? "",
        allocated: dto.allocated,
      });
      actuals.set(budgetId, {
        spent: dto.spent,
        income: actuals.get(budgetId)?.income ?? { amount: "0.00", currency: dto.spent.currency },
        transactionCount: actuals.get(budgetId)?.transactionCount ?? 0,
      });
      source.set(budgetId, "authoritative");
      refreshFailed.set(budgetId, false);
      set({ allocations, actuals, source, refreshFailed });
    } catch (err) {
      if (controller.signal.aborted) return;
      // Non-blocking failure: keep last-known-good derived data, surface the flag.
      set((s) => {
        const refreshFailed = new Map(s.refreshFailed).set(budgetId, true);
        const source = new Map(s.source).set(budgetId, "derived");
        return { refreshFailed, source };
      });
    } finally {
      refreshControllers.delete(budgetId);
    }
  },

  progressFor: (budgetId) => {
    assertBudgetUuid(budgetId, "budgetId");
    const { allocations, actuals, loaded, source, refreshFailed } = get();
    const isLoaded = loaded.get(budgetId) ?? false;
    const src = source.get(budgetId) ?? "derived";
    const failed = refreshFailed.get(budgetId) ?? false;

    if (!isLoaded) {
      // Distinct from a genuine 0% spend — the card shows a skeleton placeholder.
      return {
        pct: null,
        state: "unloaded",
        loaded: false,
        overrun: false,
        approaching: false,
        source: src,
        allocated: null,
        spent: null,
        income: null,
        transactionCount: 0,
        refreshFailed: failed,
      };
    }

    const alloc = allocations.get(budgetId);
    const result: ProgressResult = (() => {
      const allocMinor =
        alloc && parseMinorUnitsSafe(alloc.allocated.amount) !== null
          ? (parseMinorUnitsSafe(alloc.allocated.amount) as bigint)
          : null;
      if (allocMinor === null || allocMinor <= 0n) {
        // Zero/undefined denominator — never compute pct (no NaN/Infinity).
        return {
          pct: null,
          state: "no-allocation",
          loaded: true,
          overrun: false,
          approaching: false,
          source: src,
          allocated: null,
          spent: null,
          income: null,
          transactionCount: 0,
          refreshFailed: failed,
        };
      }
      const actual = actuals.get(budgetId);
      const spentMinor = parseMinorUnitsSafe(actual?.spent.amount) ?? 0n;
      const pct = roundProgressPct(spentMinor, allocMinor);

      // Threshold boundary: an exact 100% is AT the cap, i.e. overrun — never "approaching".
      const approaching = pct >= 90 && pct < 100;
      const overrun = pct >= 100;
      const state: BudgetStateCode = overrun ? "overrun" : approaching ? "approaching" : "within";

      return {
        pct,
        state,
        loaded: true,
        overrun,
        approaching,
        source: src,
        allocated: alloc?.allocated ?? null,
        spent: actual?.spent ?? null,
        income: actual?.income ?? null,
        transactionCount: actual?.transactionCount ?? 0,
        refreshFailed: failed,
      };
    })();

    return result;
  },
}));

function idleRecompute(): void {
  // Deferred so an allocations-only hydration (no transaction rows yet) still flips loaded=true.
  setTimeout(() => useBudgetStore.getState().recompute(), 0);
}

// Recompute whenever the transactions store advances its committed version (post-commit, never
// optimistically) so derived actuals always reflect the committed state — a rolled-back mutation
// yields the reverted standing automatically.
useTransactionsStore.subscribe((s, prev) => {
  if (selectTransactionsChangedVersion(s) !== selectTransactionsChangedVersion(prev)) {
    useBudgetStore.getState().recompute();
  }
});