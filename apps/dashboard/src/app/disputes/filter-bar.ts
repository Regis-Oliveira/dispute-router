import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { DisputesApi } from '../core/disputes.api';
import { FiltersStore } from '../core/filters.store';
import type { DisputeKind, DisputeState } from '../core/api.types';

const STATES: readonly DisputeState[] = [
  'received',
  'resolving',
  'represented',
  'refunded',
  'won',
  'lost',
  'expired',
];

const KINDS: readonly DisputeKind[] = ['alert', 'chargeback'];

const DUE_WINDOWS = [
  { value: '6h', label: 'Next 6h' },
  { value: '24h', label: 'Next 24h' },
  { value: '72h', label: 'Next 72h' },
] as const;

@Component({
  selector: 'app-filter-bar',
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './filter-bar.html',
  styleUrl: './filter-bar.css',
})
export class FilterBar {
  protected readonly filters = inject(FiltersStore);
  protected readonly api = inject(DisputesApi);

  protected readonly states = STATES;
  protected readonly kinds = KINDS;
  protected readonly dueWindows = DUE_WINDOWS;

  protected readonly merchantOptions = computed(() => this.api.merchants.value());

  protected onSearch(event: Event): void {
    this.filters.setSearch((event.target as HTMLInputElement).value);
  }

  protected toggleState(state: DisputeState): void {
    this.filters.toggle(this.filters.states, state);
  }

  protected toggleKind(kind: DisputeKind): void {
    this.filters.toggle(this.filters.kinds, kind);
  }

  protected toggleMerchant(id: string): void {
    this.filters.toggle(this.filters.merchants, id);
  }

  protected setDue(value: string): void {
    // Clicking the active window clears it, so the control is its own undo.
    this.filters.setDueWithin(this.filters.dueWithin() === value ? null : value);
  }
}
