package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newTestAccount builds an account of the given type with the opening balance
// parsed from a decimal string. Its own ID is stable so foreign-account tests
// can reference a second account.
func newTestAccount(t *testing.T, typ AccountType, opening string) Account {
	t.Helper()
	openingBal, err := Parse(opening, "USD")
	require.NoError(t, err)
	acc, err := NewAccount(uuid.New(), uuid.New(), "Operating Account", typ, openingBal, "USD")
	require.NoError(t, err)
	return acc
}

func mustTxFor(t *testing.T, acc Account, amount, kind string) AccountTx {
	t.Helper()
	m, err := Parse(amount, "USD")
	require.NoError(t, err)
	return AccountTx{AccountID: acc.ID, Amount: m, Kind: TxKind(kind)}
}

// money builds a Money value from an exact minor-unit (cents) amount so table
// cases stay readable without scattering Parse calls.
func money(t *testing.T, minor int64) Money {
	t.Helper()
	m, err := NewFromMajor(minor, "USD")
	require.NoError(t, err)
	return m
}

// newEvent builds a minimally valid Event envelope for a registered type so
// tests focus on the payload contract rather than wheelbarrowing envelope
// fields.
func newEvent(typ EventType, ver int, tenant uuid.UUID, payload json.RawMessage) Event {
	id := uuid.New()
	return Event{
		ID:            id,
		Type:          typ,
		Version:       ver,
		Timestamp:     time.Now().UTC(),
		TenantID:      tenant,
		ActorID:       uuid.New(),
		CorrelationID: "req-txn-1",
		SourceService: "Ledger",
		AggregateType: "transaction",
		AggregateID:   id.String(),
		Payload:       payload,
		Sequence:      1,
	}
}

// newTestUser builds an active user with a stable, non-nil role reference.
func newTestUser(t *testing.T, tenant uuid.UUID) User {
	t.Helper()
	u, err := NewUser(uuid.New(), tenant, "admin_user", "admin@example.com", "hash", "secret", true, uuid.New())
	require.NoError(t, err)
	return u
}

func TestParseAccountTypeAllowlist(t *testing.T) {
	valid := []string{"checking", "savings", "credit", "cash", "other", " checking ", "savings"}
	for _, raw := range valid {
		got, err := ParseAccountType(raw)
		require.NoError(t, err, "expected %q to parse", raw)
		require.NotEmpty(t, got)
	}
	invalid := []string{"", "foo", "Checking", "SAVINGS", "credit_card", " checking\t", "other2"}
	for _, raw := range invalid {
		_, err := ParseAccountType(raw)
		require.ErrorIs(t, err, ErrInvalidAccountType, "expected %q to be rejected", raw)
	}
	require.False(t, AccountType("").Valid())
	require.False(t, AccountType("unknown").Valid())
	require.True(t, AccountTypeChecking.Valid())
	require.True(t, AccountTypeOther.Valid())
}

func TestNewAccountValidation(t *testing.T) {
	m, err := Parse("100.00", "USD")
	require.NoError(t, err)

	t.Run("rejects empty ids", func(t *testing.T) {
		_, err := NewAccount(uuid.Nil, uuid.New(), "Name", AccountTypeChecking, m, "USD")
		require.Error(t, err)
		var de *DomainError
		require.ErrorAs(t, err, &de)
		require.Equal(t, CodeValidationFailed, de.Code)
		_, err = NewAccount(uuid.New(), uuid.Nil, "Name", AccountTypeChecking, m, "USD")
		require.Error(t, err)
	})

	t.Run("rejects unknown type", func(t *testing.T) {
		_, err := NewAccount(uuid.New(), uuid.New(), "Name", AccountType("wire"), m, "USD")
		require.ErrorIs(t, err, ErrInvalidAccountType)
	})

	t.Run("rejects bad names", func(t *testing.T) {
		for _, name := range []string{"", "   ", "y" + string(rune(0x10)) + "x"} {
			_, err := NewAccount(uuid.New(), uuid.New(), name, AccountTypeChecking, m, "USD")
			require.Error(t, err)
		}
	})

	t.Run("rejects currency mismatch", func(t *testing.T) {
		_, err := NewAccount(uuid.New(), uuid.New(), "Name", AccountTypeChecking, m, "EUR")
		require.Error(t, err)
	})

	t.Run("seeds balance to opening with version 1", func(t *testing.T) {
		acc, err := NewAccount(uuid.New(), uuid.New(), "Name", AccountTypeChecking, m, "USD")
		require.NoError(t, err)
		require.Equal(t, acc.OpeningBalance, acc.Balance)
		require.Equal(t, int64(1), acc.Version)
		require.False(t, acc.Closed)
		require.Equal(t, int64(0), acc.TransactionCount)
	})
}

