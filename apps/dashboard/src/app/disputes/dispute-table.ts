import { DatePipe, DecimalPipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { DisputesApi } from '../core/disputes.api';
import { FiltersStore, type SortKey } from '../core/filters.store';
import { formatCountdown, MoneyPipe, urgencyOf } from '../core/money';
import type { DisputeRow } from '../core/api.types';

interface Column {
  readonly key: SortKey | null;
  readonly label: string;
  readonly align?: 'right';
}

const COLUMNS: readonly Column[] = [
  { key: 'deadline_at', label: 'Deadline' },
  { key: 'merchant', label: 'Merchant' },
  { key: null, label: 'Reference' },
  { key: 'kind', label: 'Kind' },
  { key: null, label: 'Reason' },
  { key: 'state', label: 'State' },
  { key: 'amount', label: 'Amount', align: 'right' },
  { key: 'opened_at', label: 'Opened' },
];

@Component({
  selector: 'app-dispute-table',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [MoneyPipe, DatePipe, DecimalPipe],
  templateUrl: './dispute-table.html',
  styleUrl: './dispute-table.css',
})
export class DisputeTable {
  protected readonly api = inject(DisputesApi);
  protected readonly filters = inject(FiltersStore);
  protected readonly columns = COLUMNS;

  protected readonly page = computed(() => this.api.page());

  protected readonly rangeStart = computed(() =>
    this.page().total === 0 ? 0 : this.page().offset + 1,
  );

  protected readonly rangeEnd = computed(() =>
    Math.min(this.page().offset + this.page().limit, this.page().total),
  );

  protected readonly hasPrevious = computed(() => this.page().offset > 0);

  protected readonly hasNext = computed(
    () => this.page().offset + this.page().limit < this.page().total,
  );

  protected countdown(row: DisputeRow): string {
    // A resolved dispute has no clock left to run.
    return row.resolved_at ? '—' : formatCountdown(row.seconds_to_deadline);
  }

  protected urgency(row: DisputeRow): string {
    return urgencyOf(row.seconds_to_deadline, row.resolved_at);
  }

  protected sortIndicator(key: SortKey | null): string {
    if (key === null || this.filters.sort() !== key) return '';
    return this.filters.dir() === 'asc' ? '↑' : '↓';
  }

  protected ariaSort(key: SortKey | null): 'ascending' | 'descending' | 'none' | null {
    if (key === null) return null;
    if (this.filters.sort() !== key) return 'none';
    return this.filters.dir() === 'asc' ? 'ascending' : 'descending';
  }
}
