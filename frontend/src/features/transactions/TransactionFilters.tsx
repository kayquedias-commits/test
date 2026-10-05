import { useCallback, useEffect, useRef, useState } from 'react';
import { useDebounce } from '../../hooks/useDebounce';
import { isRangeValid, parseIsoDate } from '../../utils/date';
import { Field } from '../../components/ui/Field';
import type { CategoryDTO } from '../../services/api/types';

export type SortField = 'date' | 'amount' | 'description';
export type SortDirection = 'asc' | 'desc';

export interface TransactionFiltersValue {
  search?: string;
  startDate?: string;
  endDate?: string;
  categoryIds: string[];
  sortField: SortField;
  sortDirection: SortDirection;
}

export interface TransactionFiltersProps {
  value: TransactionFiltersValue;
  categories: CategoryDTO[];
  onChange: (filters: TransactionFiltersValue) => void;
}

const SORT_FIELDS: readonly SortField[] = ['date', 'amount', 'description'];
const SORT_DIRECTIONS: readonly SortDirection[] = ['asc', 'desc'];
const MAX_SEARCH_LEN = 255;

const EMPTY_FILTERS: TransactionFiltersValue = {
  categoryIds: [],
  sortField: 'date',
  sortDirection: 'desc',
};

const RANGE_ERROR = 'Start date must be on or before the end date.';
const DATE_HINT = 'Format: YYYY-MM-DD';
const SEARCH_HINT = "Search by description or exact amount. e.g., 'groceries' or '12.50'";
const CATEGORY_HINT = 'Select one or more categories to narrow results.';

interface SortOption {
  field: SortField;
  direction: SortDirection;
  label: string;
}

const SORT_OPTIONS: SortOption[] = [
  { field: 'date', direction: 'desc', label: 'Date (newest first)' },
  { field: 'date', direction: 'asc', label: 'Date (oldest first)' },
  { field: 'amount', direction: 'desc', label: 'Amount (high to low)' },
  { field: 'amount', direction: 'asc', label: 'Amount (low to high)' },
  { field: 'description', direction: 'asc', label: 'Description (A-Z)' },
  { field: 'description', direction: 'desc', label: 'Description (Z-A)' },
];

function normalizeSearch(s: string): string {
  return s.trim().replace(/\s+/g, ' ').slice(0, MAX_SEARCH_LEN);
}

function parseSortOption(value: string): { field: SortField; direction: SortDirection } | null {
  const [field, direction] = value.split(':');
  if (
    !SORT_FIELDS.includes(field as SortField) ||
    !SORT_DIRECTIONS.includes(direction as SortDirection)
  ) {
    return null;
  }
  return { field: field as SortField, direction: direction as SortDirection };
}

function isDateSet(value: string): boolean {
  return value.trim() !== '' && parseIsoDate(value) !== null;
}

