import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { DisputesApi } from '../core/disputes.api';
import { formatCountdown, formatMinor, MoneyPipe } from '../core/money';
import { EvidencePanel } from './evidence-panel';
import type { LedgerPosting } from '../core/api.types';

@Component({
  selector: 'app-dispute-detail',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [MoneyPipe, DatePipe, EvidencePanel],
  templateUrl: './dispute-detail.html',
  styleUrl: './dispute-detail.css',
  host: {
    role: 'complementary',
    '(keydown.escape)': 'api.closeDetail()',
    tabindex: '-1',
  },
})
export class DisputeDetail {
  protected readonly api = inject(DisputesApi);

  protected readonly dispute = computed(() => this.api.detail.value());
  protected readonly isLoading = computed(() => this.api.detail.isLoading());

  protected readonly countdown = computed(() => {
    const d = this.dispute();
    if (!d) return '';
    return d.resolved_at ? 'closed' : formatCountdown(d.seconds_to_deadline);
  });

  protected readonly debits = computed(() =>
    (this.dispute()?.ledger_postings ?? []).filter((p) => p.direction === 'debit'),
  );

  protected readonly credits = computed(() =>
    (this.dispute()?.ledger_postings ?? []).filter((p) => p.direction === 'credit'),
  );

  /**
   * Debits minus credits, in minor units. It is always zero - the database
   * refuses to commit anything else - and showing it is how you can see that
   * rather than take it on faith.
   */
  protected readonly balanceCheck = computed(() => {
    const sum = (postings: LedgerPosting[]) =>
      postings.reduce((total, p) => total + p.amount.amount_minor, 0);
    return sum(this.debits()) - sum(this.credits());
  });

  protected readonly refunded = computed(() => {
    const d = this.dispute();
    if (!d) return null;
    return formatMinor(d.refunded_minor, d.amount.currency);
  });
}
