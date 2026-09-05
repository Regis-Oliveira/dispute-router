import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { DisputesApi } from '../core/disputes.api';
import { FilterBar } from './filter-bar';
import { DisputeDetail } from './dispute-detail';
import { DisputeTable } from './dispute-table';
import { LiveFeedBar } from './live-feed';
import { SummaryTiles } from './summary-tiles';

@Component({
  selector: 'app-disputes-page',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FilterBar, DisputeTable, DisputeDetail, SummaryTiles, LiveFeedBar],
  templateUrl: './disputes-page.html',
  styleUrl: './disputes-page.css',
})
export class DisputesPage {
  protected readonly api = inject(DisputesApi);

  protected readonly summary = computed(() => this.api.summary.value());
  protected readonly hasSelection = computed(() => this.api.selectedId() !== null);
}
