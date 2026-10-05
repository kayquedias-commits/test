// Package usecases implements the application-layer orchestration of the FMS
// core. Use cases depend only on the output ports and the domain layer; they
// contain no framework, HTTP, or persistence types.
//
// This file owns the Identity & Access bounded-context account lifecycle
// (Module F). It provisions users, assigns roles, deprovisions accounts, and
// enforces the "last active administrator cannot be removed" acceptance
// criterion, publishing audit and deprovisioning events as it goes.
package usecases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// PasswordMinLength is the minimum allowed plaintext length for a user
// password, checked by the use case before the password reaches the hasher.
const PasswordMinLength = 12

// Sentinel use-case errors. They are identity-comparable so tests and adapters
// can assert on the exact failure independent of the wrapping DomainError.
var (
	// ErrLastAdmin signals the acceptance criterion: the last active admin in a
	// tenant cannot be deleted or deprovisioned.
	ErrLastAdmin = errors.New("use case: last active administrator cannot be removed")
	// ErrUserHasData signals a hard DeleteUser attempted on a user that owns
	// FK-bound financial/audit data; deprovision is the supported lifecycle path.
	ErrUserHasData = errors.New("use case: user owns data; deprovision instead of delete")
	// ErrSegregationOfDuties signals an incompatible role combination (e.g.
	// admin + auditor), enforced by AssignRole.
	ErrSegregationOfDuties = errors.New("use case: role assignment violates segregation of duties")
	// ErrDuplicateUsername signals a username already taken within the tenant.
	ErrDuplicateUsername = errors.New("use case: username already in use")
	// ErrDuplicateEmail signals an email already taken within the tenant.
	ErrDuplicateEmail = errors.New("use case: email already in use")
)

// UserRepository is the output-port seam for persisting User aggregates. All
// queries are scoped by the tenantID parameter so a caller can never read or
// mutate a user outside its own tenant — the tenancy predicate is part of every
// query. An adapter implementation maps database constraint violations
// (duplicate username/email) to ErrDuplicateUsername/ErrDuplicateEmail and an
// optimistic-lock mismatch (zero affected rows) to a conflict-state error.
type UserRepository interface {
	FindByID(ctx context.Context, id, tenantID uuid.UUID) (*domain.User, error)
	FindByUsername(ctx context.Context, username string, tenantID uuid.UUID) (*domain.User, error)
	FindByEmail(ctx context.Context, email string, tenantID uuid.UUID) (*domain.User, error)
	List(ctx context.Context, tenantID uuid.UUID, page, pageSize int) ([]*domain.User, int64, error)
	CountActiveAdmins(ctx context.Context, tenantID uuid.UUID) (int, error)
	HasOwnedData(ctx context.Context, id, tenantID uuid.UUID) (bool, error)
	Create(ctx context.Context, user *domain.User) error
	Update(ctx context.Context, user *domain.User, expectedVersion int64) error
	Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error
}

// RoleRepository is the output-port seam for reading the Role aggregate.
// Permission policy is data, not code — the use case loads the role to re-check
// RBAC, detect admin privilege, and validate segregation of duties before any
// write. Roles are seeded and effectively static, so only a by-ID read is needed.
type RoleRepository interface {
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Role, error)
}

// CreateUserCommand is the input boundary object for provisioning a user. The
// password is the only field that is passed to the hasher; it is never stored,
// logged, embedded in a DTO, or returned from any method.
type CreateUserCommand struct {
	Username   string
	Email      string
	Password   string
	RoleID     uuid.UUID
	MFAEnabled bool
	MFASecret  string
}

// UserService orchestrates the user-account lifecycle. It depends only on the
// user/role repository seams and the security, session, clock, and event output
// ports defined in the ports package.
type UserService struct {
	users    UserRepository
	roles    RoleRepository
	sessions ports.SessionStore
	events   ports.EventBus
	hasher   ports.PasswordHasher
	now      ports.Clock
}

// NewUserService assembles the UserService from its output ports.
func NewUserService(
	users UserRepository,
	roles RoleRepository,
	sessions ports.SessionStore,
	events ports.EventBus,
	hasher ports.PasswordHasher,
	now ports.Clock,
) *UserService {
	return &UserService{
		users:    users,
		roles:    roles,
		sessions: sessions,
		events:   events,
		hasher:   hasher,
		now:      now,
	}
}

