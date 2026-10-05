import { Component, lazy, Suspense, useEffect, useRef } from "react";
import type { ComponentType, ErrorInfo, ReactNode } from "react";
import type { RouteObject } from "react-router-dom";
import {
  createBrowserRouter,
  Navigate,
  Outlet,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { can } from "./permissions";
import { useSession } from "./stores/session";
import AppShell from "../components/layout/AppShell";
import { Skeleton } from "../components/ui/Skeleton";
import { Button } from "../components/ui/Button";
import { usePermission } from "../hooks/usePermission";
import type { Permission, UserRole } from "../services/api/types";

/* ---------------------------------------------------------------------------
 * Colocated styles (CSP-safe <style>, no inline style=/inline handlers) —
 * mirrors the Button/Skeleton/AppShell convention. Animate only transform/opacity.
 * ------------------------------------------------------------------------- */
const ROUTER_CSS = `
.route-skeleton{display:flex;flex-direction:column;gap:24px;}
.route-skeleton__header{height:48px;width:100%;}
.route-skeleton__filter{height:40px;width:100%;max-width:420px;}
.route-skeleton__status{
  position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;
  clip:rect(0 0 0 0);clip-path:inset(50%);white-space:nowrap;border:0;
}
.route-skeleton__rows{display:flex;flex-direction:column;gap:8px;}
.route-skeleton__row{min-height:56px;width:100%;}
.route-skeleton__cards{display:grid;grid-template-columns:1fr;gap:24px;}
.route-skeleton__card{min-height:96px;width:100%;}
.route-skeleton__panel{min-height:320px;width:100%;}
@media (min-width:768px){
  .route-skeleton__cards{grid-template-columns:repeat(3,minmax(0,1fr));}
}
.forbidden{
  display:flex;flex-direction:column;align-items:center;justify-content:center;
  text-align:center;min-height:60vh;width:100%;max-width:560px;margin:0 auto;
  padding:48px 24px;
}
.forbidden__icon{
  width:48px;height:48px;color:var(--color-danger,#dc2626);
  margin-bottom:16px;display:inline-flex;align-items:center;justify-content:center;
}
.forbidden__lock{width:48px;height:48px;stroke:currentColor;stroke-width:1.6;fill:none;}
.forbidden__title{
  font-size:24px;font-weight:600;line-height:1.25;color:var(--color-text,#0f172a);
  margin:0 0 8px;
}
.forbidden__body{
  font-size:16px;line-height:1.5;color:var(--color-text-secondary,#475569);
  margin:0 0 8px;max-width:60ch;
}
.forbidden__caption{
  font-size:14px;line-height:1.5;color:var(--color-text-muted,#64748b);
  margin:0 0 24px;
}
.forbidden__actions{display:flex;flex-direction:column;gap:12px;width:100%;align-items:stretch;}
@media (min-width:768px){
  .forbidden__actions{flex-direction:row;align-items:center;justify-content:center;}
  .forbidden__actions .fms-btn{min-width:160px;}
}
@media (max-width:767px){
  /* Full-width primary action in the natural thumb zone (Blueprint §6 layout). */
  .forbidden__actions .fms-btn{width:100%;}
}
@media (prefers-reduced-motion:reduce){
  .forbidden{animation:none;}
}
`;

if (typeof document !== "undefined" && !document.getElementById("fms-router-styles")) {
  const style = document.createElement("style");
  style.id = "fms-router-styles";
  style.textContent = ROUTER_CSS;
  document.head.appendChild(style);
}

/* ---------------------------------------------------------------------------
 * Route meta — single source of truth for routing AND navigation.
 * `section` groups the nav into the four visual clusters (Primary / Records /
 * Insights / Administration); nav labels match the URL and page <h1> (Jakob's
 * Law — no invented synonyms).
 * ------------------------------------------------------------------------- */
export type RouteSection = "primary" | "records" | "insights" | "admin";
export type SkeletonArchetype = "dashboard" | "table" | "panel";

export interface RouteMeta {
  /** Static route path, e.g. "/transactions". */
  path: string;
  /** Navigation label — matches the URL slug and the page's <h1>. */
  label: string;
  /** Permission required to open the route; null = any authenticated role. */
  permission: Permission | null;
  /** document.title suffix, e.g. "Transactions". */
  title: string;
  /** Nav clustering for `filterRoutesByPermission`. */
  section: RouteSection;
  /** Skeleton archetype this page's loading state should mimic. */
  skeleton: SkeletonArchetype;
  /** React.lazy factory. Modules under features/reports|exports|admin split into
   * the matching manual chunks (vite.config.ts) — kept 1:1 so no inline code
   * leaks into the entry bundle (strict-CSP should allow only hashed chunks). */
  lazy: () => Promise<{ default: ComponentType }>;
}

/**
 * Everything the app can navigate to, grouped by section. The router tree is
 * generated from this table, and `filterRoutesByPermission` produces the nav
 * subset a role may see — one permission map, no scattered conditionals.
 */
export const ROUTE_TABLE: readonly RouteMeta[] = [
  {
    path: "/dashboard",
    label: "Dashboard",
    permission: null,
    title: "Dashboard",
    section: "primary",
    skeleton: "dashboard",
    lazy: () => import("@/features/dashboard/CFODashboardPage"),
  },
  {
    path: "/onboarding",
    label: "Getting started",
    permission: null,
    title: "Getting started",
    section: "primary",
    skeleton: "panel",
    lazy: () => import("@/features/onboarding/OnboardingChecklist"),
  },

  {
    path: "/accounts",
    label: "Accounts",
    permission: "account:read",
    title: "Accounts",
    section: "records",
    skeleton: "table",
    lazy: () => import("@/features/accounts/AccountsPage"),
  },
  {
    path: "/accounts/new",
    label: "New account",
    permission: "account:write",
    title: "New account",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/accounts/AccountFormPage"),
  },
  {
    path: "/accounts/:accountId/edit",
    label: "Edit account",
    permission: "account:write",
    title: "Edit account",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/accounts/AccountFormPage"),
  },

  {
    path: "/transactions",
    label: "Transactions",
    permission: "transaction:read",
    title: "Transactions",
    section: "records",
    skeleton: "table",
    lazy: () => import("@/features/transactions/TransactionsPage"),
  },
  {
    path: "/transactions/new",
    label: "New transaction",
    permission: "transaction:write",
    title: "New transaction",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/transactions/TransactionFormPage"),
  },
  {
    path: "/transactions/:transactionId/edit",
    label: "Edit transaction",
    permission: "transaction:write",
    title: "Edit transaction",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/transactions/TransactionFormPage"),
  },

  {
    path: "/categories",
    label: "Categories",
    permission: "category:read",
    title: "Categories",
    section: "records",
    skeleton: "table",
    lazy: () => import("@/features/categories/CategoriesPage"),
  },
  {
    path: "/categories/new",
    label: "New category",
    permission: "category:write",
    title: "New category",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/categories/CategoryFormPage"),
  },
  {
    path: "/categories/:categoryId/edit",
    label: "Edit category",
    permission: "category:write",
    title: "Edit category",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/categories/CategoryFormPage"),
  },

  {
    path: "/budgets",
    label: "Budgets",
    permission: "budget:read",
    title: "Budgets",
    section: "records",
    skeleton: "table",
    lazy: () => import("@/features/budgets/BudgetsPage"),
  },
  {
    path: "/budgets/new",
    label: "New budget",
    permission: "budget:write",
    title: "New budget",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/budgets/BudgetFormPage"),
  },
  {
    path: "/budgets/:budgetId/edit",
    label: "Edit budget",
    permission: "budget:write",
    title: "Edit budget",
    section: "records",
    skeleton: "panel",
    lazy: () => import("@/features/budgets/BudgetFormPage"),
  },

  {
    path: "/reports",
    label: "Reports",
    permission: "report:read",
    title: "Reports",
    section: "insights",
    skeleton: "panel",
    lazy: () => import("@/features/reports/ReportsPage"),
  },
  {
    path: "/exports",
    label: "Exports",
    permission: "export:read",
    title: "Exports",
    section: "insights",
    skeleton: "panel",
    lazy: () => import("@/features/exports/ExportPage"),
  },

  {
    path: "/admin/users",
    label: "Users",
    permission: "admin:users",
    title: "Users",
    section: "admin",
    skeleton: "table",
    lazy: () => import("@/features/admin/UsersPage"),
  },
  {
    path: "/admin/users/new",
    label: "New user",
    permission: "admin:users",
    title: "New user",
    section: "admin",
    skeleton: "panel",
    lazy: () => import("@/features/admin/UserFormPage"),
  },
  {
    path: "/admin/users/:userId/edit",
    label: "Edit user",
    permission: "admin:users",
    title: "Edit user",
    section: "admin",
    skeleton: "panel",
    lazy: () => import("@/features/admin/UserFormPage"),
  },
  {
    path: "/admin/audit-logs",
    label: "Audit logs",
    permission: "audit:read",
    title: "Audit logs",
    section: "admin",
    skeleton: "table",
    lazy: () => import("@/features/admin/AuditLogsPage"),
  },
  {
    path: "/admin/certifications",
    label: "Certifications",
    permission: "admin:users",
    title: "Certifications",
    section: "admin",
    skeleton: "panel",
    lazy: () => import("@/features/admin/CertificationPage"),
  },
];

/** Set of static route-path prefixes used for the post-login `?next=` allowlist. */
const ROUTE_PREFIXES: ReadonlySet<string> = new Set(
  ROUTE_TABLE.map((meta) => meta.path.split("/:")[0]),
);

/** True when `pathname` is (or descends from) a known static route prefix.
 * Used to validate the `?next=` redirect target so user-controlled input is
 * never echoed into a navigable URL unbounded (open-redirect, CWE-601). */
function isRoutePathPrefix(pathname: string): boolean {
  return Array.from(ROUTE_PREFIXES).some(
    (prefix) => pathname === prefix || pathname.startsWith(`${prefix}/`),
  );
}

/** Subset of ROUTE_TABLE a role may open — the single permission map consumed
 * by navigation. Unknown roles resolve to the empty set (fail-closed). */
export function filterRoutesByPermission(role: UserRole | null): RouteMeta[] {
  if (!role) return [];
  return ROUTE_TABLE.filter((meta) => meta.permission === null || can(meta.permission, role));
}

/* ---------------------------------------------------------------------------
 * Focus + document.title management on route transition.
 * ------------------------------------------------------------------------- */

/** Moves keyboard focus to the shell's <main id="main-content"> on every route
 * change so screen-reader/keyboard users are not stranded at the old position. */
function useFocusMain(): void {
  const location = useLocation();
  useEffect(() => {
    document.getElementById("main-content")?.focus();
  }, [location.pathname]);
}

/** Sets `document.title = "<route> — FMS"` for orientation and history. */
function DocumentTitle({ title }: { title: string }): null {
  useEffect(() => {
    document.title = `${title} — FMS`;
  }, [title]);
  return null;
}

/** Pathless AppShell layout. Renders the persistent frame around an <Outlet/>
 * so the shell (sidebar/header) is never remounted across children. */
function AppLayout(): ReactNode {
  useFocusMain();
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}

/* ---------------------------------------------------------------------------
 * Loading, forbidden, and error states.
 * ------------------------------------------------------------------------- */

/**
 * Archetype-matched Suspense fallback (Doherty's Law / CLS ≤ 0.1). Each variant
 * reserves the exact dimensions of the real content (fixed header/filter/card/
 * row/panel heights) so mounting the hydrated page causes zero layout shift.
 * A single shared `role="status"` live region announces the load (Blueprint §8.3).
 */
export function RouteSkeleton({
  label,
  variant,
}: {
  label: string;
  variant: SkeletonArchetype;
}): ReactNode {
  let body: ReactNode;
  if (variant === "dashboard") {
    body = (
      <div className="route-skeleton__cards">
        {Array.from({ length: 6 }).map((_, i) => (
          <div className="route-skeleton__card" key={i}>
            <Skeleton variant="card" announceOnLoad={false} />
          </div>
        ))}
      </div>
    );
  } else if (variant === "table") {
    body = (
      <div className="route-skeleton__rows">
        {Array.from({ length: 6 }).map((_, i) => (
          <div className="route-skeleton__row" key={i}>
            <Skeleton variant="table-row" announceOnLoad={false} />
          </div>
        ))}
      </div>
    );
  } else {
    body = (
      <div className="route-skeleton__panel">
        <Skeleton variant="rect" announceOnLoad={false} />
      </div>
    );
  }

  return (
    <div className="route-skeleton" role="status" aria-live="polite">
      <span className="route-skeleton__status">Loading {label}</span>
      <div aria-hidden="true">
        <div className="route-skeleton__header">
          <Skeleton variant="rect" announceOnLoad={false} />
        </div>
        <div className="route-skeleton__filter">
          <Skeleton variant="rect" announceOnLoad={false} />
        </div>
      </div>
      {body}
    </div>
  );
}

/**
 * 403 / insufficient-permissions state — deliberately a calm, on-brand page,
 * never a blank screen or a technical "Permission Error". Recovery actions are
 * offered: primary returns to the dashboard, secondary offers a history back.
 * A single <h1> + <main> keep it semantic (Blueprint §8.1/§8.3).
 */
export function ForbiddenState({ resourceLabel }: { resourceLabel?: string }): ReactNode {
  useFocusMain();
  const navigate = useNavigate();
  const titleRef = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    titleRef.current?.focus();
  }, []);

  return (
    <main className="forbidden" role="status" aria-live="polite">
      <span className="forbidden__icon" aria-hidden="true">
        <svg className="forbidden__lock" viewBox="0 0 24 24" role="img">
          <rect x="4" y="11" width="16" height="10" rx="2" />
          <path d="M8 11V7a4 4 0 0 1 8 0v4" />
        </svg>
      </span>
      <h1 className="forbidden__title" tabIndex={-1} ref={titleRef}>
        Access required
      </h1>
      <p className="forbidden__body">
        You don&apos;t have access to this page. Ask your administrator if you
        believe this is a mistake.
      </p>
      {resourceLabel ? (
        <p className="forbidden__caption">Requested: {resourceLabel}</p>
      ) : null}
      <div className="forbidden__actions">
        <Button
          variant="primary"
          size="lg"
          onClick={() => navigate("/dashboard", { replace: true })}
        >
          Back to dashboard
        </Button>
        <Button variant="ghost" size="lg" onClick={() => navigate(-1)}>
          Go back
        </Button>
      </div>
    </main>
  );
}

