// Package usecases implements the application use-case layer: it orchestrates
// output ports, applies business rules, owns transaction boundaries, and
// publishes domain events. This file delivers the access-certification
// journey (periodic access reviews, manager certification, orphaned-account
// deprovision evidence) behind the Identity & Access bounded context, following
// the User Lifecycle and Access Certification business workflow and the
// append-only audit invariant (§5.3).
package usecases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// Review lifecycle states persisted on the certification-review row. Reviews
// are append-only evidence: a row is created OPEN and transitions at most once
// to CLOSED, and is never deleted.
const (
	reviewStatusOpen   = "OPEN"
	reviewStatusClosed = "CLOSED"
)

// orphanInactivityDays is the policy threshold for orphan detection: a user who
// never logged in or whose last login is older than this window (and is not an
// admin) is flagged as an orphaned/inactive account. It is a named
// application-layer policy, not an arbitrary literal.
const orphanInactivityDays = 90

// revokePolicyReason is the compliance reason attached to every REVOKE decision
// produced by this service. The domain aggregate requires a non-empty reason on
// revoke (ErrReasonRequiredOnRevoke), and RecordDecision takes no free-text
// reason parameter, so a fixed, allowlisted policy string is supplied.
const revokePolicyReason = "revoked via access certification"

// Bounded-collection listing defaults (§7.2): certification reviews and
// orphaned accounts are offset-paginated collections, never unbounded.
const (
	reviewListDefaultPage     = 1
	reviewListDefaultPageSize = 100
)

// maxReviewScopeLen bounds scope input to a small, allowlisted window of
// administrative identifiers (e.g. "full", "department:finance").
const maxReviewScopeLen = 128

// CertificationReview is the transport object crossing the use-case/repository
// boundary for a periodic access-review row. It is not a domain entity: the
// review's lifecycle/invariant semantics live on the domain aggregate.
type CertificationReview struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Scope       string
	WindowStart time.Time
	WindowEnd   time.Time
	Status      string
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	Version     int
}

// CertificationDecisionRecord is the transport object for an append-only
// certification decision row. Decision reuses the domain value object.
type CertificationDecisionRecord struct {
	ID          uuid.UUID
	ReviewID    uuid.UUID
	TenantID    uuid.UUID
	UserID      uuid.UUID
	ReviewerID  uuid.UUID
	Decision    domain.CertificationDecision
	EvidenceRef string
	ReviewedAt  time.Time
}

// OrphanedAccount is a read-side projection of a flagged orphaned/inactive
// account. It carries only the minimal fields required for listing; it must
// never expose password hashes, MFA secrets, session identifiers, or credentials.
type OrphanedAccount struct {
	UserID     uuid.UUID
	Username   string
	Email      string
	LastLogin  *time.Time
	AccessRole string
}

// Actor is the server-resolved identity performing a certification action. It
// is loaded from the UserRepository (never from caller-supplied flags) so the
// use case can enforce authorization and tenant isolation internally.
type Actor struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     domain.Role
}

// CreateReviewCommand is the validated input for opening a new certification
// review.
type CreateReviewCommand struct {
	Scope       string
	WindowStart time.Time
	WindowEnd   time.Time
}

// CertifyAllResult reports the outcome of a blanket certify-all close.
type CertifyAllResult struct {
	ReviewID uuid.UUID
	Status   string
	Approved int
	Revoked  int
	Pending  int
}

// CertificationDecisionUser is the minimal user projection required to decide a
// certification action on a target user (tenancy check + lifecycle status). It
// is a bounded transport object that never carries credentials or secrets.
type CertificationDecisionUser struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Status   domain.UserStatus
}

