// Package domain holds the financial-management core with zero framework, HTTP,
// or persistence dependencies. This file models the access-certification review
// (Identity & Access bounded context): periodic access reviews, manager
// certification of continuing access, and orphaned/inactive-account deprovision
// evidence. A certification is a compliance record that is finalized once and
// never mutated (append-only), satisfying the user-lifecycle certification
// journey and the tamper-evident audit invariant.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CertificationDecision is an immutable value object enumerating the possible
// outcome of an access-certification review. The allowed set is fixed and
// validated; arbitrary strings are rejected.
type CertificationDecision string

const (
	// DecisionApprove certifies the subject's continuing access.
	DecisionApprove CertificationDecision = "approve"
	// DecisionRevoke deprovisions the subject's access; requires reason + evidence.
	DecisionRevoke CertificationDecision = "revoke"
	// DecisionDefer postpones the decision, allowing a later re-review.
	DecisionDefer CertificationDecision = "defer"
)

// Reason/evidence length and count bounds. Reason feeds admin/audit views and
// export columns, and evidence refs may be surfaced or resolved downstream, so
// both are bounded and allowlisted to prevent injection and unbounded input.
const (
	CertificationReasonMaxLen      = 500
	CertificationEvidenceRefMaxLen = 255
	CertificationMaxEvidenceRefs   = 32
)

// Sentinel errors returned by the AccessCertification aggregate. They let
// adapters map failures and tests assert on error identity. Non-self-review and
// evidence/reason-on-revoke invariants are compliance-critical (segregation of
// duties, deprovision evidence) and fail closed.
var (
	// ErrInvalidCertificationDecision signals a decision outside {approve,revoke,defer}.
	ErrInvalidCertificationDecision = errors.New("domain: invalid certification decision")
	// ErrInvalidCertificationReason signals a reason that is empty-on-revoke, over
	// length, or contains rejected characters.
	ErrInvalidCertificationReason = errors.New("domain: invalid certification reason")
	// ErrReasonRequiredOnRevoke signals a revoke decision with no reason (policy).
	ErrReasonRequiredOnRevoke = errors.New("domain: reason required on revoke")
	// ErrInvalidEvidenceRef signals an evidence reference that is empty, over
	// length, or has a disallowed leading character.
	ErrInvalidEvidenceRef = errors.New("domain: invalid evidence reference")
	// ErrEvidenceRequiredOnRevoke signals a revoke decision with no evidence refs.
	ErrEvidenceRequiredOnRevoke = errors.New("domain: evidence required on revoke")
	// ErrSelfReview signals reviewerID == userID (segregation of duties; a
	// reviewer must not certify their own access).
	ErrSelfReview = errors.New("domain: reviewer cannot certify their own access")
	// ErrReviewedAtOutsidePeriod signals reviewedAt falling outside the review window.
	ErrReviewedAtOutsidePeriod = errors.New("domain: reviewed at falls outside the review period")
	// ErrCertificationRecorded signals a second Record() on an already-recorded
	// certification (append-only immutability guard).
	ErrCertificationRecorded = errors.New("domain: certification already recorded")
)

// AccessCertification is an aggregate root recording an access-certification
// decision for a subject (UserID) made by a reviewer (ReviewerID) within a
// tenant. The four UUIDs set identity and tenancy at construction and are
// immutable. Decision is the approved/revoked/deferred outcome (validated value
// object). Reason is required non-empty when Decision == DecisionRevoke.
// EvidenceRefs capture compliance artifacts (access-review sign-off,
// orphan-detection run IDs) and are required non-empty on revoke. ReviewedAt is
// the server-side certification timestamp and must fall within ReviewPeriod.
// ReviewPeriod reuses the existing DateRange value object (invariant
// Start <= End). recorded is a private append-only flag set by Record().
type AccessCertification struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	ReviewerID   uuid.UUID
	UserID       uuid.UUID
	Decision     CertificationDecision
	Reason       string
	EvidenceRefs []string
	ReviewedAt   time.Time
	ReviewPeriod DateRange
	recorded     bool
}

// Valid reports whether the decision is one of the canonical values.
func (d CertificationDecision) Valid() bool {
	switch d {
	case DecisionApprove, DecisionRevoke, DecisionDefer:
		return true
	default:
		return false
	}
}

// ParseCertificationDecision returns the decision matching s, rejecting any
// string not exactly one of the canonical lowercase constants.
func ParseCertificationDecision(s string) (CertificationDecision, error) {
	d := CertificationDecision(s)
	if !d.Valid() {
		return "", ErrInvalidCertificationDecision
	}
	return d, nil
}

// sanitizeCertificationReason trims and allowlists a certification reason:
// non-empty (caller checks emptiness separately for revoke), at most
// CertificationReasonMaxLen runes, no control characters, and no spreadsheet
// formula-injection prefix.
func sanitizeCertificationReason(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", ErrInvalidCertificationReason
	}
	if len(trimmed) > CertificationReasonMaxLen {
		return "", ErrInvalidCertificationReason
	}
	switch rune(trimmed[0]) {
	case '=', '+', '-', '@':
		return "", ErrInvalidCertificationReason
	}
	for _, r := range trimmed {
		if r <= 0x1f || r == 0x7f {
			return "", ErrInvalidCertificationReason
		}
	}
	return trimmed, nil
}

