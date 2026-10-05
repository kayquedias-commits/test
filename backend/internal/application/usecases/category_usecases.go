// Package usecases implements the application-layer orchestration of the FMS
// core. Use cases depend only on the output ports and the domain layer; they
// contain no framework, HTTP, or persistence types.
//
// This file owns the Module-C (Category taxonomy) bounded context. It manages
// the income/expense category lifecycle (create/update/delete/list), the
// merge-with-transaction-propagation path, and the tenant-scoped idempotent
// seed of the canonical default taxonomy; every mutation publishes an audit
// event into the event bus.
package usecases

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// Bounds for the bounded-collection category list (spec default 20, max 100).
const (
	defaultCategoryPageSize = 20
	maxCategoryPageSize     = 100
)

// secureCategorySource identifies the category module in domain-event envelopes.
const secureCategorySource = "category"

// CategoryRepository is the output port for the Category aggregate. Every
// method carries a tenant predicate so no query can cross a tenant boundary;
// Update and Delete perform version-guarded conditional writes for optimistic
// concurrency. ReassignSources and SeedDefaults run within a single
// transaction (see their contracts) so a category merge can never tear.
type CategoryRepository interface {
	// Create persists a new category.
	Create(ctx context.Context, c *domain.Category) error
	// GetByID loads a single tenant-scoped category.
	GetByID(ctx context.Context, id, tenantID uuid.UUID) (*domain.Category, error)
	// Update persists field changes with an atomic conditional write guarded by
	// expectedVersion; a version mismatch surfaces as a conflict-state error.
	Update(ctx context.Context, c *domain.Category, expectedVersion int64) error
	// Delete removes a category under an optimistic version guard.
	Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error
	// List returns a page of tenant-scoped categories (optionally filtered by
	// type) and the total row count. Tree grouping by parent is a presentation
	// concern delegated to the adapter.
	List(ctx context.Context, tenantID uuid.UUID, typ *domain.TransactionType, page, pageSize int) ([]*domain.Category, int64, error)
	// CountUsage reports whether a category is referenced by transactions or
	// budgets — the indexed in-use guard. It is an existence check, never a
	// full COUNT.
	CountUsage(ctx context.Context, id, tenantID uuid.UUID) (int64, error)
	// ReassignSources merges source into target inside ONE transaction: it
	// re-checks source != target and target active/depth invariants atomically,
	// re-points the source's transactions and child categories onto the target,
	// marks the source inactive (soft-delete, audit-chain safe), bumps the
	// target's version, and returns the validated plan for event publishing. A
	// version-guarded source update that races to zero rows aborts the
	// transaction and surfaces as a conflict-state error.
	ReassignSources(ctx context.Context, sourceID, targetID, tenantID uuid.UUID, sourceVersion int64) (*domain.CategoryMergePlan, error)
	// SeedDefaults idempotently upserts the canonical per-tenant taxonomy and
	// returns the number of rows inserted.
	SeedDefaults(ctx context.Context, tenantID uuid.UUID) (int64, error)
}

// CreateCategoryCommand carries the validated inputs for creating a category.
// Type must be one of the closed domain income/expense values; ParentID, when
// set, must reference a top-level category of the same tenant and type.
type CreateCategoryCommand struct {
	Name     string
	Type     domain.TransactionType
	ParentID *uuid.UUID
}

// UpdateCategoryCommand carries the mutable fields for editing a category.
// Only Name is mutable here; Type and ParentID cannot be re-assigned on edit
// (re-association is legal only through MergeCategories) so mass-assignment of
// identity or reporting-shaping fields is impossible.
type UpdateCategoryCommand struct {
	Name     string
	Type     domain.TransactionType
	ParentID *uuid.UUID
}

// CategoryService orchestrates the Module-C category use cases. Dependencies
// (CategoryRepository, EventBus, Clock) arrive via constructor injection; the
// repository owns the single ACID transaction for a merge (this layer holds no
// UnitOfWork port — the persistence adapter provides the transactional seam).
type CategoryService struct {
	cats   CategoryRepository
	events ports.EventBus
	clock  ports.Clock
}

// NewCategoryService wires the category use cases to their output ports.
func NewCategoryService(cats CategoryRepository, events ports.EventBus, now ports.Clock) *CategoryService {
	if now == nil {
		now = ports.SystemClock{}
	}
	return &CategoryService{
		cats:   cats,
		events: events,
		clock:  now,
	}
}

// CreateCategory validates the command and persists a new category for a
// tenant. The tenant and actor IDs must come from the authenticated context,
// never from client input. A depth-two parent is rejected by the aggregate
// SetParent invariant.
func (s *CategoryService) CreateCategory(ctx context.Context, cmd CreateCategoryCommand, tenantID, actorID uuid.UUID) (*domain.Category, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}
	if !cmd.Type.Valid() {
		return nil, domain.NewValidationError(domain.FieldError{Field: "type", Message: "must be income or expense"})
	}

	category, err := domain.NewCategory(uuid.New(), tenantID, cmd.Name, cmd.Type)
	if err != nil {
		return nil, s.mapCategoryError(err)
	}

	if cmd.ParentID != nil {
		parent, err := s.cats.GetByID(ctx, *cmd.ParentID, tenantID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, domain.NewNotFound("category", cmd.ParentID.String())
		}
		if err := category.SetParent(parent); err != nil {
			return nil, s.mapCategoryError(err)
		}
	}

	if err := s.cats.Create(ctx, &category); err != nil {
		return nil, err
	}
	_ = s.publishEntityModified(ctx, domain.ActionCreate, tenantID, actorID, category.ID, nil, categorySnapshot(&category))
	return &category, nil
}