export const TransactionFilters = ({
  value,
  categories,
  onChange,
}: TransactionFiltersProps) => {
  const initial = useRef<TransactionFiltersValue>(value);

  const [search, setSearch] = useState(initial.current.search ?? '');
  const [startDate, setStartDate] = useState(initial.current.startDate ?? '');
  const [endDate, setEndDate] = useState(initial.current.endDate ?? '');
  const [categoryIds, setCategoryIds] = useState<string[]>(initial.current.categoryIds ?? []);
  const [sortField, setSortField] = useState<SortField>(
    SORT_FIELDS.includes(initial.current.sortField) ? initial.current.sortField : 'date',
  );
  const [sortDirection, setSortDirection] = useState<SortDirection>(
    initial.current.sortDirection === 'asc' ? 'asc' : 'desc',
  );
  const [rangeError, setRangeError] = useState<string | null>(null);
  const [catOpen, setCatOpen] = useState(false);

  const debouncedSearch = useDebounce(search, 150);

  const lastSearchRef = useRef<string>(normalizeSearch(search));

  // Single emission path: every change converges here so only the latest
  // settled filter value ever reaches the parent (enables parent-side
  // AbortController cancellation; this component performs no fetch itself).
  const emit = useCallback(
    (filters: TransactionFiltersValue) => {
      lastSearchRef.current = normalizeSearch(debouncedSearch);
      onChange(filters);
    },
    [debouncedSearch, onChange],
  );

  const buildFilters = useCallback(
    (overrides?: Partial<TransactionFiltersValue>): TransactionFiltersValue => {
      const validCategoryIds = categoryIds.filter((id) =>
        categories.some((c) => c.id === id),
      );
      return {
        search: normalizeSearch(debouncedSearch) || undefined,
        startDate: startDate || undefined,
        endDate: endDate || undefined,
        categoryIds: validCategoryIds,
        sortField,
        sortDirection,
        ...overrides,
      };
    },
    [categoryIds, categories, debouncedSearch, startDate, endDate, sortField, sortDirection],
  );

  // Debounce applies to free-text only; selects emit immediately (see below).
  useEffect(() => {
    const normalized = normalizeSearch(debouncedSearch);
    if (normalized === lastSearchRef.current) return;
    emit(buildFilters());
  }, [debouncedSearch, buildFilters, emit]);

  const updateDates = (kind: 'start' | 'end', raw: string) => {
    const nextStart = kind === 'start' ? raw : startDate;
    const nextEnd = kind === 'end' ? raw : endDate;
    if (kind === 'start') setStartDate(raw);
    else setEndDate(raw);

    if (isDateSet(nextStart) && isDateSet(nextEnd)) {
      if (!isRangeValid(nextStart, nextEnd)) {
        // Invalid range: show inline error and block filter application.
        setRangeError(RANGE_ERROR);
        return;
      }
      setRangeError(null);
      emit(buildFilters({ startDate: nextStart, endDate: nextEnd }));
      return;
    }
    setRangeError(null);
    emit(buildFilters({ startDate: nextStart || undefined, endDate: nextEnd || undefined }));
  };

  const onSortChange = (raw: string) => {
    const parsed = parseSortOption(raw);
    if (!parsed) return;
    setSortField(parsed.field);
    setSortDirection(parsed.direction);
    emit(buildFilters(parsed));
  };

  const toggleCategory = (id: string) => {
    const next = categoryIds.includes(id)
      ? categoryIds.filter((x) => x !== id)
      : [...categoryIds, id];
    setCategoryIds(next);
    emit(
      buildFilters({
        categoryIds: next.filter((cid) => categories.some((c) => c.id === cid)),
      }),
    );
  };

  const clearFilters = () => {
    setSearch('');
    setStartDate('');
    setEndDate('');
    setCategoryIds([]);
    setSortField('date');
    setSortDirection('desc');
    setRangeError(null);
    setCatOpen(false);
    emit(EMPTY_FILTERS);
  };

  const activeCount =
    (search.trim() ? 1 : 0) +
    (startDate ? 1 : 0) +
    (endDate ? 1 : 0) +
    (categoryIds.length ? 1 : 0) +
    (sortField !== 'date' || sortDirection !== 'desc' ? 1 : 0);
  const hasActive = activeCount > 0;

  return (
    <section className="fms-filters" aria-label="Transaction filters">
      <div className="fms-filters__field fms-filters__search">
        <Field id="filter-search" label="Search" hint={SEARCH_HINT}>
          <input
            type="search"
            id="filter-search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            maxLength={MAX_SEARCH_LEN}
            autoComplete="off"
            inputMode="text"
          />
        </Field>
      </div>

      <fieldset className="fms-filters__field fms-filters__dates">
        <legend className="fms-filters__legend">Date range</legend>
        <div className="fms-filters__dates-row">
          <div className="fms-filters__control">
            <label className="fms-filters__label" htmlFor="filter-from">
              From
            </label>
            <input
              id="filter-from"
              type="date"
              value={startDate}
              onChange={(e) => updateDates('start', e.target.value)}
              aria-invalid={rangeError ? true : undefined}
              aria-describedby={`filter-date-hint${rangeError ? ' filter-range-error' : ''}`}
            />
          </div>
          <div className="fms-filters__control">
            <label className="fms-filters__label" htmlFor="filter-to">
              To
            </label>
            <input
              id="filter-to"
              type="date"
              value={endDate}
              onChange={(e) => updateDates('end', e.target.value)}
              aria-invalid={rangeError ? true : undefined}
              aria-describedby={`filter-date-hint${rangeError ? ' filter-range-error' : ''}`}
            />
          </div>
        </div>
        <p id="filter-date-hint" className="fms-filters__hint">
          {DATE_HINT}
        </p>
        {rangeError && (
          <p id="filter-range-error" className="fms-filters__error" role="alert" aria-live="assertive">
            <span aria-hidden="true">⚠</span> {rangeError}
          </p>
        )}
      </fieldset>

      <div className="fms-filters__field fms-filters__categories">
        <label id="filter-category-label" className="fms-filters__label" htmlFor="filter-category-toggle">
          Categories{categoryIds.length > 0 ? ` (${categoryIds.length})` : ''}
        </label>
        <button
          id="filter-category-toggle"
          type="button"
          className="fms-filters__toggle"
          aria-haspopup="listbox"
          aria-expanded={catOpen}
          onClick={() => setCatOpen((o) => !o)}
        >
          <span>{categoryIds.length > 0 ? `${categoryIds.length} selected` : 'All categories'}</span>
          <span aria-hidden="true">▾</span>
        </button>
        <p id="filter-category-hint" className="fms-filters__hint">
          {CATEGORY_HINT}
        </p>
        {catOpen && (
          <div
            role="listbox"
            id="filter-category-listbox"
            aria-labelledby="filter-category-label"
            className="fms-filters__listbox"
          >
            {categories.map((cat) => {
              const selected = categoryIds.includes(cat.id);
              return (
                <div
                  key={cat.id}
                  role="option"
                  aria-selected={selected}
                  className={`fms-filters__option${selected ? ' fms-filters__option--selected' : ''}`}
                  tabIndex={0}
                  onClick={() => toggleCategory(cat.id)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      toggleCategory(cat.id);
                    } else if (e.key === 'Escape') {
                      setCatOpen(false);
                    }
                  }}
                >
                  <span className="fms-filters__checkbox" aria-hidden="true">
                    {selected ? '✓' : ''}
                  </span>
                  <span className="fms-filters__option-label">{cat.name}</span>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <div className="fms-filters__field fms-filters__sort">
        <Field id="filter-sort" label="Sort by">
          <select
            id="filter-sort"
            value={`${sortField}:${sortDirection}`}
            onChange={(e) => onSortChange(e.target.value)}
          >
            {SORT_OPTIONS.map((o) => (
              <option key={o.label} value={`${o.field}:${o.direction}`}>
                {o.label}
              </option>
            ))}
          </select>
        </Field>
      </div>

      <button
        type="button"
        className={`fms-filters__clear${hasActive ? ' fms-filters__clear--active' : ''}`}
        onClick={clearFilters}
        disabled={!hasActive}
        aria-label="Clear all transaction filters"
      >
        <span aria-hidden="true">×</span>
        {hasActive ? `Clear filters (${activeCount})` : 'Clear filters'}
      </button>

      {FMS_FILTERS_CSS && typeof document !== 'undefined' ? (
        <FilterStyleSheet css={FMS_FILTERS_CSS} />
      ) : null}
    </section>
  );
};

const FMS_FILTERS_CSS = `
.fms-filters{display:flex;flex-wrap:wrap;align-items:flex-end;gap:12px;background:#f8fafc;border:1px solid #d1d5db;border-radius:12px;padding:16px;box-shadow:0 1px 2px rgba(0,0,0,0.05);}
.fms-filters__field{display:flex;flex-direction:column;gap:4px;min-width:0;flex:1 1 100%;}
.fms-filters__field.fms-filters__dates{border:0;margin:0;padding:0;}
.fms-filters__legend{font-size:14px;font-weight:500;color:#4b5563;padding:0;margin-bottom:4px;}
.fms-filters__dates-row{display:flex;gap:8px;flex-wrap:wrap;}
.fms-filters__control{display:flex;flex-direction:column;gap:4px;flex:1 1 100%;min-width:0;}
.fms-filters__label{font-size:14px;font-weight:500;color:#4b5563;}
.fms-filters__hint{font-size:12px;color:#6b7280;line-height:1.5;margin:0;}
.fms-filters__error{font-size:12px;color:#b91c1c;line-height:1.5;margin-top:4px;display:flex;align-items:center;gap:4px;opacity:1;transform:translateY(0);animation:fmsErrorIn 150ms ease-out;}
@keyframes fmsErrorIn{from{opacity:0;transform:translateY(4px);}to{opacity:1;transform:translateY(0);}}
.fms-filters input[type="search"],.fms-filters input[type="date"],.fms-filters select,.fms-filters__toggle{min-height:44px;border:1px solid #d1d5db;border-radius:8px;background:#fff;color:#1f2937;font-size:16px;line-height:1.5;padding:0 12px;width:100%;box-sizing:border-box;transition:border-color 150ms ease-out;}
.fms-filters input:hover,.fms-filters select:hover,.fms-filters__toggle:hover{border-color:#6b7280;}
.fms-filters input:focus-visible,.fms-filters select:focus-visible,.fms-filters__toggle:focus-visible,.fms-filters__option:focus-visible{outline:2px solid #2563eb;outline-offset:2px;}
.fms-filters input[type="date"][aria-invalid="true"]{border-color:#b91c1c;background:#fef2f2;}
.fms-filters__toggle{display:flex;align-items:center;justify-content:space-between;text-align:left;cursor:pointer;}
.fms-filters__listbox{position:absolute;z-index:10;display:flex;flex-direction:column;margin-top:4px;background:#fff;border:1px solid #d1d5db;border-radius:8px;box-shadow:0 4px 6px -1px rgba(0,0,0,0.1);max-height:220px;overflow-y:auto;padding:4px;}
.fms-filters__categories{position:relative;}
.fms-filters__option{display:flex;align-items:center;gap:8px;min-height:44px;padding:0 12px;cursor:pointer;border-radius:4px;transition:background-color 150ms ease-out;}
.fms-filters__option:hover{background:#f3f4f6;}
.fms-filters__option--selected{font-weight:600;color:#1d4ed8;}
.fms-filters__checkbox{width:16px;height:16px;display:inline-flex;align-items:center;justify-content:center;border:1px solid #9ca3af;border-radius:4px;font-size:12px;line-height:1;}
.fms-filters__option--selected .fms-filters__checkbox{background:#2563eb;border-color:#2563eb;color:#fff;}
.fms-filters__option-label{font-size:14px;letter-spacing:normal;}
.fms-filters__clear{min-width:96px;min-height:44px;display:inline-flex;align-items:center;justify-content:center;gap:4px;background:transparent;border:1px solid transparent;border-radius:8px;font-size:14px;font-weight:500;color:#6b7280;cursor:pointer;visibility:hidden;opacity:0;transform:translateY(2px);transition:opacity 150ms ease-out,transform 150ms ease-out,color 150ms ease-out;}
.fms-filters__clear--active{visibility:visible;opacity:1;transform:translateY(0);color:#2563eb;font-weight:600;}
.fms-filters__clear--active:hover{text-decoration:underline;color:#1d4ed8;}
.fms-filters__clear:focus-visible{outline:2px solid #2563eb;outline-offset:2px;}
.fms-filters__clear:disabled{cursor:default;}
@media (min-width:768px){
  .fms-filters{gap:16px;}
  .fms-filters__field{flex:0 0 calc(50% - 8px);}
  .fms-filters__field.fms-filters__search{flex:1 1 100%;}
  .fms-filters__control{flex:1 1 45%;}
}
@media (min-width:1024px){
  .fms-filters{flex-wrap:nowrap;align-items:flex-end;gap:24px;}
  .fms-filters__field{flex:0 0 auto;}
  .fms-filters__field.fms-filters__search{flex:1 1 auto;min-width:240px;}
  .fms-filters__field.fms-filters__dates{width:320px;}
  .fms-filters__field.fms-filters__categories{width:200px;}
  .fms-filters__field.fms-filters__sort{width:180px;}
  .fms-filters__clear{margin-left:auto;}
}
`;

function FilterStyleSheet({ css }: { css: string }) {
  const htmlId = 'fms-transaction-filters-style';
  // Static styles injected once; no user-derived data is ever placed inside.
  useEffect(() => {
    if (typeof document === 'undefined') return;
    if (document.getElementById(htmlId)) return;
    const style = document.createElement('style');
    style.id = htmlId;
    style.textContent = css;
    document.head.appendChild(style);
  }, [css]);
  return null;
}