// sanitizeEvidenceRef trims and allowlists a single evidence reference:
// non-empty, at most CertificationEvidenceRefMaxLen runes, no control
// characters (refs are treated as opaque identifiers to avoid path/URL issues).
func sanitizeEvidenceRef(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", ErrInvalidEvidenceRef
	}
	if len(trimmed) > CertificationEvidenceRefMaxLen {
		return "", ErrInvalidEvidenceRef
	}
	for _, r := range trimmed {
		if r <= 0x1f || r == 0x7f {
			return "", ErrInvalidEvidenceRef
		}
	}
	return trimmed, nil
}

// validate enforces every recording invariant and returns a structured
// CodeValidationFailed error aggregating field detail, or a sentinel error for
// the specific compliance invariants. It is the single source of invariant
// truth used by both the constructor and Record().
func (c *AccessCertification) validate() error {
	var fields []FieldError

	if c.ID == uuid.Nil {
		fields = append(fields, FieldError{Field: "id", Message: "id is required"})
	}
	if c.TenantID == uuid.Nil {
		fields = append(fields, FieldError{Field: "tenant_id", Message: "tenant_id is required"})
	}
	if c.ReviewerID == uuid.Nil {
		fields = append(fields, FieldError{Field: "reviewer_id", Message: "reviewer_id is required"})
	}
	if c.UserID == uuid.Nil {
		fields = append(fields, FieldError{Field: "user_id", Message: "user_id is required"})
	}
	if !c.Decision.Valid() {
		fields = append(fields, FieldError{Field: "decision", Message: "decision must be one of approve, revoke, defer"})
	}
	if !c.ReviewPeriod.Valid() {
		fields = append(fields, FieldError{Field: "review_period", Message: "review period start must not be after end"})
	}
	if c.ReviewedAt.IsZero() || !c.ReviewPeriod.Contains(c.ReviewedAt) {
		fields = append(fields, FieldError{Field: "reviewed_at", Message: "reviewed_at is required and must fall within the review period"})
	}
	if c.ReviewerID != uuid.Nil && c.ReviewerID == c.UserID {
		return ErrSelfReview
	}
	if len(c.EvidenceRefs) > CertificationMaxEvidenceRefs {
		fields = append(fields, FieldError{Field: "evidence_refs", Message: "too many evidence references"})
	}
	if c.Reason != "" {
		if _, err := sanitizeCertificationReason(c.Reason); err != nil {
			fields = append(fields, FieldError{Field: "reason", Message: "invalid reason"})
		}
	}

	// Evidence refs, when present, are individually allowlisted.
	cleanedRefs := make([]string, 0, len(c.EvidenceRefs))
	for _, ref := range c.EvidenceRefs {
		clean, err := sanitizeEvidenceRef(ref)
		if err != nil {
			fields = append(fields, FieldError{Field: "evidence_refs", Message: "invalid evidence reference"})
			continue
		}
		cleanedRefs = append(cleanedRefs, clean)
	}
	c.EvidenceRefs = cleanedRefs

	// Revoke requires compliance evidence and a policy reason.
	if c.Decision == DecisionRevoke {
		if len(c.EvidenceRefs) == 0 {
			return ErrEvidenceRequiredOnRevoke
		}
		if c.Reason == "" {
			return ErrReasonRequiredOnRevoke
		}
	}

	if len(fields) > 0 {
		return NewValidationError(fields...)
	}
	return nil
}

// NewAccessCertification constructs a validated AccessCertification, rejecting
// missing IDs, an invalid decision, an invalid review period, a reviewedAt
// outside the window, self-review, and (on revoke) a missing reason or evidence.
// The returned entity is not yet recorded; use Record() to finalize it.
func NewAccessCertification(
	id, tenantID, reviewerID, userID uuid.UUID,
	decision CertificationDecision,
	reason string,
	evidenceRefs []string,
	reviewedAt time.Time,
	period DateRange,
) (AccessCertification, error) {
	c := AccessCertification{
		ID:           id,
		TenantID:     tenantID,
		ReviewerID:   reviewerID,
		UserID:       userID,
		Decision:     decision,
		Reason:       reason,
		EvidenceRefs: evidenceRefs,
		ReviewedAt:   reviewedAt,
		ReviewPeriod: period,
		recorded:     false,
	}
	if err := c.validate(); err != nil {
		return AccessCertification{}, err
	}
	return c, nil
}

// Record finalizes the certification as the single append-only domain mutation.
// It re-validates every invariant (so a literal-constructed entity cannot
// bypass validation) and rejects a second call on an already-recorded
// certification with ErrCertificationRecorded, preserving audit immutability.
// It publishes no domain events — the orchestrating use case raises events such
// as AccessCertified/AccessRevoked and the Audit Service consumes them
// out-of-band.
func (c *AccessCertification) Record() error {
	if c.recorded {
		return ErrCertificationRecorded
	}
	if err := c.validate(); err != nil {
		return err
	}
	c.recorded = true
	return nil
}

// IsComplete reports whether the certification has been finalized and carries a
// valid decision. Because validate() rejects invalid decisions and (on revoke)
// enforces evidence and reason before Record() marks the entity recorded,
// recorded implies a valid decision with conditional evidence/reason satisfied.
func (c *AccessCertification) IsComplete() bool {
	return c.recorded && c.Decision.Valid()
}