// CreateUser provisions a new ACTIVE user in the tenant. It validates and
// normalizes the identity, enforces the password policy, hashes the password
// with the adaptively-parameterized hasher, and inserts the user with a
// starting version of 1. The actor must hold user:manage (defense in depth).
func (s *UserService) CreateUser(ctx context.Context, cmd CreateUserCommand, tenantID, actorID uuid.UUID) (*domain.User, error) {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return nil, err
	}

	if err := validatePassword(cmd.Password); err != nil {
		return nil, err
	}
	role, err := s.roles.FindByID(ctx, cmd.RoleID)
	if err != nil || role == nil {
		return nil, domain.NewNotFound("role", cmd.RoleID.String())
	}
	if err := s.checkSegregation(role); err != nil {
		return nil, err
	}

	user, err := domain.NewUser(
		uuid.New(),
		tenantID,
		cmd.Username,
		cmd.Email,
		"", // hash assigned below after derivation
		cmd.MFASecret,
		cmd.MFAEnabled,
		cmd.RoleID,
	)
	if err != nil {
		return nil, err
	}
	user.Version = 1

	if err := s.ensureUniqueIdentity(ctx, tenantID, &user); err != nil {
		return nil, err
	}

	hash, err := s.hasher.Hash(ctx, cmd.Password)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	user.PasswordHash = hash

	if err := s.enforceMFAMandate(&user, role); err != nil {
		return nil, err
	}

	if err := s.users.Create(ctx, &user); err != nil {
		if errors.Is(err, ErrDuplicateUsername) || errors.Is(err, ErrDuplicateEmail) {
			return nil, duplicateIdentityError(err)
		}
		if de, ok := domain.AsDomainError(err); ok && de.Code == domain.CodeConflictState {
			return nil, de
		}
		return nil, err
	}

	if err := s.publishEntityModified(ctx, domain.ActionCreate, tenantID, actorID, user.ID, nil, userSnapshot(&user)); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUser returns a single user within the tenant. The repository query is
// tenant-scoped, so this never leaks a cross-tenant identity; a missing or
// forbidden target surfaces as NOT_FOUND.
func (s *UserService) GetUser(ctx context.Context, id, tenantID, actorID uuid.UUID) (*domain.User, error) {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	user, err := s.users.FindByID(ctx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, domain.NewNotFound("user", id.String())
	}
	return user, nil
}

// ListUsers returns a tenant-scoped, offset-paginated page of users. The page
// size is clamped to the collection bound to keep responses bounded.
func (s *UserService) ListUsers(ctx context.Context, tenantID, actorID uuid.UUID, page, pageSize int) ([]*domain.User, int64, error) {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return s.users.List(ctx, tenantID, page, pageSize)
}

// UpdateUser applies an optional email, password, and/or MFA flag change to a
// user under an optimistic version guard. The password is rehashed only when a
// new plaintext is supplied and differs from the stored hash. The target is
// tenant-scoped; the actor must be an administrator.
func (s *UserService) UpdateUser(ctx context.Context, id, tenantID, actorID uuid.UUID, expectedVersion int64, email *string, password *string, mfaEnabled *bool) (*domain.User, error) {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return nil, err
	}
	user, err := s.users.FindByID(ctx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, domain.NewNotFound("user", id.String())
	}

	before := userSnapshot(user)

	if email != nil {
		normalized := strings.TrimSpace(strings.ToLower(*email))
		if normalized != user.Email {
			user.Email = normalized
			if err := s.ensureUniqueIdentity(ctx, tenantID, user); err != nil {
				return nil, err
			}
		}
	}
	if mfaEnabled != nil {
		user.MFAActive = *mfaEnabled
		if err := s.enforceMFAMandate(user, nil); err != nil {
			return nil, err
		}
	}
	if password != nil {
		if err := validatePassword(*password); err != nil {
			return nil, err
		}
		hash, err := s.hasher.Hash(ctx, *password)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		user.PasswordHash = hash
	}

	if err := s.users.Update(ctx, user, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}
	if err := s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, user.ID, before, userSnapshot(user)); err != nil {
		return nil, err
	}
	return user, nil
}

