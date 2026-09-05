import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { formatMinor } from '../core/money';
import type { DeadlineBucket, Exposure, StateCount } from '../core/api.types';

const BUCKET_LABEL: Record<string, string> = {
  overdue: 'Overdue',
  '6h': 'Under 6h',
  '24h': 'Under 24h',
  '72h': 'Under 72h',
  later: 'Later',
};

@Component({
  selector: 'app-summary-tiles',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="tiles">
      <div class="tile tile--urgent">
        <span class="tile__k">On the clock</span>
        <span class="tile__v">{{ openCount() }}</span>
        <span class="tile__note">{{ overdue() }} past deadline</span>
      </div>

      @for (money of exposure(); track money.currency) {
        <div class="tile">
          <span class="tile__k">At risk · {{ money.currency }}</span>
          <span class="tile__v tile__v--money">{{ formatted(money) }}</span>
          <span class="tile__note">{{ money.count }} open disputes</span>
        </div>
      }

      <div class="tile">
        <span class="tile__k">Alerts vs chargebacks</span>
        <span class="tile__v">{{ alerts() }} <span class="tile__slash">/</span> {{ chargebacks() }}</span>
        <span class="tile__note">refundable window vs already lost</span>
      </div>
    </div>

    <div class="deadline-bar" role="img" [attr.aria-label]="deadlineLabel()">
      @for (bucket of deadlines(); track bucket.bucket) {
        <div class="deadline-bar__seg" [attr.data-bucket]="bucket.bucket"
             [style.flex-grow]="bucket.count || 0.001">
          <span class="deadline-bar__n">{{ bucket.count }}</span>
          <span class="deadline-bar__l">{{ label(bucket.bucket) }}</span>
        </div>
      }
    </div>
  `,
  styleUrl: './summary-tiles.css',
})
export class SummaryTiles {
  readonly states = input.required<StateCount[]>();
  readonly exposure = input.required<Exposure[]>();
  readonly deadlines = input.required<DeadlineBucket[]>();

  protected readonly openCount = computed(() =>
    this.states()
      .filter((s) => s.state === 'received' || s.state === 'resolving')
      .reduce((total, s) => total + s.count, 0),
  );

  protected readonly overdue = computed(
    () => this.deadlines().find((b) => b.bucket === 'overdue')?.count ?? 0,
  );

  protected readonly alerts = computed(() =>
    this.states().filter((s) => s.kind === 'alert').reduce((t, s) => t + s.count, 0),
  );

  protected readonly chargebacks = computed(() =>
    this.states().filter((s) => s.kind === 'chargeback').reduce((t, s) => t + s.count, 0),
  );

  protected readonly deadlineLabel = computed(() =>
    this.deadlines().map((b) => `${this.label(b.bucket)}: ${b.count}`).join(', '),
  );

  protected formatted(money: Exposure): string {
    return formatMinor(money.amount_minor, money.currency);
  }

  protected label(bucket: string): string {
    return BUCKET_LABEL[bucket] ?? bucket;
  }
}