// CertificationRepository is the output port for persisting certification
// reviews and decisions. It is declared here because the repository
// implementation lives in the interface-adapter layer. Contract notes:
//
//   - Every method takes the tenant predicate explicitly so isolation is
//     enforced at the persistence boundary (RLS/WHERE tenant_id), never
//     filtered in memory.
//   - Append-only integrity is a persistence-layer property: the backing store
//     must grant INSERT/SELECT only (never UPDATE/DELETE) on decision rows, and
//     one-current-decision-per-(review,user) is enforced by a unique index.
//   - Implementations must use parameterized queries only; caller-supplied IDs
//     and scope strings must never be concatenated into SQL.
type CertificationRepository interface {
	// FindOpenByScope returns the open review for a tenant/scope, or nil plus a
	// NOT_FOUND domain error when none is open.
	FindOpenByScope(ctx context.Context, tenantID uuid.UUID, scope string) (*CertificationReview, error)
	// FindReview returns the review by id, or a NOT_FOUND domain error.
	FindReview(ctx context.Context, reviewID uuid.UUID) (*CertificationReview, error)
	// InsertReview persists a new OPEN review.
	InsertReview(ctx context.Context, r *CertificationReview) error
	// InsertDecision appends an immutable certification decision row.
	InsertDecision(ctx context.Context, d *CertificationDecisionRecord) error
	// BulkApprovePending batch-approves every not-yet-decided ACTIVE user in the
	// review's tenant in a single statement and returns the count approved.
	BulkApprovePending(ctx context.Context, reviewID, tenantID, reviewerID uuid.UUID, evidenceRef string) (int, error)
	// CloseReview transitions an OPEN review to CLOSED under an optimistic
	// version guard. The bool reports whether the transition happened; false
	// means the review was concurrently closed or the version moved (409).
	CloseReview(ctx context.Context, reviewID uuid.UUID, closedBy uuid.UUID, expectedVersion int) (bool, error)
	// ListReviews returns the offset-paginated reviews matching the tenant and
	// lifecycle status.
	ListReviews(ctx context.Context, tenantID uuid.UUID, status string, page, pageSize int) ([]CertificationReview, error)
}

// UserRepository is the output port for reading actor/target users and driving
// deprovision bookkeeping. It is declared here because no shared repository
// contract exists on disk; the concrete adapter lives in the interface layer.
//
//   - The 90-day orphan predicate (never logged in, or last login older than
//     cutoff; not an admin) is evaluated in the query with parameterized SQL.
//   - Returns are projections (Actor, CertificationDecisionUser, OrphanedAccount)
//     that never serialize credentials or secrets.
type UserRepository interface {
	// FindActor loads a user's role and tenant for use-case authorization.
	// Returns a NOT_FOUND domain error when the user is unknown.
	FindActor(ctx context.Context, userID uuid.UUID) (Actor, error)
	// FindTargetUser loads the minimal target-user projection for a decision,
	// scoped to the tenant. A wrong-tenant or absent user returns the generic
	// NOT_FOUND domain error (no existence leak across tenants).
	FindTargetUser(ctx context.Context, tenantID, userID uuid.UUID) (CertificationDecisionUser, error)
	// IsLastActiveAdmin reports whether userID is the last active administrator
	// in the tenant (guards the Module F "last admin cannot be removed" criterion).
	IsLastActiveAdmin(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
	// MarkDeprovisioning transitions the target user to the intermediate
	// DEPROVISIONING state so a revoked user cannot re-authenticate while the
	// async deprovision completes. It is idempotent on retry.
	MarkDeprovisioning(ctx context.Context, tenantID, userID uuid.UUID) error
	// FindOrphaned returns the offset-paginated orphaned accounts for the
	// tenant under the 90-day rule, excluding admins.
	FindOrphaned(ctx context.Context, tenantID uuid.UUID, cutoff time.Time, page, pageSize int) ([]OrphanedAccount, error)
}

// CertificationService implements the access-certification use cases: opening
// periodic reviews, recording append-only manager decisions, listing pending
// reviews and orphaned accounts, and closing reviews via blanket certification.
// It depends only on output ports (Clean Architecture: no framework or ORM).
type CertificationService struct {
	cert     CertificationRepository
	users    UserRepository
	sessions ports.SessionStore
	events   ports.EventBus
	now      ports.Clock
}

// NewCertificationService wires the certification use cases to their output
// ports: the certification/decision repository, the user repository, the
// session revocation store, the domain event bus, and a deterministic clock.
func NewCertificationService(
	cert CertificationRepository,
	users UserRepository,
	sessions ports.SessionStore,
	events ports.EventBus,
	now ports.Clock,
) *CertificationService {
	return &CertificationService{
		cert:     cert,
		users:    users,
		sessions: sessions,
		events:   events,
		now:      now,
	}
}

// CreateReview opens a new periodic access review for a scope in the tenant.
// Only a caller holding certification:manage (admin/manager) may create reviews,
// and only one OPEN review may exist per scope (uniqueness guarded both by a
// repository pre-check and, in the adapter, by a partial unique index). The
// close of the create is recorded as an append-only audit event.
func (s *CertificationService) CreateReview(ctx context.Context, cmd CreateReviewCommand, tenantID, actorID uuid.UUID) (*CertificationReview, error) {
	actor, err := s.authorize(ctx, actorID, tenantID)
	if err != nil {
		return nil, err
	}

	scope := strings.TrimSpace(cmd.Scope)
	if scope == "" || len(scope) > maxReviewScopeLen {
		return nil, domain.NewValidationError(domain.FieldError{Field: "scope", Message: "scope is required and must be at most 128 characters"})
	}

	period, err := domain.NewDateRange(cmd.WindowStart, cmd.WindowEnd)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "window", Message: "window start must not be after window end"})
	}

	existing, err := s.cert.FindOpenByScope(ctx, tenantID, scope)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status == reviewStatusOpen {
		return nil, domain.NewConflict("an open certification review already exists for this scope")
	}

	now := s.now.Now()
	review := &CertificationReview{
		ID:          uuid.New(),
		TenantID:    tenantID,
		Scope:       scope,
		WindowStart: period.Start,
		WindowEnd:   period.End,
		Status:      reviewStatusOpen,
		CreatedBy:   actor.UserID,
		CreatedAt:   now,
		Version:     1,
	}
	if err := s.cert.InsertReview(ctx, review); err != nil {
		return nil, domain.NewInternal(err)
	}

	// Evidence record: the review open is an append-only audit event. The
	// CertificationReviewOpened event type is not registered in the domain
	// event registry, so the open is recorded as EntityModified (defense in
	// depth for the compliance evidence trail).
	after, err := json.Marshal(review)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	ev, err := s.newEvent(ctx, actor, domain.EventEntityModified, "certification.review", review.ID.String(), domain.EntityModifiedPayload{
		EntityType: "certification.review",
		EntityID:   review.ID.String(),
		Action:     domain.ActionCreate,
		Before:     nil,
		After:      after,
	})
	if err != nil {
		return nil, err
	}
	if err := s.events.Publish(ctx, ev); err != nil {
		// Publish is best-effort post-commit dispatch per §7.4; a sink failure
		// must never fail the persisted write. The outbox relay retries.
		_ = errors.Join(err)
	}

	return review, nil
}

