// Package ports defines the output (driven) ports owned by the application
// layer and implemented by infrastructure adapters, inverting dependency
// direction per Clean Architecture (specification §5.1, §5.2). Infrastructure
// compiles against these interfaces, never the reverse.
package ports

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// ExportFormat enumerates the supported report/export output formats.
type ExportFormat string

const (
	// FormatCSV is the comma-separated values render target.
	FormatCSV ExportFormat = "csv"
	// FormatPDF is the portable-document render target.
	FormatPDF ExportFormat = "pdf"
)

// ReportSummary holds the top-line figures for a PDF report layout. Monetary
// figures are decimal.Decimal canonical values (two-decimal precision, never
// binary floats, per §7.3); the renderer applies display formatting.
type ReportSummary struct {
	TotalIncome    decimal.Decimal
	TotalExpense   decimal.Decimal
	NetAmount      decimal.Decimal
	DateRangeStart time.Time
	DateRangeEnd   time.Time
}

// ReportPeriodPoint is one dated point in a PDF report period series.
type ReportPeriodPoint struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	Income      decimal.Decimal
	Expense     decimal.Decimal
	Net         decimal.Decimal
}

// ReportModel carries the full data a PDF layout needs for any of the four
// report types (profit/loss, expense breakdown, income trend, cash flow — §8.2).
// An empty dataset is represented as a zero-valued Summary and empty
// PeriodSeries, never an error (Module E boundary).
type ReportModel struct {
	Summary      ReportSummary
	PeriodSeries []ReportPeriodPoint
	CurrencyCode string
}

// ExportGateway is the rendering/storage seam for permission-scoped async
// exports (blueprint §5.2, Module H). Use cases call it after enforcing the
// export permission scope and date-range validity; large exports run on async
// workers and complete via a short-lived pre-signed download URL (§7.2, §7.4).
//
// Implementing adapters (infrastructure/export) must translate storage/provider
// failures into the domain error vocabulary (§7.7), never leak SDK-native errors.
//
// Security invariants the adapter MUST enforce:
//
//   - objectKey is always server-generated in the form {tenant_id}/{uuid}.{ext}
//     and never derived from client input; reject any key failing the allowlist
//     pattern (no "..", leading "/", backslash, or URL-encoded separators).
//   - IssuePresignedURL must re-validate that the key belongs to the current
//     principal before signing, and clamp ttl to min(ttl, 15*time.Minute). The
//     returned URL is an HTTPS bearer token and must never be logged.
//   - RenderCSV must escape any cell value beginning with '=', '+', '-', '@',
//     tab, or CR (CSV formula-injection defense).
//   - RenderPDF must HTML-escape all dynamic string fields and disable remote
//     resource loading in the renderer.
//   - Upload must reject payloads above a defined size cap and be idempotent by
//     objectKey (overwrite semantics).
type ExportGateway interface {
	// RenderCSV renders tabular data as CSV bytes. rows is expected csv-aligned
	// with headers (validated at the use-case boundary); an empty rows set yields
	// a header-only CSV, not an error.
	RenderCSV(ctx context.Context, rows [][]string, headers []string) ([]byte, error)
	// RenderPDF renders a ReportModel as PDF bytes for the four report layouts.
	RenderPDF(ctx context.Context, report ReportModel) ([]byte, error)
	// Upload stores rendered bytes at objectKey in the object store.
	Upload(ctx context.Context, objectKey string, data []byte) error
	// IssuePresignedURL returns a short-lived download URL for objectKey; the
	// URL is only valid after Upload of the same key has succeeded.
	IssuePresignedURL(ctx context.Context, objectKey string, ttl time.Duration) (string, error)
}