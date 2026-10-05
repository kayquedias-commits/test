// Package domain holds the financial-management core with zero framework, HTTP,
// or persistence dependencies (Clean Architecture invariant). This file defines
// the income/expense category taxonomy (Category aggregate root per Blueprint
// Module C): one level of subcategories, in-use deletion protection, and merge
// with transaction propagation. Changes never corrupt reporting aggregates
// because cross-type and cross-tenant operations are rejected at the boundary.
package domain

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// CategoryNameMaxLen is the maximum length of a category name. It is kept
// tighter than the 255-character transaction description cap because category
// names feed report group-by keys and export columns.
const CategoryNameMaxLen = 100

// Sentinel errors returned by the Category aggregate. They are sentinel so
// adapters can map identity and tests can assert on error identity rather than
// message text; domain-context deletions surface as CodeConflictState at the
// adapter boundary.
var (
	// ErrCategoryInUse signals a Delete attempt on a category referenced by at
	// least one transaction or child category.
	ErrCategoryInUse = errors.New("domain: category is in use")
	// ErrCategoryHasChildren signals a Delete attempt on a category still acting
	// as a parent, which would orphan its subcategories.
	ErrCategoryHasChildren = errors.New("domain: category has child categories")
	// ErrCategoryParentNotTopLevel signals a SetParent or merge whose target is
	// itself a subcategory, which would exceed the one-level depth invariant.
	ErrCategoryParentNotTopLevel = errors.New("domain: parent must be a top-level category")
	// ErrCategorySelfParent signals a category set as its own parent.
	ErrCategorySelfParent = errors.New("domain: category cannot be its own parent")
	// ErrCategoryNilParent signals a nil parent argument where an entity was
	// required.
	ErrCategoryNilParent = errors.New("domain: nil parent category")
	// ErrCategoryNilTarget signals a nil merge target.
	ErrCategoryNilTarget = errors.New("domain: nil merge target category")
	// ErrCategoryMergeIntoSelf signals a merge of a category into itself.
	ErrCategoryMergeIntoSelf = errors.New("domain: cannot merge category into itself")
	// ErrCategoryCrossTenantMerge signals a merge or re-parent across different
	// tenants, which would corrupt tenant isolation.
	ErrCategoryCrossTenantMerge = errors.New("domain: cannot merge across tenants")
	// ErrCategoryCrossTypeMerge signals a merge or re-parent between income and
	// expense, which would sign-mix reporting aggregates.
	ErrCategoryCrossTypeMerge = errors.New("domain: cannot merge across types")
	// ErrInvalidCategoryName signals a name that fails allowlist validation.
	ErrInvalidCategoryName = errors.New("domain: invalid category name")
)

// Category is an aggregate root enforcing taxonomy integrity for the
// income/expense classification hierarchy.
//
// ID and TenantID are set at construction and immutable. Name must satisfy the
// allowlist enforced by renameCategoryName/NewCategory. Type is the income/
// expense value object and fixes the aggregation sign for reporting. ParentID is
// nil for a top-level category and otherwise references a top-level category,
// which SetParent enforces (depth is limited to one). InUse is derived by the
// repository from transaction references and is not a primary truth field; the
// repository must re-verify it atomically with any delete (in-transaction check
// plus the persistence-level ON DELETE RESTRICT backstop) to close the
// check-then-delete race. Version guards optimistic locking on every mutating
// method.
type Category struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Name     string
	Type     TransactionType
	ParentID *uuid.UUID
	InUse    bool
	Version  int64
}

// validateCategoryType returns ErrInvalidTransactionType unless typ is one of
// the canonical income/expense values.
func validateCategoryType(typ TransactionType) error {
	if !typ.Valid() {
		return ErrInvalidTransactionType
	}
	return nil
}

// sanitizeCategoryName trims the name and enforces the allowlist: non-empty,
// at most CategoryNameMaxLen runes, no control characters, and no spreadsheet
// formula-injection prefix ('=', '+', '-', '@'). It returns the trimmed,
// validated name.
func sanitizeCategoryName(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", ErrInvalidCategoryName
	}
	if len(trimmed) > CategoryNameMaxLen {
		return "", ErrInvalidCategoryName
	}
	switch rune(trimmed[0]) {
	case '=', '+', '-', '@':
		return "", ErrInvalidCategoryName
	}
	for _, r := range trimmed {
		if r <= 0x1f || r == 0x7f {
			return "", ErrInvalidCategoryName
		}
	}
	return trimmed, nil
}