func TestBalanceInvariantUnderSequences(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "100.00")

	err := acc.ApplyTransaction(mustTxFor(t, acc, "25.00", string(TxKindCredit)), acc.Version)
	require.NoError(t, err)
	err = acc.ApplyTransaction(mustTxFor(t, acc, "10.00", string(TxKindCredit)), acc.Version)
	require.NoError(t, err)
	err = acc.ApplyTransaction(mustTxFor(t, acc, "5.00", string(TxKindDebit)), acc.Version)
	require.NoError(t, err)

	require.Equal(t, "130.00", acc.Balance.String())
	require.Equal(t, int64(4), acc.Version)
	require.Equal(t, int64(3), acc.TransactionCount)
	require.NoError(t, acc.Reconcile([]AccountTx{
		mustTxFor(t, acc, "25.00", string(TxKindCredit)),
		mustTxFor(t, acc, "10.00", string(TxKindCredit)),
		mustTxFor(t, acc, "5.00", string(TxKindDebit)),
	}))
}

func TestCreditAccountDirectionInverts(t *testing.T) {
	acc := newTestAccount(t, AccountTypeCredit, "0.00")

	// Debts increase the amount owed.
	err := acc.ApplyTransaction(mustTxFor(t, acc, "500.00", string(TxKindDebit)), acc.Version)
	require.NoError(t, err)
	require.Equal(t, "500.00", acc.Balance.String())

	// Repayments reduce the amount owed.
	err = acc.ApplyTransaction(mustTxFor(t, acc, "120.00", string(TxKindCredit)), acc.Version)
	require.NoError(t, err)
	require.Equal(t, "380.00", acc.Balance.String())
}

func TestAdjustBalanceOptimisticLock(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "50.00")
	delta, err := Parse("20.00", "USD")
	require.NoError(t, err)

	stale := acc.Version
	require.NoError(t, acc.AdjustBalance(delta, stale))
	require.Equal(t, "70.00", acc.Balance.String())
	require.Equal(t, stale+1, acc.Version)

	// A stale guarded write must not mutate.
	err = acc.AdjustBalance(delta, stale)
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
	require.Equal(t, "70.00", acc.Balance.String())
	require.Equal(t, stale+1, acc.Version)

	// Currency mismatch is rejected without mutation.
	eur, err := Parse("10.00", "EUR")
	require.NoError(t, err)
	require.Error(t, acc.AdjustBalance(eur, acc.Version))
	require.Equal(t, "70.00", acc.Balance.String())
}

func TestApplyTransactionStaleVersion(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	stale := acc.Version

	require.NoError(t, acc.ApplyTransaction(mustTxFor(t, acc, "10.00", string(TxKindCredit)), stale))
	staleCopy := acc.Version

	// Stale version on a second write is rejected and leaves the balance intact.
	err := acc.ApplyTransaction(mustTxFor(t, acc, "10.00", string(TxKindCredit)), stale)
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
	require.Equal(t, "10.00", acc.Balance.String())
	require.Equal(t, staleCopy, acc.Version)
}

func TestApplyTransactionCrossAccountRejected(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	other := newTestAccount(t, AccountTypeChecking, "0.00")

	tx := mustTxFor(t, other, "10.00", string(TxKindCredit))
	require.NotEqual(t, acc.ID, tx.AccountID)

	err := acc.ApplyTransaction(tx, acc.Version)
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
	require.Equal(t, "0.00", acc.Balance.String())

	// nil reference is also rejected.
	err = acc.ApplyTransaction(AccountTx{AccountID: uuid.Nil, Amount: other.OpeningBalance, Kind: TxKindCredit}, acc.Version)
	require.Error(t, err)
}

func TestApplyTransactionNegativeRejected(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	neg := acc.OpeningBalance.Negate()

	err := acc.ApplyTransaction(AccountTx{AccountID: acc.ID, Amount: neg, Kind: TxKindCredit}, acc.Version)
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
	require.Equal(t, "0.00", acc.Balance.String())
}