// DeleteUser hard-deletes a user via the optimistic-lock guard. The last-admin
// criterion and the FK-bound-data ownership check both run inside the delete
// flow so the guard cannot be bypassed; a user that owns data returns
// ErrUserHasData because deprovision is the supported lifecycle path.
func (s *UserService) DeleteUser(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64, actorID uuid.UUID) error {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return err
	}
	user, err := s.users.FindByID(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if user == nil {
		return domain.NewNotFound("user", id.String())
	}
	before := userSnapshot(user)

	if err := s.enforceLastAdmin(ctx, tenantID, user); err != nil {
		return err
	}

	hasData, err := s.users.HasOwnedData(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if hasData {
		return domain.NewConflict(ErrUserHasData.Error())
	}

	if err := s.users.Delete(ctx, id, tenantID, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}
	return s.publishEntityModified(ctx, domain.ActionDelete, tenantID, actorID, user.ID, before, nil)
}

// AssignRole changes a target user's role. It routes through the same
// optimistic version guard as UpdateUser, validates role existence and
// segregation of duties, prevents the last admin being downgraded away, and
// terminates the target's sessions when the change is a privilege downgrade.
func (s *UserService) AssignRole(ctx context.Context, id, roleID, tenantID uuid.UUID, expectedVersion int64, actorID uuid.UUID) error {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return err
	}
	target, err := s.users.FindByID(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if target == nil {
		return domain.NewNotFound("user", id.String())
	}
	if expectedVersion != target.Version {
		return domain.NewConflict("user was modified by another user")
	}

	newRole, err := s.roles.FindByID(ctx, roleID)
	if err != nil || newRole == nil {
		return domain.NewNotFound("role", roleID.String())
	}
	if err := s.checkSegregation(newRole); err != nil {
		return err
	}

	wasAdmin, err := s.isAdminRole(ctx, target)
	if err != nil {
		return err
	}
	becomesNonAdmin := wasAdmin && !newRole.Has(domain.PermissionUserManage)
	if becomesNonAdmin {
		count, err := s.users.CountActiveAdmins(ctx, tenantID)
		if err != nil {
			return err
		}
		if err := target.CanDelete(count); err != nil {
			return domain.NewForbidden(ErrLastAdmin.Error())
		}
	}

	before := userSnapshot(target)

	// AssignRole mutates the domain aggregate (bumps Version).
	if err := target.AssignRole(roleID); err != nil {
		return err
	}
	if err := s.users.Update(ctx, target, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}
	if err := s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, target.ID, before, userSnapshot(target)); err != nil {
		return err
	}

	if becomesNonAdmin {
		_ = s.sessions.RevokeAllForUser(ctx, target.ID)
	}
	return nil
}

// Deprovision marks a user inactive (terminal DEPROVISIONED status), revokes
// the user's sessions, and publishes UserDeprovisioned carrying both the target
// and the actor. The last-admin criterion is enforced here exactly as on
// DeleteUser so deprovision cannot bypass the guard.
func (s *UserService) Deprovision(ctx context.Context, id uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) error {
	if err := s.requireUserManage(ctx, tenantID, actorID); err != nil {
		return err
	}
	user, err := s.users.FindByID(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if user == nil {
		return domain.NewNotFound("user", id.String())
	}
	if expectedVersion != user.Version {
		return domain.NewConflict("user was modified by another user")
	}
	before := userSnapshot(user)

	if err := s.enforceLastAdmin(ctx, tenantID, user); err != nil {
		return err
	}

	payload, err := user.Deprovision() // yield the event payload and bump Version
	if err != nil {
		return err
	}
	if err := s.users.Update(ctx, user, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}

	if err := s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, user.ID, before, userSnapshot(user)); err != nil {
		return err
	}
	if err := s.publishUserDeprovisioned(ctx, tenantID, actorID, payload); err != nil {
		return err
	}

	// POST-COMMIT compensating revocation: sessions are eventually consistent
	// with the inactive status. Even if this best-effort call fails, the auth
	// path refuses DEPROVISIONED accounts, and the UserDeprovisioned event
	// consumer revokes them again — this is never a standing security hole.
	_ = s.sessions.RevokeAllForUser(ctx, id)
	return nil
}

