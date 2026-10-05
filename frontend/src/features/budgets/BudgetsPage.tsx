import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { EmptyState } from "../../components/ui/EmptyState";
import { Skeleton } from "../../components/ui/Skeleton";
import { Pagination } from "../../components/ui/Pagination";
import { ConfirmDialog } from "../../components/ui/ConfirmDialog";
import { BudgetVsActualCard } from "./BudgetVsActualCard";
import { usePermission } from "../../hooks/usePermission";
import { listBudgets } from "../../services/api/endpoints";
import type { BudgetDTO, BudgetPeriodType } from "../../services/api/types";

const CSTYLE_ID = "fms-budgets-page-styles";
const CSTYLE_CSS = `
.bp-page{max-width:1280px;margin:0 auto;padding:24px 24px 64px;}
.bp-header{display:flex;align-items:center;justify-content:space-between;gap:16px;margin:0 0 24px;}
.bp-title{margin:0;font-size:24px;font-weight:600;line-height:1.25;color:#0f172a;}
.bp-create{min-height:44px;padding:8px 16px;background:#2563eb;color:#fff;border:0;border-radius:8px;font-size:14px;font-weight:500;cursor:pointer;transition:background-color 150ms ease-out;}
.bp-create:hover{background:#1d4ed8;}
.bp-create:focus-visible{outline:2px solid #2563eb;outline-offset:2px;}
.bp-groups{display:flex;flex-direction:column;gap:32px;}
.bp-group{background:#f8fafc;border:1px solid #e2e8f0;border-radius:12px;padding:16px;}
.bp-group__header{display:flex;align-items:baseline;gap:12px;margin-bottom:12px;}
.bp-group__title{margin:0;font-size:12px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:#64748b;}
.bp-count{font-size:12px;font-weight:500;color:#94a3b8;}
.bp-list{margin:0;padding:0;list-style:none;}
.bp-card-row{margin-top:12px;}
.bp-card--active{box-shadow:0 4px 6px -1px rgba(0,0,0,0.1),0 2px 4px -2px rgba(0,0,0,0.06);}
.bp-row{display:flex;align-items:flex-start;gap:12px;}
.bp-actions{margin-left:auto;display:flex;gap:8px;flex:none;}
.bp-action{display:inline-flex;align-items:center;justify-content:center;min-width:44px;min-height:44px;border:0;border-radius:8px;background:transparent;color:#6b7280;cursor:pointer;transition:background-color 150ms ease-out,color 150ms ease-out;}
.bp-action:hover{background:#f1f5f9;color:#0f172a;}
.bp-action:focus-visible{outline:2px solid #2563eb;outline-offset:2px;}
.bp-action--danger:hover{background:#fee2e2;color:#dc2626;}
.bp-badge{display:inline-flex;align-items:center;gap:6px;height:24px;padding:0 10px;border-radius:9999px;font-size:12px;font-weight:500;}
.bp-badge--active{background:#eff6ff;color:#1d4ed8;}
.bp-badge--upcoming{background:#f8fafc;color:#475569;border:1px solid #e2e8f0;}
.bp-badge--closed{background:#eef2f7;color:#64748b;border:1px solid #e2e8f0;}
.bp-period{margin:8px 0 0;font-size:12px;font-weight:500;letter-spacing:.04em;text-transform:uppercase;color:#94a3b8;}
.bp-skel-groups{display:flex;flex-direction:column;gap:32px;}
.bp-skel-group{background:#f8fafc;border:1px solid #e2e8f0;border-radius:12px;padding:16px;}
.bp-skel-list{display:flex;flex-direction:column;gap:12px;margin-top:12px;}
.bp-sr{position:absolute;width:1px;height:1px;clip:rect(0 0 0 0);overflow:hidden;white-space:nowrap;border:0;padding:0;margin:-1px;}
@media (max-width:767px){
  .bp-page{padding:16px 16px 64px;}
  .bp-header{flex-direction:column;align-items:stretch;}
  .bp-create{width:100%;}
  .bp-row{flex-direction:column;align-items:stretch;}
  .bp-actions{margin-left:0;}
}
@media (prefers-reduced-motion:reduce){
  .bp-create,.bp-action,.bp-card--active{transition:none;}
}
`;

/** Labels and display order for budget period types, rendered as static text. */
const PERIOD_ORDER: readonly BudgetPeriodType[] = ["MONTHLY", "QUARTERLY", "ANNUAL"];

const PERIOD_LABEL: Record<BudgetPeriodType, string> = {
  MONTHLY: "Monthly",
  QUARTERLY: "Quarterly",
  ANNUAL: "Annual",
};

export type BudgetStatus = "active" | "upcoming" | "closed";

const STATUS_LABEL: Record<BudgetStatus, string> = {
  active: "Active",
  upcoming: "Upcoming",
  closed: "Closed",
};