func TestDeletionBoundary(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	require.True(t, acc.CanDelete())

	require.NoError(t, acc.ApplyTransaction(mustTxFor(t, acc, "5.00", string(TxKindCredit)), acc.Version))
	require.False(t, acc.CanDelete())
}

func TestReconcileDetectsDrift(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "100.00")
	require.NoError(t, acc.ApplyTransaction(mustTxFor(t, acc, "20.00", string(TxKindCredit)), acc.Version))

	// Exact match reconciles cleanly.
	require.NoError(t, acc.Reconcile([]AccountTx{mustTxFor(t, acc, "20.00", string(TxKindCredit))}))

	// A mismatched transaction set reports drift as CONFLICT_STATE.
	err := acc.Reconcile([]AccountTx{mustTxFor(t, acc, "9001.00", string(TxKindCredit))})
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
	// Reconcile is diagnostic-only: it must never mutate the balance or version.
	require.Equal(t, "120.00", acc.Balance.String())
}

func TestReconcileCrossAccountRejected(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	other := newTestAccount(t, AccountTypeChecking, "0.00")

	err := acc.Reconcile([]AccountTx{mustTxFor(t, other, "10.00", string(TxKindCredit))})
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)
}

func TestClosedGuard(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "0.00")
	require.NoError(t, acc.Close())
	require.True(t, acc.Closed)

	// Explicit re-close is a conflict, not an idempotent no-op.
	err := acc.Close()
	require.Error(t, err)
	var de *DomainError
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)

	err = acc.ApplyTransaction(mustTxFor(t, acc, "10.00", string(TxKindCredit)), acc.Version)
	require.Error(t, err)
	require.ErrorAs(t, err, &de)
	require.Equal(t, CodeConflictState, de.Code)

	delta, err := Parse("10.00", "USD")
	require.NoError(t, err)
	require.Error(t, acc.AdjustBalance(delta, acc.Version))
	require.Equal(t, "0.00", acc.Balance.String())
}

func TestZeroAmountTransactionBumpsNothing(t *testing.T) {
	acc := newTestAccount(t, AccountTypeChecking, "50.00")
	before := acc.Version

	require.NoError(t, acc.ApplyTransaction(mustTxFor(t, acc, "0.00", string(TxKindCredit)), before))
	require.Equal(t, before, acc.Version)
	require.Equal(t, int64(0), acc.TransactionCount)
	require.Equal(t, "50.00", acc.Balance.String())
}

// TestMoneyPrecision locks down the monetary value-object invariants: at most
// two decimals, non-negative sign at the parse boundary, exact minor-unit
// arithmetic, and overflow rejection on accumulated addition.
func TestMoneyPrecision(t *testing.T) {
	t.Run("rejects more than two decimals", func(t *testing.T) {
		_, err := Parse("12.345", "USD")
		require.Error(t, err)
	})

	t.Run("rejects signs, exponents, and separators", func(t *testing.T) {
		for _, raw := range []string{"-5.00", "+5.00", "1e3", "1_000", "1.", ".5", "12,34"} {
			_, err := Parse(raw, "USD")
			require.Error(t, err, "expected %q to be rejected", raw)
		}
	})

	t.Run("one and two decimals normalize to minor units", func(t *testing.T) {
		m, err := Parse("12.34", "USD")
		require.NoError(t, err)
		require.Equal(t, int64(1234), m.Major())
		require.Equal(t, "12.34", m.String())

		m2, err := Parse("12.3", "USD")
		require.NoError(t, err)
		require.Equal(t, int64(1230), m2.Major())
		require.Equal(t, "12.30", m2.String())
	})

	t.Run("accepts the NUMERIC(12,2) upper bound", func(t *testing.T) {
		m, err := Parse("9999999999.99", "USD")
		require.NoError(t, err)
		require.Equal(t, int64(999999999999), m.Major())
	})

	t.Run("add sub negate are exact at minor precision", func(t *testing.T) {
		a := money(t, 1005) // 10.05
		b := money(t, 5)    // 0.05
		sum, err := a.Add(b)
		require.NoError(t, err)
		require.Equal(t, "10.10", sum.String())

		diff, err := a.Sub(b)
		require.NoError(t, err)
		require.Equal(t, "10.00", diff.String())

		neg := money(t, 500).Negate()
		require.True(t, neg.IsNegative())
		require.Equal(t, int64(-500), neg.Major())
		require.Equal(t, "-5.00", neg.String())
	})

	t.Run("accumulated addition guards int64 overflow", func(t *testing.T) {
		// Arithmetic runs on int64 minor units; adding two huge balances must
		// be rejected, never silently wrapped. Note the Money overflow guard is
		// int64-based — enforcing the NUMERIC(12,2) column ceiling is a
		// persistence CHECK-constraint concern covered by an integration test,
		// not by this value object.
		maxBad, err := NewFromMajor(math.MaxInt64, "USD")
		require.NoError(t, err)
		_, err = maxBad.Add(money(t, 1))
		require.ErrorIs(t, err, errMoneyOverflow)
	})

	t.Run("sign coupling is enforced at the transaction gate", func(t *testing.T) {
		// Money does not refuse negatives by itself (adjustments need them);
		// the reason-code coupling lives in NewTransaction.
		neg := money(t, 500).Negate()
		cat := uuid.New()
		_, err := NewTransaction(uuid.New(), uuid.New(), uuid.New(), []uuid.UUID{cat}, neg, time.Now(), "adjust", Income, false, nil)
		require.ErrorIs(t, err, ErrNegativeAmountRequiresReasonCode)

		rc := ReasonAdjustment
		_, err = NewTransaction(uuid.New(), uuid.New(), uuid.New(), []uuid.UUID{cat}, neg, time.Now(), "adjust", Income, false, &rc)
		require.NoError(t, err)
	})
}