/* ---------------------------------------------------------------------------
 * Fail-closed PermissionGuard + route error boundary.
 * ------------------------------------------------------------------------- */

/**
 * Fail-closed access gate, composed OUTSIDE the Suspense/lazy boundary.
 *
 * Order of checks:
 *   - mfa_pending  → route to the MFA challenge.
 *   - unauthenticated (role null) → route to /login, preserving `?next=` for a
 *     known route prefix only (validated — CWE-601 open-redirect safety).
 *   - permission present but not granted → ForbiddenState (403).
 *   - otherwise → render children (the Suspense + lazy page).
 *
 * Children are never rendered while the session role is unresolved, and an
 * unknown/missing permission denies by default. This is UX-only.
 */
export function PermissionGuard({
  permission,
  children,
}: {
  permission: Permission | null;
  children: ReactNode;
}): ReactNode {
  const status = useSession((s) => s.status);
  const { role, can: canCheck } = usePermission();
  const location = useLocation();

  if (status === "mfa_pending") {
    return <Navigate to="/mfa" replace />;
  }
  if (role === null) {
    // Session is anonymous (or still hydrating) → never render children.
    const next = isRoutePathPrefix(location.pathname)
      ? `?next=${encodeURIComponent(location.pathname)}`
      : "";
    return <Navigate to={`/login${next}`} replace />;
  }
  if (permission !== null && !canCheck(permission)) {
    return <ForbiddenState resourceLabel={location.pathname} />;
  }
  return children;
}