const STATUS_ICON_META: Record<BudgetStatus, { cls: string; glyph: ReactNode }> = {
  active: {
    cls: "bp-badge bp-badge--active",
    glyph: (
      <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
        <path d="M12 22a10 10 0 1 1 0-20 10 10 0 0 1 0 20z" />
      </svg>
    ),
  },
  upcoming: {
    cls: "bp-badge bp-badge--upcoming",
    glyph: (
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" aria-hidden="true">
        <circle cx="12" cy="12" r="9" />
      </svg>
    ),
  },
  closed: {
    cls: "bp-badge bp-badge--closed",
    glyph: (
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" aria-hidden="true">
        <path d="M5 12h14" />
      </svg>
    ),
  },
};

/** ISO date strings compare lexicographically, so budget period status is a pure string comparison. */
function periodStatus(b: BudgetDTO, today: string): BudgetStatus {
  if (today < b.periodStart) return "upcoming";
  if (today > b.periodEnd) return "closed";
  return "active";
}

/** Human-readable period window rendered as a caption, e.g. "2025-01-01 – 2025-03-31". */
function periodRangeText(b: BudgetDTO): string {
  return `${b.periodStart} \u2013 ${b.periodEnd}`;
}

export type BudgetsPageToast = {
  severity: "info" | "success" | "warning" | "error";
  title: string;
  body?: string;
};

export type BudgetsPageProps = {
  onCreateBudget?: () => void;
  onEditBudget?: (budget: BudgetDTO) => void;
  onBackToDashboard?: () => void;
  onToast?: (toast: BudgetsPageToast) => void;
  onInvalidateCaches?: () => void;
  /** Parent-provided network delete; the page owns the confirm dialog and in-flight state. */
  onDeleteBudget?: (budget: BudgetDTO) => Promise<void>;
};

const PAGE_SIZE = 50;

/**
 * Budgets list page (route /budgets): budgets grouped by period type then row,
 * each with a vs-actual card, offset pagination over a bounded collection, and
 * permission-gated create/edit/delete affordances.
 */