// TestCategoryDeleteGuard verifies the in-use deletion protection boundary of
// the Category aggregate: an in-use category cannot be deleted, while an
// unused one can. InUse is derived by the repository from transaction
// references, so these tests set it directly (white-box) to model a referenced
// category.
func TestCategoryDeleteGuard(t *testing.T) {
	tenant := uuid.New()
	inUse, err := NewCategory(uuid.New(), tenant, "Rent", Expense)
	require.NoError(t, err)

	// Model a category referenced by at least one transaction.
	inUse.InUse = true
	err = inUse.Delete()
	require.Error(t, err)
	de, ok := AsDomainError(err)
	require.True(t, ok)
	require.Equal(t, CodeConflictState, de.Code)

	unused, err := NewCategory(uuid.New(), tenant, "Petty Cash", Expense)
	require.NoError(t, err)
	require.NoError(t, unused.Delete())
}

// TestCategoryMerge verifies the merge semantics of the Category aggregate:
// a validated merge produces a plan that reassigns the source's transactions
// and children to the target, and rejects merges that would break tenant
// isolation, sign-mix reporting, one-level depth, or self/cross-tenant
// integrity.
func TestCategoryMerge(t *testing.T) {
	tenant := uuid.New()
	src, err := NewCategory(uuid.New(), tenant, "Travel", Expense)
	require.NoError(t, err)
	dst, err := NewCategory(uuid.New(), tenant, "Business", Expense)
	require.NoError(t, err)

	childID := uuid.New()
	txID := uuid.New()

	plan, err := src.MergeInto(&dst, []uuid.UUID{childID}, []uuid.UUID{txID})
	require.NoError(t, err)
	require.Equal(t, src.ID, plan.SourceID)
	require.Equal(t, dst.ID, plan.TargetID)
	require.Equal(t, []uuid.UUID{txID}, plan.TransactionIDs)
	require.Equal(t, []uuid.UUID{childID}, plan.ChildIDs)
	// MergeInto is read-validate-only; it never mutates either aggregate.
	require.Equal(t, false, src.InUse)

	t.Run("rejects depth beyond one level", func(t *testing.T) {
		// Target that is itself a subcategory cannot accept a source with
		// children, or depth would exceed one.
		subTarget := dst
		pid := dst.ID
		subTarget.ParentID = &pid
		_, err := src.MergeInto(&subTarget, []uuid.UUID{childID}, nil)
		require.ErrorIs(t, err, ErrCategoryParentNotTopLevel)
	})

	t.Run("rejects cross-type merge", func(t *testing.T) {
		incomeDst, err := NewCategory(uuid.New(), tenant, "Income Side", Income)
		require.NoError(t, err)
		_, err = src.MergeInto(&incomeDst, nil, nil)
		require.ErrorIs(t, err, ErrCategoryCrossTypeMerge)
	})

	t.Run("rejects cross-tenant merge", func(t *testing.T) {
		otherTenant, err := NewCategory(uuid.New(), uuid.New(), "Other Tenant", Expense)
		require.NoError(t, err)
		_, err = src.MergeInto(&otherTenant, nil, nil)
		require.ErrorIs(t, err, ErrCategoryCrossTenantMerge)
	})

	t.Run("rejects self and nil target", func(t *testing.T) {
		_, err := src.MergeInto(&src, nil, nil)
		require.ErrorIs(t, err, ErrCategoryMergeIntoSelf)
		_, err = src.MergeInto(nil, nil, nil)
		require.ErrorIs(t, err, ErrCategoryNilTarget)
	})
}

