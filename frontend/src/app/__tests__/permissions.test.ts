/**
 * Permission-map gating tests (Blueprint §8.1 / §9.7).
 *
 * This suite validates UI rendering gating only. It is NOT a security
 * enforcement boundary: the backend re-validates the same role matrix at the
 * API/resource layer (§7.8). `can` performs role-wide grant checks; per-resource
 * ownership (cross-tenant) is applied by consumers and the backend, not by the
 * `can` resolver itself.
 *
 * The module is deny-by-default: any role or permission not explicitly granted
 * resolves to `false`. This is what lets the UI HIDE unauthorized affordances
 * (never render disabled controls) for read-only roles.
 */
import { describe, it, expect } from "vitest";
import type { Permission, UserRole } from "../../services/api/types";
import { can, permissionsFor } from "../permissions";

/** Tokens no read-only role may hold. Used to prove least-privilege denial. */
const WRITE_TOKENS: Permission[] = [
  "account:write",
  "transaction:write",
  "category:write",
  "budget:write",
  "export:create",
  "admin:users",
];

describe("can", () => {
  it("grants ADMIN every permission, including user management", () => {
    const allPermissions: Permission[] = [
      "account:read",
      "account:write",
      "transaction:read",
      "transaction:write",
      "category:read",
      "category:write",
      "budget:read",
      "budget:write",
      "report:read",
      "export:read",
      "export:create",
      "admin:users",
      "audit:read",
    ];
    for (const permission of allPermissions) {
      expect(can(permission, "ADMIN")).toBe(true);
    }
  });

  it("grants FINANCE_MANAGER full access except user management", () => {
    expect(can("transaction:write", "FINANCE_MANAGER")).toBe(true);
    expect(can("export:create", "FINANCE_MANAGER")).toBe(true);
    expect(can("admin:users", "FINANCE_MANAGER")).toBe(false);
  });

  it("grants ACCOUNTANT ledger access without audit or admin", () => {
    expect(can("transaction:write", "ACCOUNTANT")).toBe(true);
    expect(can("audit:read", "ACCOUNTANT")).toBe(false);
    expect(can("admin:users", "ACCOUNTANT")).toBe(false);
  });

  it("grants AUDITOR read + audit access but denies writes and export", () => {
    expect(can("audit:read", "AUDITOR")).toBe(true);
    expect(can("report:read", "AUDITOR")).toBe(true);
    expect(can("export:create", "AUDITOR")).toBe(false);
    expect(can("transaction:write", "AUDITOR")).toBe(false);
    for (const write of WRITE_TOKENS) {
      expect(can(write, "AUDITOR")).toBe(false);
    }
  });

  it("denies VIEW_ONLY every write and export token (least privilege)", () => {
    expect(can("export:create", "VIEW_ONLY")).toBe(false);
    expect(can("transaction:write", "VIEW_ONLY")).toBe(false);
    expect(can("category:write", "VIEW_ONLY")).toBe(false);
    for (const write of WRITE_TOKENS) {
      expect(can(write, "VIEW_ONLY")).toBe(false);
    }
  });

  it("keeps legitimate read access for VIEW_ONLY", () => {
    expect(can("transaction:read", "VIEW_ONLY")).toBe(true);
    expect(can("report:read", "VIEW_ONLY")).toBe(true);
    expect(can("export:read", "VIEW_ONLY")).toBe(true);
  });

  it("grants BUDGET_OWNER budget control without transaction or export writes", () => {
    expect(can("budget:write", "BUDGET_OWNER")).toBe(true);
    expect(can("transaction:write", "BUDGET_OWNER")).toBe(false);
    expect(can("export:create", "BUDGET_OWNER")).toBe(false);
  });

  it("denies EMPLOYEE account access and any write/export token", () => {
    expect(can("account:read", "EMPLOYEE")).toBe(false);
    expect(can("category:write", "EMPLOYEE")).toBe(false);
    expect(can("export:create", "EMPLOYEE")).toBe(false);
  });

  it("resolves unknown roles to zero grants (fail-closed)", () => {
    expect(can("transaction:read", "ROOT" as UserRole)).toBe(false);
    expect(can("user:manage", "UNKNOWN" as UserRole)).toBe(false);
    expect(can("audit:read", "NONE" as UserRole)).toBe(false);
  });

  it("accepts a resource argument without throwing (ownership is not resolved here)", () => {
    // `can` treats every grant as role-wide; cross-tenant rejection is the
    // consumer/backend responsibility (Blackboard §7.8), not an in-module test.
    expect(can("audit:read", "AUDITOR", { ownerId: "tenant-other", id: "x" })).toBe(true);
  });
});

describe("permissionsFor", () => {
  it("returns no permissions for unknown roles", () => {
    expect(permissionsFor("ROOT" as UserRole)).toEqual([]);
  });

  it("returns the exact VIEW_ONLY read-only set", () => {
    expect(permissionsFor("VIEW_ONLY")).toEqual([
      "account:read",
      "transaction:read",
      "category:read",
      "budget:read",
      "report:read",
      "export:read",
    ]);
  });

  it("returns the exact AUDITOR set without export:create or writes", () => {
    expect(permissionsFor("AUDITOR")).toEqual([
      "account:read",
      "transaction:read",
      "category:read",
      "budget:read",
      "report:read",
      "export:read",
      "audit:read",
    ]);
  });

  it("returns every permission for ADMIN", () => {
    expect(permissionsFor("ADMIN")).toHaveLength(13);
    expect(permissionsFor("ADMIN")).toContain("admin:users");
    expect(permissionsFor("ADMIN")).toContain("audit:read");
    expect(permissionsFor("ADMIN")).toContain("export:create");
  });
});