export default function BudgetsPage({
  onCreateBudget,
  onEditBudget,
  onBackToDashboard,
  onToast,
  onInvalidateCaches,
  onDeleteBudget,
}: BudgetsPageProps) {
  const { can } = usePermission();
  const canRead = can("budget:read");
  const canWrite = can("budget:write");

  const [budgets, setBudgets] = useState<BudgetDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">("loading");
  const [retryNonce, setRetryNonce] = useState(0);
  const [today, setToday] = useState(() => new Date().toISOString().slice(0, 10));
  const [deleteTarget, setDeleteTarget] = useState<BudgetDTO | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);

  useEffect(() => {
    if (!canRead) return;
    const controller = new AbortController();
    setLoadState("loading");
    listBudgets({ page, pageSize: PAGE_SIZE }, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        setBudgets(result.items ?? []);
        setTotal(result.total ?? 0);
        setLoadState("ready");
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        if ((err as { name?: string } | null)?.name === "AbortError") return;
        setBudgets([]);
        setLoadState("error");
      });
    return () => controller.abort();
  }, [canRead, page, retryNonce]);

  useEffect(() => {
    if (typeof document === "undefined" || document.getElementById(CSTYLE_ID)) return;
    const el = document.createElement("style");
    el.id = CSTYLE_ID;
    el.textContent = CSTYLE_CSS;
    document.head.appendChild(el);
  }, []);

  const grouped = useMemo(() => {
    const groups = new Map<BudgetPeriodType, BudgetDTO[]>();
    for (const b of budgets) {
      const arr = groups.get(b.periodType);
      if (arr) arr.push(b);
      else groups.set(b.periodType, [b]);
    }
    const out: { type: BudgetPeriodType; items: BudgetDTO[] }[] = [];
    for (const type of PERIOD_ORDER) {
      const items = groups.get(type);
      if (items) out.push({ type, items });
    }
    return out;
  }, [budgets]);

  const handleDelete = async () => {
    if (!deleteTarget || !onDeleteBudget) return;
    setDeletingId(deleteTarget.id);
    try {
      await onDeleteBudget(deleteTarget);
      onToast?.({ severity: "success", title: "Budget deleted" });
      onInvalidateCaches?.();
      setRetryNonce((n) => n + 1);
      setDeleteTarget(null);
    } catch {
      onToast?.({
        severity: "error",
        title: "Couldn\u2019t delete budget",
        body: "Please try again.",
      });
      setDeleteTarget(null);
    } finally {
      setDeletingId(null);
    }
  };

  return (
    <main className="bp-page">
      {!canRead ? (
        <section role="alert">
          <EmptyState
            tone="info"
            title="Insufficient permissions"
            description="You need read access to view budgets."
            actionLabel={onBackToDashboard ? "Back to dashboard" : undefined}
            onAction={onBackToDashboard}
          />
        </section>
      ) : loadState === "loading" ? (
        <section>
          <div role="status">
            <span className="bp-sr">Loading budgets…</span>
          </div>
          <div className="bp-skel-groups" aria-hidden="true">
            {PERIOD_ORDER.map((type) => (
              <div className="bp-skel-group" key={type}>
                <Skeleton
                  variant="text"
                  lines={1}
                  width="120px"
                  height={18}
                  announceOnLoad={false}
                />
                <div className="bp-skel-list">
                  {[0, 1].map((i) => (
                    <Skeleton
                      key={i}
                      variant="rect"
                      width="100%"
                      height={96}
                      announceOnLoad={false}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>
        </section>
      ) : loadState === "error" ? (
        <section role="alert">
          <EmptyState
            tone="info"
            title="Failed to load budgets"
            description="We couldn\u2019t load budgets. Check your connection and try again."
            actionLabel="Retry"
            onAction={() => setRetryNonce((n) => n + 1)}
          />
        </section>
      ) : budgets.length === 0 ? (
        <section role="status">
          {canWrite && onCreateBudget ? (
            <EmptyState
              tone="info"
              title="Set up your first budget"
              description="Add a budget to start tracking actual spending against a category allocation."
              actionLabel="Create first budget"
              onAction={onCreateBudget}
            />
          ) : (
            <EmptyState
              tone="neutral"
              title="No budgets yet"
              description="No budgets have been set up in this workspace yet. Ask a manager with budget permissions to create one."
            />
          )}
        </section>
      ) : (
        <>
          <header className="bp-header">
            <h1 className="bp-title">Budgets</h1>
            {canWrite && onCreateBudget ? (
              <button type="button" className="bp-create" onClick={onCreateBudget}>
                New budget
              </button>
            ) : null}
          </header>

          <div className="bp-groups">
            {grouped.map((group) => (
              <section className="bp-group" key={group.type}>
                <header className="bp-group__header">
                  <h2 className="bp-group__title">{PERIOD_LABEL[group.type]}</h2>
                  <span className="bp-count">
                    {group.items.length} {group.items.length === 1 ? "budget" : "budgets"}
                  </span>
                </header>
                <ul className="bp-list">
                  {group.items.map((budget) => {
                    const status = periodStatus(budget, today);
                    const badgeMeta = STATUS_ICON_META[status];
                    const label = String(budget.categoryId);
                    const isActive = status === "active";
                    return (
                      <li className="bp-card-row" key={budget.id}>
                        <BudgetVsActualCard
                          budget={{ id: budget.id }}
                          label={`Budget for category ${label}`}
                          className={isActive ? "bp-card--active" : undefined}
                        />
                        <div className="bp-row">
                          <span className={badgeMeta.cls}>
                            {badgeMeta.glyph}
                            {STATUS_LABEL[status]}
                          </span>
                          <p className="bp-period">{periodRangeText(budget)}</p>
                          {canWrite ? (
                            <div className="bp-actions">
                              {onEditBudget ? (
                                <button
                                  type="button"
                                  className="bp-action"
                                  aria-label={`Edit budget ${label}`}
                                  onClick={() => onEditBudget(budget)}
                                >
                                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                                    <path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z" />
                                  </svg>
                                </button>
                              ) : null}
                              {onDeleteBudget ? (
                                <button
                                  type="button"
                                  className="bp-action bp-action--danger"
                                  aria-label={`Delete budget ${label}`}
                                  onClick={() => setDeleteTarget(budget)}
                                >
                                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                                    <path d="M3 6h18" />
                                    <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" />
                                    <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
                                    <path d="M10 11v6M14 11v6" />
                                  </svg>
                                </button>
                              ) : null}
                            </div>
                          ) : null}
                        </div>
                      </li>
                    );
                  })}
                </ul>
              </section>
            ))}
          </div>

          {budgets.length > 0 && total > PAGE_SIZE ? (
            <Pagination
              offset={{ page, total, pageSize: PAGE_SIZE }}
              onChange={({ page: nextPage }) => {
                if (nextPage) setPage(nextPage);
              }}
              loading={loadState === "loading"}
            />
          ) : null}
        </>
      )}

      <ConfirmDialog
        open={deleteTarget !== null}
        title="Delete budget"
        danger
        consequence={
          deleteTarget
            ? "Deleting this budget removes all allocations for it. This cannot be undone and restores no historical allocations."
            : ""
        }
        confirmLabel="Delete"
        cancelLabel="Cancel"
        loading={deletingId !== null}
        onConfirm={() => void handleDelete()}
        onCancel={() => {
          if (deletingId === null) setDeleteTarget(null);
        }}
      />
    </main>
  );
}