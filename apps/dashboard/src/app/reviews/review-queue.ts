import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { ReviewsApi } from '../core/reviews.api';
import { formatCountdown, MoneyPipe, urgencyOf } from '../core/money';
import type { ReviewRow } from '../core/api.types';
import { standingOf, type Standing } from './standing';

/**
 * The queue, ordered by deadline rather than arrival.
 *
 * The oldest draft is not the most urgent one, and a list sorted by age quietly
 * lets the tightest deadline sit behind three comfortable ones.
 */
@Component({
  selector: 'app-review-queue',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [MoneyPipe],
  templateUrl: './review-queue.html',
  styleUrl: './review-queue.css',
})
export class ReviewQueue {
  protected readonly api = inject(ReviewsApi);

  protected standing(row: ReviewRow): Standing {
    return standingOf(row.outcome, row.recommendation);
  }

  protected select(row: ReviewRow): void {
    this.api.select(row.run_id);
  }

  protected countdown(seconds: number): string {
    return formatCountdown(seconds);
  }

  protected urgency(seconds: number): string {
    return urgencyOf(seconds, null);
  }
}