// NewCategory constructs a validated top-level Category. The name and type pass
// through the allowlist; an id or tenant of uuid.Nil, an invalid type, or an
// invalid name yield a structured DomainError with code-field detail or the
// ErrInvalidTransactionType sentinel. On success the category starts unused and
// optimistic-locked at version zero.
func NewCategory(id, tenantID uuid.UUID, name string, typ TransactionType) (Category, error) {
	var c Category

	var fields []FieldError
	if id == uuid.Nil {
		fields = append(fields, FieldError{Field: "id", Message: "id is required"})
	}
	if tenantID == uuid.Nil {
		fields = append(fields, FieldError{Field: "tenant_id", Message: "tenant_id is required"})
	}
	if err := validateCategoryType(typ); err != nil {
		return c, err
	}
	cleanName, err := sanitizeCategoryName(name)
	if err != nil {
		fields = append(fields, FieldError{Field: "name", Message: "invalid category name"})
	}
	if len(fields) > 0 {
		return c, NewValidationError(fields...)
	}

	c = Category{
		ID:       id,
		TenantID: tenantID,
		Name:     cleanName,
		Type:     typ,
		InUse:    false,
		Version:  0,
	}
	return c, nil
}

// SetParent re-parents c under p, enforcing the one-level depth invariant. The
// parent must be a top-level category of the same tenant and type, and cannot
// be c itself. Only c is mutated — p is never written (aggregate isolation), so
// callers must not rely on p changing. On success ParentID points at p.ID and
// Version increments.
func (c *Category) SetParent(p *Category) error {
	if p == nil {
		return ErrCategoryNilParent
	}
	if p.ID == c.ID {
		return ErrCategorySelfParent
	}
	if p.ParentID != nil {
		return ErrCategoryParentNotTopLevel
	}
	if p.TenantID != c.TenantID {
		return ErrCategoryCrossTenantMerge
	}
	if p.Type != c.Type {
		return ErrCategoryCrossTypeMerge
	}
	c.ParentID = &p.ID
	c.Version++
	return nil
}

// Delete guards the in-use invariant: a category referenced by transactions or
// acting as a parent cannot be deleted without a preceding merge. The returned
// CodeConflictState error maps to HTTP 409 by the adapter. This is the domain
// guard only — the repository must re-verify InUse atomically with the delete
// (and the schema must carry ON DELETE RESTRICT) so a concurrent transaction
// cannot slip through between this check and the delete.
func (c *Category) Delete() error {
	if c.InUse {
		return NewConflict(ErrCategoryInUse.Error())
	}
	return nil
}

// CategoryMergePlan is an immutable value object describing a validated merge:
// the source and target categories and the IDs of the source's transactions and
// child categories that the use case reassigns to the target. Execution (re-point
// transactions and children, delete the source, bump both versions) belongs to a
// single ACID transaction in the use-case layer; this aggregate never performs
// the writes itself.
type CategoryMergePlan struct {
	SourceID       uuid.UUID
	TargetID       uuid.UUID
	TransactionIDs []uuid.UUID
	// ChildIDs are reassigned to the target only when the target is top-level,
	// preserving the one-level depth invariant; otherwise the merge is rejected.
	ChildIDs []uuid.UUID
}

// MergeInto validates that c can be merged into target and returns the
// reassignment plan. It rejects, before any plan is produced: a nil target or
// self-merge; a cross-tenant target; a cross-type target (income into expense or
// vice versa would sign-mix reporting aggregates); and a merge whose source has
// children into a non-top-level target (child promotion would exceed depth one).
//
// The caller supplies childIDs and transactionIDs — the aggregate holds no child
// or transaction lists. MergeInto is read-validate-only: it never mutates c or
// target. The use case executes the plan atomically and emits the merge event,
// so historical rows re-bucket consistently and reporting aggregates stay intact.
func (c *Category) MergeInto(target *Category, childIDs, transactionIDs []uuid.UUID) (CategoryMergePlan, error) {
	if target == nil {
		return CategoryMergePlan{}, ErrCategoryNilTarget
	}
	if target.ID == c.ID {
		return CategoryMergePlan{}, ErrCategoryMergeIntoSelf
	}
	if target.TenantID != c.TenantID {
		return CategoryMergePlan{}, ErrCategoryCrossTenantMerge
	}
	if target.Type != c.Type {
		return CategoryMergePlan{}, ErrCategoryCrossTypeMerge
	}
	if len(childIDs) > 0 && target.ParentID != nil {
		return CategoryMergePlan{}, ErrCategoryParentNotTopLevel
	}

	return CategoryMergePlan{
		SourceID:       c.ID,
		TargetID:       target.ID,
		TransactionIDs: transactionIDs,
		ChildIDs:       childIDs,
	}, nil
}

// Rename applies a new name to c after allowlist validation, bumping Version on
// success. A name that is empty, over-length, control-character-laden, or starts
// with a spreadsheet formula-injection prefix yields a structured
// CodeValidationFailed error rather than mutating state.
func (c *Category) Rename(newName string) error {
	cleanName, err := sanitizeCategoryName(newName)
	if err != nil {
		return NewValidationError(FieldError{Field: "name", Message: "invalid category name"})
	}
	c.Name = cleanName
	c.Version++
	return nil
}