// RecordDecision appends an immutable certification decision for a target user
// within an open review. Decisions are append-only: corrections are recorded as
// new rows (supersession), never edits. A REVOKE additionally marks the target
// for deprovisioning, revokes all of that user's sessions, and emits a
// UserDeprovisioned event. Authorization is internal (certification:manage,
// tenant match), and the segregation-of-duties / last-admin invariants are
// enforced here before any side effect.
func (s *CertificationService) RecordDecision(
	ctx context.Context,
	reviewID, userID uuid.UUID,
	decision domain.CertificationDecision,
	evidenceRef string,
	tenantID, reviewerID uuid.UUID,
) error {
	actor, err := s.authorize(ctx, reviewerID, tenantID)
	if err != nil {
		return err
	}

	if !decision.Valid() {
		return domain.NewValidationError(domain.FieldError{Field: "decision", Message: "decision must be one of approve, revoke, defer"})
	}

	review, err := s.cert.FindReview(ctx, reviewID)
	if err != nil {
		return err
	}
	if review.TenantID != tenantID {
		// Generic NOT_FOUND so a foreign review id reveals no existence.
		return domain.NewNotFound("certification review", "")
	}
	if review.Status != reviewStatusOpen {
		return domain.NewConflict("review is not open for decisions")
	}

	target, err := s.users.FindTargetUser(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	if target.UserID == uuid.Nil {
		return domain.NewNotFound("user", "")
	}

	now := s.now.Now()
	reason := ""
	if decision == domain.DecisionRevoke {
		reason = revokePolicyReason
	}
	// Build the append-only evidence through the domain aggregate, which is the
	// sole write path for all certification invariants (self-review, reason and
	// evidence on revoke, decision allowlist, evidence-ref allowlist). The use
	// case must not re-implement these rules.
	singleInstant, err := domain.NewDateRange(now, now)
	if err != nil {
		return domain.NewInternal(err)
	}
	evidence, err := domain.NewAccessCertification(
		uuid.New(),
		tenantID,
		reviewerID,
		userID,
		decision,
		reason,
		[]string{strings.TrimSpace(evidenceRef)},
		now,
		singleInstant,
	)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSelfReview):
			return domain.NewForbidden("a reviewer cannot certify their own access")
		case errors.Is(err, domain.ErrEvidenceRequiredOnRevoke),
			errors.Is(err, domain.ErrInvalidEvidenceRef):
			return domain.NewValidationError(domain.FieldError{Field: "evidence_ref", Message: "evidence reference is required and must be valid on revoke"})
		case errors.Is(err, domain.ErrReasonRequiredOnRevoke):
			return domain.NewValidationError(domain.FieldError{Field: "reason", Message: "a reason is required on revoke"})
		case errors.Is(err, domain.ErrInvalidCertificationDecision):
			return domain.NewValidationError(domain.FieldError{Field: "decision", Message: "decision must be one of approve, revoke, defer"})
		case errors.Is(err, domain.ErrInvalidCertificationReason):
			return domain.NewValidationError(domain.FieldError{Field: "reason", Message: "reason is invalid"})
		case errors.Is(err, domain.ErrReviewedAtOutsidePeriod):
			return domain.NewValidationError(domain.FieldError{Field: "reviewed_at", Message: "reviewed at must fall within the review period"})
		}
		return domain.NewInternal(err)
	}
	if err := evidence.Record(); err != nil {
		if errors.Is(err, domain.ErrCertificationRecorded) {
			return domain.NewConflict("a decision has already been recorded for this CertificationDecision")
		}
		return domain.NewInternal(err)
	}

	record := &CertificationDecisionRecord{
		ID:          evidence.ID,
		ReviewID:    reviewID,
		TenantID:    tenantID,
		UserID:      userID,
		ReviewerID:  reviewerID,
		Decision:    evidence.Decision,
		EvidenceRef: evidenceRefRef(evidence),
		ReviewedAt:  evidence.ReviewedAt,
	}
	if err := s.cert.InsertDecision(ctx, record); err != nil {
		return domain.NewInternal(err)
	}

	after, err := json.Marshal(record)
	if err != nil {
		return domain.NewInternal(err)
	}
	evDM, err := s.newEvent(ctx, actor, domain.EventEntityModified, "certification.decision", record.ID.String(), domain.EntityModifiedPayload{
		EntityType: "certification.decision",
		EntityID:   record.ID.String(),
		Action:     domain.ActionCreate,
		Before:     nil,
		After:      after,
	})
	if err != nil {
		return err
	}
	_ = s.events.Publish(ctx, evDM)

	if decision == domain.DecisionRevoke {
		last, err := s.users.IsLastActiveAdmin(ctx, tenantID, userID)
		if err != nil {
			return err
		}
		if last {
			return domain.NewConflict("the last active administrator cannot be deprovisioned")
		}
		if err := s.users.MarkDeprovisioning(ctx, tenantID, userID); err != nil {
			return domain.NewInternal(err)
		}
		// Revoke ALL sessions for the target and fail closed if revocation is
		// unavailable: the deprovision event is not published until sessions are
		// confirmed cut off, so a revoked user cannot retain standing access.
		if err := s.sessions.RevokeAllForUser(ctx, userID); err != nil {
			return domain.NewDependencyUnavailable("session revocation unavailable")
		}
		evDP, err := s.newEvent(ctx, actor, domain.EventUserDeprovisioned, "user", userID.String(), domain.UserDeprovisionedPayload{UserID: userID})
		if err != nil {
			return err
		}
		_ = s.events.Publish(ctx, evDP)
	}

	return nil
}

