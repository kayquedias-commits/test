/**
 * Permission-filtered primary navigation (Blueprint §8.1).
 *
 * Render surfaces the routes the current session role may actually open, so no
 * hidden dead-ends are offered. The item list is driven entirely by a single
 * permission map (`usePermission` -> `app/permissions.ts` -> server-provided
 * role); there are no per-role `if` branches in this file.
 *
 * This is a UI-only, least-privilege gate: it decides which destination may be
 * rendered. It is NOT a security control - the backend enforces the same
 * matrix at the resource boundary and rejects unauthorized requests regardless
 * of what this component renders.
 *
 * The route table is colocated here (a config seam for a future `router.tsx`;
 * it is deliberately NOT imported from the router so nav can render even when
 * route metadata is still being established). Labels and paths are compile-time
 * string literals; labels are always rendered as React text nodes (never as
 * markup) and paths are validated against `javascript:`/`data:`/`vbscript:`
 * before use, per the security review.
 */
import React, { useEffect, useMemo, useRef, useState } from "react";
import { NavLink } from "react-router-dom";
import { usePermission } from "../../hooks/usePermission";
import { useSession } from "../../app/stores/session";
import type { Permission } from "../../services/api/types";

/** Always-visible destinations are grouped to hold nav options under Hick's-law
 * limits; a null label means the group renders as a top-level item without a
 * section header (Dashboard). */
interface NavItem {
  /** Static route path (constant; validated below, never built from user input). */
  path: string;
  /** Display label, rendered as a text node only. */
  label: string;
  /** Permission required to render; null means public to any authenticated role. */
  permission: Permission | null;
  /** Icon key mapped to a hardcoded inline SVG (see ICONS). */
  icon: string;
}

interface NavGroup {
  id: string;
  /** Section header label; null = no header (top-level destination). */
  label: string | null;
  items: NavItem[];
}

const NAV_GROUPS: NavGroup[] = [
  {
    id: "dashboard",
    label: null,
    items: [{ path: "/dashboard", label: "Dashboard", permission: null, icon: "dashboard" }],
  },
  {
    id: "operations",
    label: "Operations",
    items: [
      { path: "/accounts", label: "Accounts", permission: "account:read", icon: "accounts" },
      { path: "/transactions", label: "Transactions", permission: "transaction:read", icon: "transactions" },
      { path: "/categories", label: "Categories", permission: "category:read", icon: "categories" },
      { path: "/budgets", label: "Budgets", permission: "budget:read", icon: "budgets" },
    ],
  },
  {
    id: "reporting",
    label: "Reporting",
    items: [
      { path: "/reports", label: "Reports", permission: "report:read", icon: "reports" },
      { path: "/exports", label: "Exports", permission: "export:read", icon: "exports" },
    ],
  },
  {
    id: "administration",
    label: "Administration",
    items: [
      { path: "/admin/users", label: "Users", permission: "admin:users", icon: "users" },
      { path: "/admin/audit-logs", label: "Audit Logs", permission: "audit:read", icon: "audit" },
    ],
  },
];

// Escape hatch for schemes that must never reach an href. The table above is
// static, but this is defense-in-depth: a future editor error cannot emit a
// dangerous scheme to the DOM.
const UNSAFE_SCHEME = /^(javascript|data|vbscript):/i;

const isSafePath = (path: string): boolean => !UNSAFE_SCHEME.test(path);

/** Hardcoded inline icons (20px, aria-hidden). Keys not present fall back to a
 * neutral dot so a config typo never crashes the nav. */
const ICONS: Record<string, React.ReactNode> = {
  dashboard: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <rect x="2.5" y="2.5" width="6" height="8" rx="1" />
      <rect x="11.5" y="2.5" width="6" height="5" rx="1" />
      <rect x="11.5" y="10.5" width="6" height="7" rx="1" />
      <rect x="2.5" y="13.5" width="6" height="4" rx="1" />
    </svg>
  ),
  accounts: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <rect x="2.5" y="4" width="15" height="12" rx="1.5" />
      <path d="M2.5 8h15" />
      <path d="M6 12h3" />
    </svg>
  ),
  transactions: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M3 6h14M3 10h14M3 14h9" />
      <path d="M14 12.5l2.5 2.5 2.5-2.5" />
    </svg>
  ),
  categories: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <rect x="3" y="3" width="5.5" height="5.5" rx="1" />
      <rect x="11.5" y="3" width="5.5" height="5.5" rx="1" />
      <rect x="3" y="11.5" width="5.5" height="5.5" rx="1" />
      <rect x="11.5" y="11.5" width="5.5" height="5.5" rx="1" />
    </svg>
  ),
  budgets: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M10 2.5a7.5 7.5 0 1 1 0 15 7.5 7.5 0 0 1 0-15z" />
      <path d="M10 5.5v9M7 7.5h6" />
    </svg>
  ),
  reports: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M3 3h14v14H3z" />
      <path d="M6 14l3-4 2 2 3-5" />
    </svg>
  ),
  exports: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M10 3v8M7 8l3 3 3-3" />
      <path d="M4 13v3h12v-3" />
    </svg>
  ),
  users: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <circle cx="7.5" cy="7" r="2.5" />
      <path d="M2.5 16c0-2.8 2.2-5 5-5s5 2.2 5 5" />
      <path d="M13.5 4.8a2.5 2.5 0 0 1 0 4.9M14.5 11.3c2.2.5 3.5 2.5 3.5 4.7" />
    </svg>
  ),
  audit: (
    <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M4 4h12v12H4z" />
      <circle cx="10" cy="10" r="1" />
    </svg>
  ),
};

