package domain

import (
	"errors"
	"regexp"
)

var (
	errRoleNameFormat  = errors.New("domain: invalid role name: must match ^[a-z0-9_]{1,64}$")
	errRoleUnknownPerm = errors.New("domain: unknown permission: not in the allowlist catalog")
	errRoleEmptyPerms  = errors.New("domain: a role must grant at least one permission")
	errRoleEmptyName   = errors.New("domain: role name must not be empty")
)

// roleNameRE is the structural allowlist for role names: lowercase letters,
// digits, and underscores, at most 64 characters. Seeded roles use snake_case
// names that double as the idempotent natural key for migration 0005.
var roleNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Role is the Identity & Access aggregate root for RBAC. It carries an
// identity (ID) and marks seeded, non-deletable roles (System=true) so the
// "last admin cannot be removed" protection can be enforced at the use-case
// boundary. Permission policy is data, not code: the grants in Permissions are
// validated against the domain catalog at construction and matched exactly by
// the helpers below.
type Role struct {
	ID          string // UUIDv4 string, assigned by the repository/seed
	Name        string
	Permissions []Permission
	System      bool
}

// NewRole validates and constructs a Role from an untrusted source. It rejects
// an empty or non-allowlisted name, any permission outside the domain catalog,
// and an empty grant set (least privilege, never a role with no defined
// access). Permissions are deduplicated, preserving order. The zero ID is
// intentionally left for the repository/seed to assign so seeding can preserve
// stable UUIDs.
func NewRole(name string, perms []Permission, system bool) (Role, error) {
	if name == "" {
		return Role{}, errRoleEmptyName
	}
	if !roleNameRE.MatchString(name) {
		return Role{}, errRoleNameFormat
	}
	if len(perms) == 0 {
		return Role{}, errRoleEmptyPerms
	}
	perms, err := normalizePermissions(perms)
	if err != nil {
		return Role{}, err
	}
	return Role{Name: name, Permissions: perms, System: system}, nil
}

// Has reports whether the role holds exactly the given permission, matching
// the token by exact string equality against the validated allowlist catalog.
// It fails closed: an unknown, empty, or un-granted token returns false.
func (r Role) Has(p Permission) bool {
	if !validPermission(p) {
		return false
	}
	for _, held := range r.Permissions {
		if held == p {
			return true
		}
	}
	return false
}

// CanAny reports whether the role holds at least one of the given permissions.
// It is the OR-semantics helper for route-level reach (e.g., a reports page
// reachable by either report:read or dashboard:read). An empty argument list
// or an empty grant set yields false (least privilege).
func (r Role) CanAny(ps ...Permission) bool {
	if len(ps) == 0 || len(r.Permissions) == 0 {
		return false
	}
	for _, p := range ps {
		if r.Has(p) {
			return true
		}
	}
	return false
}

// CanAll reports whether the role holds every one of the given permissions. It
// is the conjunctive helper for destructive or administrative operations,
// which must never be gated by a single permissive match. Empty ps returns
// true; an empty grant set returns false.
func (r Role) CanAll(ps ...Permission) bool {
	if len(ps) == 0 || len(r.Permissions) == 0 {
		return len(ps) == 0
	}
	for _, p := range ps {
		if !r.Has(p) {
			return false
		}
	}
	return true
}

// normalizePermissions validates every element against the allowlist catalog
// and removes duplicates, preserving first-occurrence order. It rejects the
// set outright if any token is unknown (fail-fast allowlist, not blocklist).
func normalizePermissions(perms []Permission) ([]Permission, error) {
	seen := make(map[Permission]struct{}, len(perms))
	out := make([]Permission, 0, len(perms))
	for _, p := range perms {
		if !validPermission(p) {
			return nil, errRoleUnknownPerm
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

// Permission is a value object naming a single capability granted to a role.
// Tokens follow the resource:action convention and map exactly to the REST
// surface; the closed set below is the allowlist catalog that is the single
// source of truth for both the seeded grants (migration 0005) and the helpers.
type Permission string

// The full permission catalog. Each token is independent: report:read does not
// imply export:create (export is gated separately, see §9.7), and dashboard
// access does not imply data read permissions.
const (
	PermissionTransactionWrite    Permission = "transaction:write"
	PermissionTransactionRead     Permission = "transaction:read"
	PermissionAccountWrite        Permission = "account:write"
	PermissionAccountRead         Permission = "account:read"
	PermissionCategoryWrite       Permission = "category:write"
	PermissionCategoryRead        Permission = "category:read"
	PermissionBudgetWrite         Permission = "budget:write"
	PermissionBudgetRead          Permission = "budget:read"
	PermissionReportRead          Permission = "report:read"
	PermissionExportCreate        Permission = "export:create"
	PermissionUserManage          Permission = "user:manage"
	PermissionAuditRead           Permission = "audit:read"
	PermissionCertificationManage Permission = "certification:manage"
	PermissionDashboardRead       Permission = "dashboard:read"
	PermissionClosePeriod         Permission = "close:period"
)

// validPermission is the allowlist membership test shared by Has, CanAny, and
// NewRole so the helpers and construction never disagree on what is permitted.
func validPermission(p Permission) bool {
	switch p {
	case PermissionTransactionWrite,
		PermissionTransactionRead,
		PermissionAccountWrite,
		PermissionAccountRead,
		PermissionCategoryWrite,
		PermissionCategoryRead,
		PermissionBudgetWrite,
		PermissionBudgetRead,
		PermissionReportRead,
		PermissionExportCreate,
		PermissionUserManage,
		PermissionAuditRead,
		PermissionCertificationManage,
		PermissionDashboardRead,
		PermissionClosePeriod:
		return true
	default:
		return false
	}
}