// TestLastAdminInvariant enforces the "last admin user cannot be deprovisioned"
// acceptance criterion: an ACTIVE user cannot be deleted/deprovisioned when the
// tenant's active admin count is at most one, while a non-final active admin and
// an already-suspended final admin remain removable.
func TestLastAdminInvariant(t *testing.T) {
	tenant := uuid.New()
	admin := newTestUser(t, tenant)

	// Final active admin cannot be removed (FORBIDDEN, not CONFLICT).
	err := admin.CanDelete(1)
	require.Error(t, err)
	de, ok := AsDomainError(err)
	require.True(t, ok)
	require.Equal(t, CodeForbidden, de.Code)

	// A non-final active admin can be removed.
	require.NoError(t, admin.CanDelete(2))

	// A suspended final admin can be removed (not an active admin).
	require.NoError(t, admin.Deactivate())
	require.NoError(t, admin.CanDelete(1))
}

// TestAcceptance_LastAdminCannotBeDeleted is the executable form of the §7
// acceptance criterion: deprovisioning/removing the only remaining admin is
// blocked, then deprovisioning transitions the aggregate to the terminal state
// and yields the outbox payload for the UserDeprovisioned event.
func TestAcceptance_LastAdminCannotBeDeleted(t *testing.T) {
	tenant := uuid.New()
	admin := newTestUser(t, tenant)

	require.Error(t, admin.CanDelete(1))

	payload, err := admin.Deprovision()
	require.NoError(t, err)
	require.Equal(t, admin.ID, payload.UserID)
	require.Equal(t, UserStatusDeprovisioned, admin.Status)
	require.False(t, admin.Active())

	// Deprovisioning is irreversible: a second call is a conflict.
	_, err = admin.Deprovision()
	require.Error(t, err)
	de, ok := AsDomainError(err)
	require.True(t, ok)
	require.Equal(t, CodeConflictState, de.Code)
}

// TestUserDeprovisionedEventContract verifies that the payload returned by
// Deprovision, wrapped into the canonical EventUserDeprovisioned envelope, is
// valid for outbox publication (spec §7.4 outbox pattern). The payload must
// never carry secrets — only the user id.
func TestUserDeprovisionedEventContract(t *testing.T) {
	tenant := uuid.New()
	user := newTestUser(t, tenant)
	payload, err := user.Deprovision()
	require.NoError(t, err)

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(raw), user.PasswordHash)
	require.NotContains(t, string(raw), user.MFASecret)

	ver, ok := EventUserDeprovisioned.Version()
	require.True(t, ok)
	ev := newEvent(EventUserDeprovisioned, ver, tenant, raw)

	require.NoError(t, ev.Validate())
	decoded, err := DecodePayload(EventUserDeprovisioned, ev.Payload)
	require.NoError(t, err)
	decodedPayload, ok := decoded.(UserDeprovisionedPayload)
	require.True(t, ok)
	require.Equal(t, user.ID, decodedPayload.UserID)
}