const fallbackIcon = (
  <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
    <circle cx="10" cy="10" r="6" />
  </svg>
);

/**
 * Permission-filtered primary navigation.
 *
 * Renders the accessible destinations for the current role into four groups;
 * hides whole section headers when the role has no item in that group; keeps a
 * state-driven mobile disclosure; and always leaves the user a Sign out affordance.
 */
export function Navigation() {
  const { role, can } = usePermission();
  const logout = useSession((state) => state.logout);
  const isLoggingOut = useSession((state) => state.isLoggingOut);

  const [mobileOpen, setMobileOpen] = useState(false);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  // Fail-closed: while role is unresolved (unauthenticated or hydrating) every
  // item is gated out; the skeleton below holds layout so nothing flashes.
  const groups = useMemo(() => {
    if (role === null) return [];
    return NAV_GROUPS.map((group) => ({
      ...group,
      // A null permission is public to any authenticated role (never implicitly
      // blocked); otherwise rely on the hook's O(1) set lookup (INP-safe).
      items: group.items.filter(
        (item) => item.permission === null || can(item.permission),
      ),
      // Drop empty section headers entirely (Hick's Law / Miller: a role with no
      // Administration item must never see a ghost "Administration" header).
    })).filter((group) => group.label === null || group.items.length > 0);
  }, [role, can]);

  // Mobile disclosure: focus the first link on open, restore focus to the
  // toggle when it closes so keyboard users are never left stranded.
  useEffect(() => {
    if (!mobileOpen) return;
    const first = listRef.current?.querySelector<HTMLAnchorElement>("a");
    first?.focus();
  }, [mobileOpen]);

  // Escape closes the disclosure (it is a disclosure, not a modal - no trap).
  useEffect(() => {
    if (!mobileOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMobileOpen(false);
        toggleRef.current?.focus();
      }
    };
    const onClick = (e: MouseEvent) => {
      if (
        mobileOpen &&
        !listRef.current?.contains(e.target as Node) &&
        !toggleRef.current?.contains(e.target as Node)
      ) {
        setMobileOpen(false);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onClick);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onClick);
    };
  }, [mobileOpen]);

  useEffect(() => {
    ensureNavStyles();
  }, []);

  // While the session role is unresolved, render a reserved skeleton so
  // hydration never causes layout shift (CLS target) nor flashes privileged links.
  if (role === null) {
    return <nav aria-label="Primary" className="fms-nav fms-nav--skeleton" />;
  }

  // No permitted destinations for the current role (empty nav): show an
  // explicit, instructive empty state instead of a blank rail (a11y/UX).
  const emptyAccess = groups.length === 0;

  return (
    <nav aria-label="Primary" className="fms-nav">
      <button
        type="button"
        ref={toggleRef}
        className="fms-nav__toggle"
        aria-expanded={mobileOpen}
        aria-controls="fms-nav-list"
        aria-label="Toggle navigation menu"
        onClick={() => setMobileOpen((o) => !o)}
      >
        <span className="fms-nav__toggle-bars" aria-hidden="true" />
        <span className="fms-nav__toggle-label">Menu</span>
      </button>

      <ul
        id="fms-nav-list"
        ref={listRef}
        className={`fms-nav__list${mobileOpen ? " is-open" : ""}`}
        hidden={!mobileOpen}
      >
        {emptyAccess && (
          <li className="fms-nav__empty">You do not have access to any sections.</li>
        )}
        {!emptyAccess &&
          groups.map((group) => {
            const header = group.label !== null;
            return (
              <React.Fragment key={group.id}>
                {header && <li className="fms-nav__sect">{group.label}</li>}
                {group.items.map((item) => {
                  if (!isSafePath(item.path)) return null;
                  return (
                    <li key={item.path}>
                      <NavLink to={item.path} className="fms-nav__link">
                        <span className="fms-nav__icon">{ICONS[item.icon] ?? fallbackIcon}</span>
                        <span className="fms-nav__label">{item.label}</span>
                      </NavLink>
                    </li>
                  );
                })}
              </React.Fragment>
            );
          })}

        <li className="fms-nav__sep" aria-hidden="true" />

        <li>
          <button
            type="button"
            className="fms-nav__logout"
            onClick={() => void logout()}
            disabled={isLoggingOut}
          >
            <span className="fms-nav__icon">{ICONS.exports ?? fallbackIcon}</span>
            <span className="fms-nav__label">
              {isLoggingOut ? "Signing out…" : "Sign out"}
            </span>
          </button>
        </li>
      </ul>
    </nav>
  );
}

