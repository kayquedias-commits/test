// Package ports declares the application-level ports (output interfaces) consumed by use
// cases. The Cache port is a hot-data cache for pre-aggregated read-model projections:
// budget vs-actual aggregates (TTL 60 s) and category/account lists (TTL 5 m). It is never
// the authoritative ledger — transactional facts are always served from the relational
// store; the cache only holds derived projections.
package ports

import (
	"context"
	"time"

	"fms/backend/internal/domain"
)

// TTL policy for cached projections. Callers own these constants; the Cache implementation
// is policy-agnostic and only enforces whatever TTL is passed to Set.
const (
	// BudgetAggregateTTL bounds how stale a budget vs-actual aggregate may be served after a
	// mutation self-heals on the estimate path when invalidation itself fails.
	BudgetAggregateTTL = 60 * time.Second
	// ListCacheTTL applies to category and account lists, which mutate rarely.
	ListCacheTTL = 5 * time.Minute
)

// cacheKeyPrefix namespaces all cache keys so budget aggregates, category/account lists,
// rate-limit counters, and session blacklists can coexist in one Redis instance.
const cacheKeyPrefix = "fms:v1"

// Cache abstracts a hot-data key/value store for pre-aggregated read-model projections.
//
// Get follows a load-bearing miss-vs-error contract that preserves the "ledger is never
// served from cache" invariant:
//   - (nil, nil) means the key is not cached. The caller recomputes from the authoritative
//     repository and populates the cache.
//   - (nil, error) means the cache is unavailable. The caller must fall back to the
//     authoritative repository and surface an X-Cache: MISS/ERROR header; an error is never
//     treated as a miss.
//
// Implementations must fail fast with short timeouts so a cache outage degrades to the
// authoritative store rather than blocking the request path. Errors returned by any method
// are domain-level DEPENDENCY_UNAVAILABLE errors, never driver-specific types.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	InvalidatePattern(ctx context.Context, pattern string) error
}

// NewCacheUnavailable wraps an underlying cache failure as a domain DEPENDENCY_UNAVAILABLE
// error so use cases can branch on it and fall back to the authoritative store.
func NewCacheUnavailable(cause error) error {
	return &domain.DomainError{
		Code:    domain.CodeDependencyUnavailable,
		Message: "cache unavailable",
		Cause:   cause,
	}
}

// BudgetKey builds the tenant-scoped cache key for a budget vs-actual aggregate. period is
// an ISO-8601 interval ("2024-01-01/2024-12-31") matching the Budget DateRange encoding.
// tenantID is derived from the IAM-established tenant context, never client request input;
// an empty tenantID is a programming error and yields an empty key.
func BudgetKey(tenantID, budgetID, period string) string {
	if tenantID == "" {
		return ""
	}
	return cacheKeyPrefix + ":" + tenantID + ":budget:" + budgetID + ":" + period
}

// CategoryListKey builds the tenant-scoped cache key for a category list. tenantID comes
// from the server-derived tenant context; empty tenantID yields an empty key.
func CategoryListKey(tenantID string) string {
	if tenantID == "" {
		return ""
	}
	return cacheKeyPrefix + ":" + tenantID + ":categories:list"
}

// AccountListKey builds the tenant-scoped cache key for an account list. tenantID comes
// from the server-derived tenant context; empty tenantID yields an empty key.
func AccountListKey(tenantID string) string {
	if tenantID == "" {
		return ""
	}
	return cacheKeyPrefix + ":" + tenantID + ":accounts:list"
}

// BudgetPattern builds the allowlisted tenant-scoped pattern used to invalidate all budget
// aggregates for a tenant (e.g. after a bulk transaction import). Invalidation must be
// issued per-tenant; the expected composition is always via this helper and never a
// free-form, externally supplied pattern.
func BudgetPattern(tenantID string) string {
	if tenantID == "" {
		return ""
	}
	return cacheKeyPrefix + ":" + tenantID + ":budget:*"
}