// TestEnumeratedAllowlists asserts that unknown TransactionType and ReasonCode
// values are rejected at the domain boundary rather than accepted as arbitrary
// strings — the allowlist that stops unapproved codes from bypassing the
// negative-amount gate or flipping a ledger's aggregation direction.
func TestEnumeratedAllowlists(t *testing.T) {
	_, err := ParseTransactionType("income")
	require.NoError(t, err)
	_, err = ParseTransactionType("expense")
	require.NoError(t, err)
	_, err = ParseTransactionType("refund")
	require.ErrorIs(t, err, ErrInvalidTransactionType)
	_, err = ParseTransactionType("Income")
	require.ErrorIs(t, err, ErrInvalidTransactionType)
	_, err = ParseTransactionType("")
	require.ErrorIs(t, err, ErrInvalidTransactionType)
	require.False(t, TransactionType("").Valid())
	require.False(t, TransactionType("unknown").Valid())
	require.True(t, Income.Valid())
	require.True(t, Expense.Valid())

	_, err = ParseReasonCode("adjustment")
	require.NoError(t, err)
	_, err = ParseReasonCode("refund")
	require.NoError(t, err)
	_, err = ParseReasonCode("ADJUSTMENT")
	require.ErrorIs(t, err, ErrInvalidReasonCode)
	_, err = ParseReasonCode("")
	require.ErrorIs(t, err, ErrInvalidReasonCode)
	require.False(t, ReasonCode("").Valid())
	require.False(t, ReasonCode("unknown").Valid())
	require.True(t, ReasonAdjustment.Valid())
	require.True(t, ReasonRefund.Valid())
}

// TestTransactionValidation drives NewTransaction against table-driven input /
// expected-error cases. Field aggregation to a single VALIDATION_FAILED
// response is a use-case/adapter concern (RFC 7807, §7.7); this domain test
// asserts the precise invalid-field sentinels each rule produces. The sentinels
// are exported so the use-case layer can map them onto the field_errors array.
func TestTransactionValidation(t *testing.T) {
	tenant := uuid.New()
	account := uuid.New()
	category := uuid.New()
	future := time.Now().UTC().Add(24 * time.Hour)

	valid := func() (uuid.UUID, uuid.UUID, uuid.UUID, []uuid.UUID, Money, time.Time, string, TransactionType, bool, *ReasonCode) {
		amt, err := Parse("50.00", "USD")
		require.NoError(t, err)
		id, accID := uuid.New(), account
		return id, tenant, accID, []uuid.UUID{category}, amt, time.Now(), "Groceries", Expense, false, nil
	}

	assertErr := func(name string, got error, want error) {
		t.Helper()
		if want == nil {
			require.NoError(t, got, name)
			return
		}
		require.ErrorIs(t, got, want, name)
	}

	t.Run("description must be non-empty and within 255 runes", func(t *testing.T) {
		id, ten, acc, cats, amt, dt, _, typ, sch, rc := valid()
		_, err := NewTransaction(id, ten, acc, cats, amt, dt, "", typ, sch, rc)
		assertErr("empty description", err, ErrInvalidDescription)

		longDesc := strings.Repeat("a", 256)
		id, ten, acc, cats, amt, dt, _, typ, sch, rc = valid()
		_, err = NewTransaction(id, ten, acc, cats, amt, dt, longDesc, typ, sch, rc)
		assertErr("256-char description", err, ErrInvalidDescription)

		// Exactly 255 is accepted.
		id, ten, acc, cats, amt, dt, _, typ, sch, rc = valid()
		_, err = NewTransaction(id, ten, acc, cats, amt, dt, strings.Repeat("a", 255), typ, sch, rc)
		assertErr("255-char description", err, nil)
	})

	t.Run("negative amounts require a reason code; bounds are accepted", func(t *testing.T) {
		// A negative amount without a reason code is rejected.
		neg, err := Parse("50.00", "USD")
		require.NoError(t, err)
		neg = neg.Negate()
		id, ten, acc, cats, _, dt, desc, typ, sch, rc := valid()
		_, err = NewTransaction(id, ten, acc, cats, neg, dt, desc, typ, sch, rc)
		assertErr("negative without reason", err, ErrNegativeAmountRequiresReasonCode)

		// Zero is non-negative, so the domain accepts it (positivity above zero
		// is enforced by account posting rules and downstream policy).
		id, ten, acc, cats, _, dt, desc, typ, sch, rc = valid()
		zero, err := Parse("0.00", "USD")
		require.NoError(t, err)
		_, err = NewTransaction(id, ten, acc, cats, zero, dt, desc, typ, sch, rc)
		assertErr("zero amount", err, nil)

		// Minimum representable: 0.01.
		id, ten, acc, cats, _, dt, desc, typ, sch, rc = valid()
		min, err := Parse("0.01", "USD")
		require.NoError(t, err)
		_, err = NewTransaction(id, ten, acc, cats, min, dt, desc, typ, sch, rc)
		assertErr("min amount", err, nil)

		// Maximum NUMERIC(12,2) value accepted at the domain boundary.
		id, ten, acc, cats, _, dt, desc, typ, sch, rc = valid()
		max, err := Parse("9999999999.99", "USD")
		require.NoError(t, err)
		_, err = NewTransaction(id, ten, acc, cats, max, dt, desc, typ, sch, rc)
		assertErr("max amount", err, nil)
	})

	t.Run("required references", func(t *testing.T) {
		id, ten, _, cats, amt, dt, desc, typ, sch, rc := valid()
		_, err := NewTransaction(id, ten, uuid.Nil, cats, amt, dt, desc, typ, sch, rc)
		assertErr("empty account", err, ErrInvalidAccountID)

		id, ten, acc, _, amt, dt, desc, typ, sch, rc = valid()
		_, err = NewTransaction(id, ten, acc, nil, amt, dt, desc, typ, sch, rc)
		assertErr("no categories", err, ErrAtLeastOneCategory)
	})

	t.Run("calendar-valid dates only", func(t *testing.T) {
		// 2023-02-29 does not exist; NewTransaction normalizes and rejects it.
		badDate := time.Date(2023, 2, 29, 0, 0, 0, 0, time.UTC)
		id, ten, acc, cats, amt, _, desc, typ, sch, rc := valid()
		_, err := NewTransaction(id, ten, acc, cats, amt, badDate, desc, typ, sch, rc)
		assertErr("impossible date", err, ErrInvalidTransactionDate)
	})

	t.Run("future dates require the scheduled flag", func(t *testing.T) {
		// Future date without scheduled is rejected.
		id, ten, acc, cats, amt, _, desc, typ, _, rc := valid()
		_, err := NewTransaction(id, ten, acc, cats, amt, future, desc, typ, false, rc)
		assertErr("future unscheduled", err, ErrFutureDateRequiresScheduled)

		// Same future date with scheduled is accepted.
		id, ten, acc, cats, amt, _, desc, typ, _, rc = valid()
		_, err = NewTransaction(id, ten, acc, cats, amt, future, desc, typ, true, rc)
		assertErr("future scheduled", err, nil)
	})
}