// UpdateCategory edits an existing category under an optimistic version guard.
// Only the name is mutable; attempting to change Type or re-parent is rejected
// as a validation error, and version mismatches surface as conflict-state.
func (s *CategoryService) UpdateCategory(ctx context.Context, id uuid.UUID, expectedVersion int64, cmd UpdateCategoryCommand, tenantID, actorID uuid.UUID) (*domain.Category, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}

	category, err := s.cats.GetByID(ctx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if category == nil {
		return nil, domain.NewNotFound("category", id.String())
	}
	if expectedVersion != category.Version {
		return nil, domain.NewConflict("category was modified by another user")
	}

	if cmd.Type != "" && cmd.Type != category.Type {
		return nil, domain.NewValidationError(domain.FieldError{Field: "type", Message: "category type cannot be changed; re-association requires a merge"})
	}
	if cmd.ParentID != nil && (category.ParentID == nil || *cmd.ParentID != *category.ParentID) {
		return nil, domain.NewValidationError(domain.FieldError{Field: "parent_id", Message: "category parent cannot be changed; re-association requires a merge"})
	}

	before := categorySnapshot(category)

	if cmd.Name != "" {
		if err := category.Rename(cmd.Name); err != nil {
			return nil, s.mapCategoryError(err)
		}
	}

	if err := s.cats.Update(ctx, category, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}
	_ = s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, id, before, categorySnapshot(category))
	return category, nil
}

// DeleteCategory removes a category under an optimistic version guard. An
// in-use category (referenced by a transaction, budget, or child) is refused
// with a conflict-state error (HTTP 409) — deletion is legal only after a
// merge.
func (s *CategoryService) DeleteCategory(ctx context.Context, id uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return domain.NewForbidden("insufficient permissions")
	}

	category, err := s.cats.GetByID(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if category == nil {
		return domain.NewNotFound("category", id.String())
	}
	if expectedVersion != category.Version {
		return domain.NewConflict("category was modified by another user")
	}

	// Domain guard: a category in use (transactions or children) cannot be
	// deleted without a preceding merge.
	if err := category.Delete(); err != nil {
		return s.mapCategoryError(err)
	}
	// Repository existence re-check closes the check-then-delete race.
	usage, err := s.cats.CountUsage(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if usage > 0 {
		return s.mapCategoryError(domain.ErrCategoryInUse)
	}

	before := categorySnapshot(category)

	if err := s.cats.Delete(ctx, id, tenantID, expectedVersion); err != nil {
		return err
	}
	_ = s.publishEntityModified(ctx, domain.ActionDelete, tenantID, actorID, id, before, nil)
	return nil
}

// MergeCategories reassigns a source category's transactions and child
// categories onto a target and marks the source inactive, atomically. The
// merge never hard-deletes the source (audit-chain safe) and rejects
// cross-tenant, cross-type, self, and depth-violating merges. Publish of the
// source-deactivation and target-usage events lets the read-model consumer
// refresh reporting aggregates propagation-style, off the write path.
func (s *CategoryService) MergeCategories(ctx context.Context, sourceID, targetID uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return domain.NewForbidden("insufficient permissions")
	}

	source, err := s.cats.GetByID(ctx, sourceID, tenantID)
	if err != nil {
		return err
	}
	if source == nil {
		return domain.NewNotFound("category", sourceID.String())
	}
	target, err := s.cats.GetByID(ctx, targetID, tenantID)
	if err != nil {
		return err
	}
	if target == nil {
		return domain.NewNotFound("category", targetID.String())
	}
	if expectedVersion != source.Version {
		return domain.NewConflict("category was modified by another user")
	}

	// Domain validation of the merge relationship (tenant, type, self). The
	// child/depth guard is re-verified atomically by ReassignSources inside the
	// transaction, so a concurrent deactivate/merge of the target cannot slip
	// in between this read-validate and the reassignment.
	if _, err := source.MergeInto(target, nil, nil); err != nil {
		return s.mapCategoryError(err)
	}

	sourceBefore := categorySnapshot(source)
	targetBefore := categorySnapshot(target)

	plan, err := s.cats.ReassignSources(ctx, sourceID, targetID, tenantID, expectedVersion)
	if err != nil {
		return conflictOrRepoError(err)
	}

	_ = s.publishEntityModified(ctx, domain.ActionDelete, tenantID, actorID, plan.SourceID, sourceBefore, nil)
	_ = s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, plan.TargetID, targetBefore, targetBefore)
	return nil
}