interface RouteErrorBoundaryState {
  hasError: boolean;
}
interface RouteErrorBoundaryProps {
  children: ReactNode;
}

/**
 * Catches lazy-chunk load / network failures so a broken route never shows a
 * blank screen (Nielsen #9) — offers a concrete "Try again" recovery action.
 */
export class RouteErrorBoundary extends Component<
  RouteErrorBoundaryProps,
  RouteErrorBoundaryState
> {
  state: RouteErrorBoundaryState = { hasError: false };

  static getDerivedStateFromError(): RouteErrorBoundaryState {
    return { hasError: true };
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo): void {
    // Log for product support; never surfaced to the user (no stack traces).
    console.error("Route chunk failed to load", error, errorInfo);
  }

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <main className="forbidden" role="alert">
          <h1 className="forbidden__title" tabIndex={-1}>
            Unable to load this page
          </h1>
          <p className="forbidden__body">
            Something went wrong while loading the page. Check your connection
            and try again.
          </p>
          <div className="forbidden__actions">
            <Button variant="primary" size="lg" onClick={() => window.location.reload()}>
              Try again
            </Button>
            <Button variant="ghost" size="lg" onClick={() => window.location.reload()}>
              Reload
            </Button>
          </div>
        </main>
      );
    }
    return this.props.children;
  }
}