/** Idempotently inject the colocated stylesheet for this component. */
let navStylesLoaded = false;
function ensureNavStyles() {
  if (navStylesLoaded) return;
  navStylesLoaded = true;
  if (typeof document === "undefined") return;

  const style = document.createElement("style");
  style.id = "fms-nav-styles";
  style.textContent = `
    .fms-nav {
      --nav-width: 256px;
      --nav-padding-x: var(--space-3, 12px);
      --nav-sect-gap: var(--space-5, 20px);
      --nav-link-text: var(--text-sm, 14px);
      --nav-link-py: var(--space-2, 8px);
      --nav-link-px: var(--space-3, 12px);
      --nav-link-radius: var(--radius-md, 8px);
      --nav-icon-size: 20px;
      display: flex;
      flex-direction: column;
      position: sticky;
      top: 0;
      height: 100vh;
      box-sizing: border-box;
      width: var(--nav-width);
      max-width: 100vw;
      padding: var(--space-3, 12px) var(--nav-padding-x);
      overflow-y: auto;
      background: var(--color-surface, #ffffff);
      color: var(--color-text-base, #1a1a1a);
    }
    .fms-nav__list {
      flex: 1 1 auto;
      display: flex;
      flex-direction: column;
      gap: var(--nav-sect-gap);
      margin: 0;
      padding: 0;
      list-style: none;
      min-width: 0;
    }
    .fms-nav__sect {
      font-size: var(--text-xs, 12px);
      font-weight: var(--font-medium, 500);
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--color-text-subtle, #6b7280);
      margin: 0 0 var(--space-2, 8px);
      padding-left: var(--nav-link-px);
      line-height: var(--leading-tight, 1.25);
      min-height: 44px;
      display: flex;
      align-items: center;
    }
    .fms-nav__link {
      display: flex;
      align-items: center;
      gap: var(--space-3, 12px);
      min-height: 44px;
      padding: var(--nav-link-py) var(--nav-link-px);
      border-radius: var(--nav-link-radius);
      color: var(--color-text-base, #1a1a1a);
      font-size: var(--nav-link-text);
      line-height: var(--leading-tight, 1.25);
      text-decoration: none;
      white-space: nowrap;
      transition: background-color 150ms ease-out, color 150ms ease-out, box-shadow 150ms ease-out;
    }
    .fms-nav__link:hover {
      background: var(--color-surface-hover, #f3f4f6);
      color: var(--color-text-strong, #111827);
    }
    .fms-nav__link:focus-visible {
      outline: 2px solid var(--color-primary-500, #4285f4);
      outline-offset: 2px;
      box-shadow: 0 0 0 2px var(--color-focus-ring, rgba(66, 133, 244, 0.4));
    }
    .fms-nav__link[aria-current="page"] {
      background: var(--color-primary-50, #eef4fd);
      color: var(--color-primary-700, #1a5fb4);
      font-weight: var(--font-medium, 500);
      transition: background-color 200ms ease-out, color 200ms ease-out;
    }
    .fms-nav__icon {
      width: var(--nav-icon-size);
      height: var(--nav-icon-size);
      display: inline-flex;
      align-items: center;
      justify-content: center;
      flex: 0 0 auto;
    }
    .fms-nav__icon > svg {
      display: block;
    }
    .fms-nav__label {
      overflow: hidden;
    }
    .fms-nav__empty {
      font-size: var(--nav-link-text);
      color: var(--color-text-subtle, #6b7280);
      padding: var(--nav-link-py) var(--nav-link-px);
      min-height: 44px;
      display: flex;
      align-items: center;
    }
    .fms-nav__sep {
      border-top: 1px solid var(--color-border, #e5e7eb);
      margin: var(--space-2, 8px) 0;
      min-height: 0;
    }
    .fms-nav__logout {
      display: flex;
      align-items: center;
      gap: var(--space-3, 12px);
      width: 100%;
      min-height: 44px;
      padding: var(--nav-link-py) var(--nav-link-px);
      margin-top: var(--space-2, 8px);
      border: 0;
      border-radius: var(--nav-link-radius);
      background: transparent;
      color: var(--color-text-subtle, #6b7280);
      font-size: var(--nav-link-text);
      font-family: inherit;
      line-height: var(--leading-tight, 1.25);
      text-align: left;
      cursor: pointer;
      transition: background-color 150ms ease-out, color 150ms ease-out;
    }
    .fms-nav__logout:hover:not(:disabled) {
      background: var(--color-surface-hover, #f3f4f6);
      color: var(--color-text-strong, #111827);
    }
    .fms-nav__logout:focus-visible {
      outline: 2px solid var(--color-primary-500, #4285f4);
      outline-offset: 2px;
      box-shadow: 0 0 0 2px var(--color-focus-ring, rgba(66, 133, 244, 0.4));
    }
    .fms-nav__logout:active:not(:disabled) {
      transform: translateY(1px);
    }
    .fms-nav__logout:disabled {
      opacity: 0.6;
      cursor: not-allowed;
    }
    .fms-nav__toggle {
      display: none;
    }

    /* Mobile (<768px): collapse into a disclosure above the list. */
    @media (max-width: 767px) {
      .fms-nav {
        position: sticky;
        top: 0;
        width: 100%;
        height: auto;
        max-height: 100vh;
        padding: var(--space-2, 8px);
        overflow: visible;
      }
      .fms-nav__toggle {
        display: flex;
        align-items: center;
        gap: var(--space-3, 12px);
        min-height: 44px;
        padding: var(--space-2, 8px) var(--space-3, 12px);
        border: 0;
        border-radius: var(--nav-link-radius);
        background: transparent;
        color: var(--color-text-base, #1a1a1a);
        font-size: var(--nav-link-text);
        font-family: inherit;
        font-weight: var(--font-medium, 500);
        cursor: pointer;
        transition: background-color 150ms ease-out;
      }
      .fms-nav__toggle:hover {
        background: var(--color-surface-hover, #f3f4f6);
      }
      .fms-nav__toggle:focus-visible {
        outline: 2px solid var(--color-primary-500, #4285f4);
        outline-offset: 2px;
        box-shadow: 0 0 0 2px var(--color-focus-ring, rgba(66, 133, 244, 0.4));
      }
      .fms-nav__toggle-bars {
        width: 18px;
        height: 2px;
        background: currentColor;
        position: relative;
      }
      .fms-nav__toggle-bars::before,
      .fms-nav__toggle-bars::after {
        content: "";
        position: absolute;
        left: 0;
        width: 18px;
        height: 2px;
        background: currentColor;
      }
      .fms-nav__toggle-bars::before { top: -6px; }
      .fms-nav__toggle-bars::after { top: 6px; }
      .fms-nav__list {
        gap: var(--nav-sect-gap);
        padding-top: var(--space-2, 8px);
        margin-top: var(--space-2, 8px);
        border-top: 1px solid var(--color-border, #e5e7eb);
        opacity: 0;
        transform: translateY(-8px);
        transition: opacity 150ms ease-in, transform 150ms ease-in;
        pointer-events: none;
      }
      .fms-nav__list.is-open {
        opacity: 1;
        transform: translateY(0);
        transition: opacity 200ms ease-out, transform 200ms ease-out;
        pointer-events: auto;
      }
    }

    /* Tablet icon rail (768-1024px): labels visually hidden but accessible. */
    @media (min-width: 768px) and (max-width: 1024px) {
      .fms-nav {
        width: 64px;
        padding: var(--space-3, 12px) var(--space-2, 8px);
      }
      .fms-nav__sect,
      .fms-nav__label,
      .fms-nav__toggle-label {
        position: absolute;
        width: 1px;
        height: 1px;
        margin: -1px;
        padding: 0;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
        border: 0;
      }
      .fms-nav__link,
      .fms-nav__logout {
        justify-content: center;
        padding-left: var(--space-1, 4px);
        padding-right: var(--space-1, 4px);
      }
      .fms-nav__link[aria-current="page"] .fms-nav__icon {
        color: var(--color-primary-700, #1a5fb4);
      }
    }

    @media (prefers-reduced-motion: reduce) {
      .fms-nav__link,
      .fms-nav__logout,
      .fms-nav__list {
        transition: none;
      }
    }
  `;
  document.head.appendChild(style);
}