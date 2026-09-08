import { computed, effect, inject, Injectable, signal } from '@angular/core';
import { toObservable, toSignal } from '@angular/core/rxjs-interop';
import { Router } from '@angular/router';
import { debounceTime, distinctUntilChanged } from 'rxjs';
import type { DisputeKind, DisputeState } from './api.types';

export type SortKey = 'opened_at' | 'deadline_at' | 'amount' | 'state' | 'merchant' | 'kind';
export type SortDir = 'asc' | 'desc';

const PAGE_SIZE = 50;

/**
 * Every filter the dashboard can express, held as signals and mirrored into the
 * URL.
 *
 * The URL is the point: an operator who finds something worth escalating sends
 * a link, and the person who opens it sees the same rows. State kept only in
 * component fields cannot be shared, bookmarked, or reloaded.
 */
@Injectable({ providedIn: 'root' })
export class FiltersStore {
  private readonly router = inject(Router);

  readonly searchInput = signal('');
  readonly merchants = signal<readonly string[]>([]);
  readonly states = signal<readonly DisputeState[]>([]);
  readonly kinds = signal<readonly DisputeKind[]>([]);
  readonly dueWithin = signal<string | null>(null);
  /**
   * Defaults to true. Sorting by deadline across *every* dispute puts the
   * oldest resolved ones first, which is the least useful thing this page
   * could open on. The job is "what runs out next", so the landing state is
   * the open queue.
   */
  readonly openOnly = signal(true);
  readonly sort = signal<SortKey>('deadline_at');
  readonly dir = signal<SortDir>('asc');
  readonly offset = signal(0);

  /**
   * The search box debounced.
   *
   * Signals have no notion of time, so this drops into RxJS for the one thing
   * it is genuinely better at and comes straight back. Without it every
   * keystroke is a query against half a million rows.
   */
  private readonly debouncedSearch = toSignal(
    toObservable(this.searchInput).pipe(debounceTime(250), distinctUntilChanged()),
    { initialValue: '' },
  );

  readonly search = computed(() => this.debouncedSearch().trim());

  /**
   * States a dispute can never be in while it is still on the clock. Selecting
   * one of these with "Still on the clock" on asks for something that cannot
   * exist, and the result is an empty table that looks like missing data.
   */
  private readonly settledStates: readonly DisputeState[] = [
    'represented',
    'refunded',
    'won',
    'lost',
    'expired',
  ];

  /**
   * The filters currently contradict each other: every selected state is one a
   * dispute reaches after it stops being open, and the open-only filter is on.
   *
   * Worth naming rather than leaving the operator to work out. "No disputes
   * match these filters" is true and useless when 708 of them are one click
   * away, and the click is a checkbox nobody remembers is ticked.
   */
  readonly openOnlyExcludesEverything = computed(
    () =>
      this.openOnly() &&
      this.states().length > 0 &&
      this.states().every((state) => this.settledStates.includes(state)),
  );

  /** True when anything is narrowing the result set. */
  readonly isFiltered = computed(
    () =>
      this.search() !== '' ||
      this.merchants().length > 0 ||
      this.states().length > 0 ||
      this.kinds().length > 0 ||
      this.dueWithin() !== null ||
      this.openOnly(),
  );

  /** The query string sent to /api/disputes. */
  readonly listQuery = computed(() => {
    const params = this.baseParams();
    params.set('sort', this.sort());
    params.set('dir', this.dir());
    params.set('limit', String(PAGE_SIZE));
    params.set('offset', String(this.offset()));
    return params.toString();
  });

  /**
   * The summary uses the same filters minus paging and sorting: the tiles
   * describe the whole filtered set, not the page being looked at.
   */
  readonly summaryQuery = computed(() => this.baseParams().toString());

  readonly pageSize = PAGE_SIZE;

  constructor() {
    this.restoreFromUrl();

    // Mirror state into the URL. replaceUrl so a filter change is not a browser
    // history entry - twenty back presses to leave a dashboard is a bug.
    effect(() => {
      const query = this.listQuery();
      void this.router.navigate([], {
        queryParams: Object.fromEntries(new URLSearchParams(query)),
        replaceUrl: true,
      });
    });
  }

  private baseParams(): URLSearchParams {
    const params = new URLSearchParams();
    const add = (key: string, value: string) => {
      if (value !== '') params.set(key, value);
    };

    add('q', this.search());
    add('merchant', this.merchants().join(','));
    add('state', this.states().join(','));
    add('kind', this.kinds().join(','));
    if (this.dueWithin()) add('due_within', this.dueWithin()!);
    if (this.openOnly()) add('open', 'true');
    return params;
  }

  /** Any filter change resets to page one; page 7 of a different result set is meaningless. */
  private resetPage(): void {
    this.offset.set(0);
  }

  setSearch(value: string): void {
    this.searchInput.set(value);
    this.resetPage();
  }

  toggle<T extends string>(list: ReturnType<typeof signal<readonly T[]>>, value: T): void {
    const current = list();
    list.set(current.includes(value) ? current.filter((v) => v !== value) : [...current, value]);
    this.resetPage();
  }

  setDueWithin(value: string | null): void {
    this.dueWithin.set(value);
    // A deadline window only means something for disputes still on the clock.
    if (value !== null) this.openOnly.set(true);
    this.resetPage();
  }

  setOpenOnly(value: boolean): void {
    this.openOnly.set(value);
    if (!value) this.dueWithin.set(null);
    this.resetPage();
  }

  /** Clicking the active column flips direction; a new column starts ascending. */
  sortBy(key: SortKey): void {
    if (this.sort() === key) {
      this.dir.update((d) => (d === 'asc' ? 'desc' : 'asc'));
    } else {
      this.sort.set(key);
      this.dir.set(key === 'deadline_at' ? 'asc' : 'desc');
    }
    this.resetPage();
  }

  nextPage(total: number): void {
    if (this.offset() + PAGE_SIZE < total) this.offset.update((o) => o + PAGE_SIZE);
  }

  previousPage(): void {
    this.offset.update((o) => Math.max(0, o - PAGE_SIZE));
  }

  clear(): void {
    this.searchInput.set('');
    this.merchants.set([]);
    this.states.set([]);
    this.kinds.set([]);
    this.dueWithin.set(null);
    this.openOnly.set(false);
    this.resetPage();
  }

  private restoreFromUrl(): void {
    const params = new URLSearchParams(window.location.search);

    // A link someone shared describes its filters exactly, including the ones
    // it deliberately left off. Only a bare URL gets the defaults.
    if ([...params.keys()].length === 0) return;

    const csv = (key: string) => (params.get(key) ?? '').split(',').filter(Boolean);

    if (params.get('q')) this.searchInput.set(params.get('q')!);
    this.merchants.set(csv('merchant'));
    this.states.set(csv('state') as DisputeState[]);
    this.kinds.set(csv('kind') as DisputeKind[]);
    this.dueWithin.set(params.get('due_within'));
    this.openOnly.set(params.get('open') === 'true');
    if (params.get('sort')) this.sort.set(params.get('sort') as SortKey);
    if (params.get('dir') === 'desc') this.dir.set('desc');
    const offset = Number(params.get('offset'));
    if (Number.isInteger(offset) && offset >= 0) this.offset.set(offset);
  }
}
