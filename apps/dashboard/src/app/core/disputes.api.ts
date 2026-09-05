import { computed, inject, Injectable, signal } from '@angular/core';
import { httpResource } from '@angular/common/http';
import { API_BASE } from './api.config';
import { FiltersStore } from './filters.store';
import type { DisputeDetail, DisputeList, MerchantOption, Summary } from './api.types';

const EMPTY_LIST: DisputeList = { rows: [], page: { offset: 0, limit: 0, total: 0 }, took_ms: 0 };

const EMPTY_SUMMARY: Summary = {
  states: [],
  open_value: [],
  deadlines: [],
  daily: [],
  took_ms: 0,
};

/**
 * The read model, as resources.
 *
 * httpResource takes a *reactive* URL: the moment a filter signal changes the
 * computed URL changes, the request is reissued and the previous one is
 * aborted. There is no subscribe, no unsubscribe, and no chance of a slow
 * earlier response landing after a faster later one and painting stale rows.
 */
@Injectable({ providedIn: 'root' })
export class DisputesApi {
  private readonly base = inject(API_BASE);
  private readonly filters = inject(FiltersStore);

  readonly list = httpResource<DisputeList>(
    () => `${this.base}/api/disputes?${this.filters.listQuery()}`,
    { defaultValue: EMPTY_LIST },
  );

  readonly summary = httpResource<Summary>(
    () => `${this.base}/api/summary?${this.filters.summaryQuery()}`,
    { defaultValue: EMPTY_SUMMARY },
  );

  readonly merchants = httpResource<MerchantOption[]>(() => `${this.base}/api/merchants`, {
    defaultValue: [],
  });

  /** Which dispute the detail panel is showing, or null when it is closed. */
  readonly selectedId = signal<number | null>(null);

  readonly detail = httpResource<DisputeDetail | undefined>(
    () => {
      const id = this.selectedId();
      // Returning undefined puts the resource in Idle and issues no request,
      // which is how "nothing is selected" is expressed.
      return id === null ? undefined : `${this.base}/api/disputes/${id}`;
    },
    { defaultValue: undefined },
  );

  readonly rows = computed(() => this.list.value().rows);
  readonly page = computed(() => this.list.value().page);
  readonly tookMs = computed(() => this.list.value().took_ms);

  readonly isLoading = computed(() => this.list.isLoading());
  readonly error = computed(() => this.list.error());

  /** The href for the CSV export, carrying whatever filters are active. */
  readonly exportUrl = computed(
    () => `${this.base}/api/disputes.csv?${this.filters.summaryQuery()}`,
  );

  select(id: number): void {
    this.selectedId.set(id);
  }

  closeDetail(): void {
    this.selectedId.set(null);
  }

  refresh(): void {
    this.list.reload();
    this.summary.reload();
  }
}