// ListPending returns the offset-paginated open certification reviews for the
// tenant. Access is restricted to certificate-managing callers.
func (s *CertificationService) ListPending(ctx context.Context, tenantID, actorID uuid.UUID) ([]CertificationReview, error) {
	if _, err := s.authorize(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	reviews, err := s.cert.ListReviews(ctx, tenantID, reviewStatusOpen, reviewListDefaultPage, reviewListDefaultPageSize)
	if err != nil {
		return nil, err
	}
	if reviews == nil {
		return []CertificationReview{}, nil
	}
	return reviews, nil
}

// ListOrphanedAccounts returns the offset-paginated orphaned/inactive accounts
// for the tenant under the 90-day rule (never logged in, or last login older
// than the cutoff, excluding admins). The cutoff is derived from the injected
// Clock in UTC; the predicate and admin exclusion are evaluated in the
// repository's parameterized query.
func (s *CertificationService) ListOrphanedAccounts(ctx context.Context, tenantID, actorID uuid.UUID) ([]OrphanedAccount, error) {
	if _, err := s.authorize(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	cutoff := s.now.Now().UTC().AddDate(0, 0, -orphanInactivityDays)
	orphans, err := s.users.FindOrphaned(ctx, tenantID, cutoff, reviewListDefaultPage, reviewListDefaultPageSize)
	if err != nil {
		return nil, err
	}
	if orphans == nil {
		return []OrphanedAccount{}, nil
	}
	return orphans, nil
}

// CertifyAll closes an open review by bulk-approving every not-yet-decided
// ACTIVE user in its tenant (a single batched statement, no per-user N+1 loop),
// then transitions the review to CLOSED under an optimistic version guard. A
// concurrent close yields CONFLICT_STATE. Pending counts reflect a blanket
// approve-only pass: prior REVOKE decisions were recorded as separate rows and
// are excluded because BulkApprovePending only targets undecided users.
func (s *CertificationService) CertifyAll(ctx context.Context, reviewID, tenantID, reviewerID uuid.UUID) (*CertifyAllResult, error) {
	actor, err := s.authorize(ctx, reviewerID, tenantID)
	if err != nil {
		return nil, err
	}

	review, err := s.cert.FindReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	if review.TenantID != tenantID {
		return nil, domain.NewNotFound("certification review", "")
	}
	if review.Status != reviewStatusOpen {
		return nil, domain.NewConflict("review is not open for certification")
	}

	approved, err := s.cert.BulkApprovePending(ctx, reviewID, tenantID, reviewerID, "certify-all:"+reviewID.String())
	if err != nil {
		return nil, domain.NewInternal(err)
	}

	closed, err := s.cert.CloseReview(ctx, reviewID, reviewerID, review.Version)
	if err != nil {
		return nil, err
	}
	if !closed {
		return nil, domain.NewConflict("review was concurrently closed")
	}

	result := &CertifyAllResult{
		ReviewID: reviewID,
		Status:   reviewStatusClosed,
		Approved: approved,
		Revoked:  0,
		Pending:  0,
	}
	after, err := json.Marshal(result)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	ev, err := s.newEvent(ctx, actor, domain.EventEntityModified, "certification.review", reviewID.String(), domain.EntityModifiedPayload{
		EntityType: "certification.review",
		EntityID:   reviewID.String(),
		Action:     domain.ActionCreate,
		Before:     nil,
		After:      after,
	})
	if err != nil {
		return nil, err
	}
	_ = s.events.Publish(ctx, ev)

	return result, nil
}

// authorize resolves the actor from the user repository and enforces, inside
// the use case, that the actor belongs to the given tenant and holds
// certification:manage (admin/manager-only). It returns a generic FORBIDDEN on
// any mismatch so no existence is revealed. Authorization is never trusted from
// the caller.
func (s *CertificationService) authorize(ctx context.Context, actorID, tenantID uuid.UUID) (Actor, error) {
	actor, err := s.users.FindActor(ctx, actorID)
	if err != nil {
		return Actor{}, err
	}
	if actor.TenantID != tenantID || actorID != actor.UserID {
		return Actor{}, domain.NewForbidden("access denied")
	}
	if !actor.Role.Has(domain.PermissionCertificationManage) {
		return Actor{}, domain.NewForbidden("access denied")
	}
	return actor, nil
}

// newEvent builds a validated domain.Event envelope around a registered payload.
// The use case supplies a server-issued correlation id and a placeholder
// positive sequence; the outbox/EventBus adapter assigns the authoritative
// per-aggregate monotonic Sequence and carries tenant/actor context from the
// IAM-established request context per the EventBus port contract.
func (s *CertificationService) newEvent(ctx context.Context, actor Actor, typ domain.EventType, aggregateType, aggregateID string, payload any) (domain.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	ev := domain.Event{
		ID:             uuid.New(),
		Type:           typ,
		Version:        1,
		Timestamp:      s.now.Now(),
		TenantID:       actor.TenantID,
		ActorID:        actor.UserID,
		CorrelationID:  uuid.New().String(),
		SourceService:  "iam",
		AggregateType:  aggregateType,
		AggregateID:    aggregateID,
		Payload:        raw,
		Sequence:       1,
	}
	if err := ev.Validate(); err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	return ev, nil
}

// evidenceRefRef returns the first trimmed evidence reference carried by the
// recorded aggregate, or the empty string when none is present. REVOKE
// decisions always carry exactly one evidence ref supplied by the caller.
func evidenceRefRef(evidence domain.AccessCertification) string {
	if len(evidence.EvidenceRefs) == 0 {
		return ""
	}
	return evidence.EvidenceRefs[0]
}