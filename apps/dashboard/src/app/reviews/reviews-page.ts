import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { ReviewsApi } from '../core/reviews.api';
import { DraftPanel } from './draft-panel';
import { ReviewQueue } from './review-queue';

/**
 * The review queue: what the agent drafted, and the one screen where a person
 * can send it.
 *
 * The shell only. The list is ReviewQueue and the decision is DraftPanel, split
 * the same way the disputes page is - each piece carries its own styles, which
 * is what keeps any one of them under the component style budget.
 */
@Component({
  selector: 'app-reviews-page',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ReviewQueue, DraftPanel],
  templateUrl: './reviews-page.html',
  styleUrl: './reviews-page.css',
})
export class ReviewsPage {
  protected readonly api = inject(ReviewsApi);
  protected readonly hasSelection = computed(() => this.api.selectedId() !== null);

  protected onReviewer(event: Event): void {
    this.api.setReviewer((event.target as HTMLInputElement).value);
  }
}