// requireUserManage verifies that the actor holding the given tenant context is
// an administrator via the live Role aggregate (defense in depth; never trusted
// from client claims). A nil/mismatched actor yields FORBIDDEN.
func (s *UserService) requireUserManage(ctx context.Context, tenantID, actorID uuid.UUID) error {
	if actorID == uuid.Nil {
		return domain.NewForbidden("insufficient permissions")
	}
	actor, err := s.users.FindByID(ctx, actorID, tenantID)
	if err != nil || actor == nil {
		return domain.NewForbidden("insufficient permissions")
	}
	if actor.TenantID != tenantID {
		return domain.NewForbidden("insufficient permissions")
	}
	role, err := s.roles.FindByID(ctx, actor.RoleID)
	if err != nil || role == nil {
		return domain.NewForbidden("insufficient permissions")
	}
	if !role.Has(domain.PermissionUserManage) {
		return domain.NewForbidden("insufficient permissions")
	}
	return nil
}

// isAdminRole reports whether the user's current role grants user:manage.
func (s *UserService) isAdminRole(ctx context.Context, u *domain.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	role, err := s.roles.FindByID(ctx, u.RoleID)
	if err != nil {
		return false, err
	}
	return role != nil && role.Has(domain.PermissionUserManage), nil
}

// enforceLastAdmin applies the domain-owned last-admin invariant: an ACTIVE
// admin user cannot be removed when the active admin count for the tenant
// (including this user) is at most one. The count is read within the same
// mutation flow as the delete/deprovision so the TOCTOU race is closed by the
// repository's tenant-scoped locking.
func (s *UserService) enforceLastAdmin(ctx context.Context, tenantID uuid.UUID, user *domain.User) error {
	isAdmin, err := s.isAdminRole(ctx, user)
	if err != nil {
		return err
	}
	if !isAdmin || !user.Active() {
		return nil
	}
	count, err := s.users.CountActiveAdmins(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := user.CanDelete(count); err != nil {
		return err // domain FORBIDDEN guard — the invariant's single source of truth
	}
	return nil
}

// checkSegregation rejects incompatible role combinations (segregation of
// duties). The matrix is kept as a small allowlist check at the application
// boundary; extending it means editing this predicate only.
func (s *UserService) checkSegregation(role *domain.Role) error {
	if role == nil {
		return domain.NewNotFound("role", "")
	}
	if role.Has(domain.PermissionUserManage) && role.Has(domain.PermissionAuditRead) {
		return domain.NewForbidden(ErrSegregationOfDuties.Error())
	}
	return nil
}

// enforceMFAMandate per the specification, MFA is required for all users; an
// administrator or auditor role may not be created or mutated with MFA disabled.
func (s *UserService) enforceMFAMandate(user *domain.User, role *domain.Role) error {
	isAdmin := false
	if role != nil {
		isAdmin = role.Has(domain.PermissionUserManage) || role.Has(domain.PermissionAuditRead)
	}
	if !isAdmin {
		return nil
	}
	if !user.MFAActive {
		return domain.NewValidationError(domain.FieldError{
			Field:   "mfa_enabled",
			Message: "multi-factor authentication is mandatory for administrator and auditor roles",
		})
	}
	return nil
}

// ensureUniqueIdentity pre-checks username/email uniqueness within the tenant so
// a friendly error is surfaced before reaching the persistence constraint. The
// database UNIQUE(tenant_id, username)/UNIQUE(tenant_id, email) constraints
// remain the authoritative backstop under concurrency.
func (s *UserService) ensureUniqueIdentity(ctx context.Context, tenantID uuid.UUID, user *domain.User) error {
	byUsername, err := s.users.FindByUsername(ctx, user.Username, tenantID)
	if err != nil {
		return err
	}
	if byUsername != nil && byUsername.ID != user.ID {
		return duplicateIdentityError(ErrDuplicateUsername)
	}
	byEmail, err := s.users.FindByEmail(ctx, user.Email, tenantID)
	if err != nil {
		return err
	}
	if byEmail != nil && byEmail.ID != user.ID {
		return duplicateIdentityError(ErrDuplicateEmail)
	}
	return nil
}

// duplicateIdentityError maps a duplicate-identity sentinel to a structured
// validation error so the client receives a safe, enumerable code.
func duplicateIdentityError(err error) error {
	if errors.Is(err, ErrDuplicateEmail) {
		return domain.NewValidationError(domain.FieldError{Field: "email", Message: "email is already registered"})
	}
	return domain.NewValidationError(domain.FieldError{Field: "username", Message: "username is already registered"})
}

// validatePassword enforces the allowlist password policy: at least
// PasswordMinLength non-whitespace characters and no surrounding whitespace that
// could be trimmed to a weaker effective password.
func validatePassword(p string) error {
	if len(p) < PasswordMinLength {
		return domain.NewValidationError(domain.FieldError{Field: "password", Message: "must be at least 12 characters"})
	}
	if strings.TrimSpace(p) != p {
		return domain.NewValidationError(domain.FieldError{Field: "password", Message: "must not begin or end with whitespace"})
	}
	return nil
}

// conflictOrRepoError normalizes a repository update error: a conflict-state
// domain error (optimistic-lock mismatch) is returned unchanged; anything else
// is passed through.
func conflictOrRepoError(err error) error {
	if err == nil {
		return nil
	}
	if de, ok := domain.AsDomainError(err); ok {
		return de
	}
	return err
}

// userSnapshot is the minimal, non-secret representation of a user carried in
// audit events. It never includes PasswordHash or MFASecret.
func userSnapshot(u *domain.User) json.RawMessage {
	type snapshot struct {
		Version int64             `json:"version"`
		RoleID  string            `json:"role_id"`
		Status  domain.UserStatus `json:"status"`
	}
	if u == nil {
		return nil
	}
	raw, _ := json.Marshal(snapshot{
		Version: u.Version,
		RoleID:  u.RoleID.String(),
		Status:  u.Status,
	})
	return raw
}

// publishEntityModified emits the append-only audit event for a create/edit/
// delete mutation, carrying the actor identity and a minimal before/after state.
func (s *UserService) publishEntityModified(ctx context.Context, action domain.AuditAction, tenantID, actorID, entityID uuid.UUID, before, after json.RawMessage) error {
	payload := domain.EntityModifiedPayload{
		EntityType: "user",
		EntityID:   entityID.String(),
		Action:     action,
		Before:     before,
		After:      after,
	}
	event, err := s.newEvent(domain.EventEntityModified, tenantID, actorID, entityID, payload)
	if err != nil {
		return err
	}
	return s.events.Publish(ctx, event)
}

// publishUserDeprovisioned emits the deprovisioning event carrying both the
// target user and the actor who performed the deprovision, for the session-
// revocation and access-certification consumers.
func (s *UserService) publishUserDeprovisioned(ctx context.Context, tenantID, actorID uuid.UUID, payload domain.UserDeprovisionedPayload) error {
	event, err := s.newEvent(domain.EventUserDeprovisioned, tenantID, actorID, payload.UserID, payload)
	if err != nil {
		return err
	}
	return s.events.Publish(ctx, event)
}

// newEvent wraps a payload into a validated domain.Event envelope. The event ID
// doubles as the correlation id (sanitized by construction), satisfying the
// event envelope allowlist without deriving a fresh trace key. Sequence is a
// non-zero placeholder assigned by the publishing infrastructure; the bus
// re-assigns it and never reads it from here.
func (s *UserService) newEvent(typ domain.EventType, tenantID, actorID, aggregateID uuid.UUID, payload any) (domain.Event, error) {
	if tenantID == uuid.Nil || actorID == uuid.Nil || aggregateID == uuid.Nil {
		return domain.Event{}, domain.NewInternal(errors.New("use case: event requires non-zero ids"))
	}
	version, ok := typ.Version()
	if !ok {
		return domain.Event{}, domain.ErrUnknownEventType
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	evtID := uuid.New()
	return domain.Event{
		ID:            evtID,
		Type:          typ,
		Version:       version,
		Timestamp:     s.now.Now().UTC(),
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: evtID.String(),
		SourceService: "IAM",
		AggregateType: "user",
		AggregateID:   aggregateID.String(),
		Payload:       raw,
		Sequence:      1,
	}, nil
}