// TestTransactionEventContracts verifies the transaction event payloads that
// the audit/budget/reporting consumers rely on: posted, edited (with before and
// after), and deleted (with before only). Each payload is wrapped into its
// canonical Event envelope and validated, including the round-trip through
// DecodePayload.
func TestTransactionEventContracts(t *testing.T) {
	tenant := uuid.New()
	accID := uuid.New()
	txID := uuid.New()

	posted, err := NewTransactionPostedPayload(txID, accID, "50.00", Income)
	require.NoError(t, err)
	postedRaw, err := json.Marshal(posted)
	require.NoError(t, err)

	ver, ok := EventTransactionPosted.Version()
	require.True(t, ok)
	ev := newEvent(EventTransactionPosted, ver, tenant, postedRaw)
	require.NoError(t, ev.Validate())
	decoded, err := DecodePayload(EventTransactionPosted, ev.Payload)
	require.NoError(t, err)
	require.IsType(t, TransactionPostedPayload{}, decoded)
	require.Equal(t, txID, decoded.(TransactionPostedPayload).TransactionID)

	// An edit-style payload carries both before and after.
	edited := TransactionEditedPayload{
		TransactionID: txID,
		Before:        json.RawMessage(`{"description":"Groceries","amount":"50.00"}`),
		After:         json.RawMessage(`{"description":"Groceries & supplies","amount":"50.00"}`),
	}
	require.NoError(t, edited.Validate())
	editedRaw, err := json.Marshal(edited)
	require.NoError(t, err)
	ver, ok = EventTransactionEdited.Version()
	require.True(t, ok)
	ev = newEvent(EventTransactionEdited, ver, tenant, editedRaw)
	require.NoError(t, ev.Validate())

	// A delete-style payload carries before only; after is nil.
	deleted := TransactionEditedPayload{
		TransactionID: txID,
		Before:        json.RawMessage(`{"description":"Groceries","amount":"50.00"}`),
	}
	require.NoError(t, deleted.Validate())
	deletedRaw, err := json.Marshal(deleted)
	require.NoError(t, err)
	ver, ok = EventTransactionDeleted.Version()
	require.True(t, ok)
	ev = newEvent(EventTransactionDeleted, ver, tenant, deletedRaw)
	require.NoError(t, ev.Validate())
}