// ListCategories returns a tenant-scoped page of categories (optionally
// filtered by type) with the total count. Pagination is bounded
// (page >= 1, 1 <= pageSize <= 100).
func (s *CategoryService) ListCategories(ctx context.Context, tenantID uuid.UUID, typ *domain.TransactionType, page, pageSize int) ([]*domain.Category, int64, error) {
	if tenantID == uuid.Nil {
		return nil, 0, domain.NewForbidden("invalid tenant context")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultCategoryPageSize
	}
	if pageSize > maxCategoryPageSize {
		pageSize = maxCategoryPageSize
	}
	return s.cats.List(ctx, tenantID, typ, page, pageSize)
}

// SeedDefaults idempotently upserts the canonical income/expense taxonomy for
// a tenant and returns the number of rows inserted. It is strictly
// tenant-scoped and safe to run at tenant bootstrap; re-invocation produces no
// duplicates due to the per-tenant unique guard in the repository.
func (s *CategoryService) SeedDefaults(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, domain.NewForbidden("invalid tenant context")
	}
	return s.cats.SeedDefaults(ctx, tenantID)
}

// mapCategoryError translates a Category aggregate error into a structured
// domain error. In-use, cross-tenant/type, self, and depth violations surface
// as conflict-state (HTTP 409); type and name failures as field-level
// validation errors; anything unexpected as an internal error.
func (s *CategoryService) mapCategoryError(err error) error {
	switch {
	case errors.Is(err, domain.ErrCategoryInUse):
		return domain.NewConflict(domain.ErrCategoryInUse.Error())
	case errors.Is(err, domain.ErrCategoryHasChildren):
		return domain.NewConflict(domain.ErrCategoryHasChildren.Error())
	case errors.Is(err, domain.ErrCategoryParentNotTopLevel):
		return domain.NewConflict(domain.ErrCategoryParentNotTopLevel.Error())
	case errors.Is(err, domain.ErrCategorySelfParent):
		return domain.NewConflict(domain.ErrCategorySelfParent.Error())
	case errors.Is(err, domain.ErrCategoryMergeIntoSelf):
		return domain.NewConflict(domain.ErrCategoryMergeIntoSelf.Error())
	case errors.Is(err, domain.ErrCategoryCrossTenantMerge):
		return domain.NewConflict(domain.ErrCategoryCrossTenantMerge.Error())
	case errors.Is(err, domain.ErrCategoryCrossTypeMerge):
		return domain.NewConflict(domain.ErrCategoryCrossTypeMerge.Error())
	case errors.Is(err, domain.ErrCategoryNilParent):
		return domain.NewInternal(err)
	case errors.Is(err, domain.ErrCategoryNilTarget):
		return domain.NewInternal(err)
	case errors.Is(err, domain.ErrInvalidCategoryName):
		return domain.NewValidationError(domain.FieldError{Field: "name", Message: "invalid category name"})
	case errors.Is(err, domain.ErrInvalidTransactionType):
		return domain.NewValidationError(domain.FieldError{Field: "type", Message: "must be income or expense"})
	default:
		return domain.NewInternal(err)
	}
}

// newEvent wraps a payload into a validated domain.Event envelope, mirroring
// the BudgetService helper. The event ID doubles as the correlation id;
// Sequence is a non-zero placeholder assigned by the publishing infrastructure.
func (s *CategoryService) newEvent(typ domain.EventType, tenantID, actorID, aggregateID uuid.UUID, payload any) (domain.Event, error) {
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
		Timestamp:     s.clock.Now().UTC(),
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: evtID.String(),
		SourceService: secureCategorySource,
		AggregateType: "category",
		AggregateID:   aggregateID.String(),
		Payload:       raw,
		Sequence:      1,
	}, nil
}

// publishEntityModified publishes a registered EventEntityModified lifecycle
// event for a category mutation; the before/after state carries minimal
// category details for the append-only audit consumer.
func (s *CategoryService) publishEntityModified(ctx context.Context, action domain.AuditAction, tenantID, actorID, entityID uuid.UUID, before, after json.RawMessage) error {
	payload := domain.EntityModifiedPayload{
		EntityType: "category",
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

// categorySnapshot renders a non-secret subset of a category carried in audit
// events; it omits no identity fields required by the event contract.
func categorySnapshot(c *domain.Category) json.RawMessage {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(struct {
		Name     string                 `json:"name"`
		Type     domain.TransactionType `json:"type"`
		ParentID *uuid.UUID             `json:"parent_id,omitempty"`
		InUse    bool                   `json:"in_use"`
		Version  int64                  `json:"version"`
	}{
		Name:     c.Name,
		Type:     c.Type,
		ParentID: c.ParentID,
		InUse:    c.InUse,
		Version:  c.Version,
	})
	if err != nil {
		return nil
	}
	return raw
}

// conflictOrRepoError re-wraps a repository conflict-state error unchanged and
// otherwise passes the repository error through. It is shared by the category
// update path and the merge reassignment path so an optimistic-lock failure
// surfaces as HTTP 409.
func conflictOrRepoError(err error) error {
	if de, ok := domain.AsDomainError(err); ok && de.Code == domain.CodeConflictState {
		return err
	}
	return err
}