/** Wraps a public route in the shared error-boundary + Suspense skeleton. */
function WatchPublicBoundary({ children }: { children: ReactNode }): ReactNode {
  return (
    <RouteErrorBoundary>
      <Suspense fallback={<RouteSkeleton label="Sign in" variant="panel" />}>
        {children}
      </Suspense>
    </RouteErrorBoundary>
  );
}

/** Builds the element for a protected route: guard → title → error → skeleton. */
function protectedElement(meta: RouteMeta): ReactNode {
  const LazyComp = lazy(meta.lazy);
  return (
    <PermissionGuard permission={meta.permission}>
      <DocumentTitle title={meta.title} />
      <RouteErrorBoundary>
        <Suspense fallback={<RouteSkeleton label={meta.label} variant={meta.skeleton} />}>
          <LazyComp />
        </Suspense>
      </RouteErrorBoundary>
    </PermissionGuard>
  );
}

/** Lazy public routes (login/MFA), declared once so the router tree stays flat. */
const LoginPage = lazy(() => import("@/features/auth/LoginPage"));
const MfaChallenge = lazy(() => import("@/features/auth/MfaChallenge"));

/** Public-route gate: an already-authenticated user visiting /login or /mfa is
 * sent back to the dashboard instead of re-prompted for credentials. */
function PublicGate({ children }: { children: ReactNode }): ReactNode {
  const status = useSession((s) => s.status);
  if (status === "authenticated") {
    return <Navigate to="/dashboard" replace />;
  }
  return <WatchPublicBoundary>{children}</WatchPublicBoundary>;
}

/** Catch-all: never silently show a 403 for an unknown URL. Not authenticated →
 * login (deep-link refresh preserves the path); authenticated → dashboard. */
function CatchAll(): ReactNode {
  const status = useSession((s) => s.status);
  const location = useLocation();
  if (status !== "authenticated") {
    const next = isRoutePathPrefix(location.pathname)
      ? `?next=${encodeURIComponent(location.pathname)}`
      : "";
    return <Navigate to={`/login${next}`} replace />;
  }
  return <Navigate to="/dashboard" replace />;
}

/** Application route tree wired into a browser router (data router catalog). */
export function createAppRouter() {
  const protectedChildren: RouteObject[] = ROUTE_TABLE.map((meta) => ({
    path: meta.path,
    element: protectedElement(meta),
  }));

  const routes: RouteObject[] = [
    { path: "/login", element: <PublicGate><LoginPage /></PublicGate> },
    { path: "/mfa", element: <PublicGate><MfaChallenge /></PublicGate> },
    {
      element: <AppLayout />,
      children: protectedChildren,
    },
    { path: "*", element: <CatchAll /> },
  ];

  return createBrowserRouter(routes);
}

/** Default router instance for production mount. */
export const router = createAppRouter();