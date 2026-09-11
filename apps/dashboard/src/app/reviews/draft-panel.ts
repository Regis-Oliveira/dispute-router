import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { MoneyPipe } from '../core/money';
import { ReviewsApi } from '../core/reviews.api';
import { canSubmit, formatCost, needsOverrideWarning, standingOf } from './standing';

/**
 * One draft, and the decision about it.
 *
 * A run that reached this queue by escalation was REJECTED by the verifier, and
 * nothing here may let it look like one that passed. The findings sit above the
 * letter, the confirmation says so in words, and approving it takes an extra
 * click. The whole system is built so nothing reaches a card network
 * unexamined; a screen that flattens "checked" and "refused" into one ready
 * state undoes all of it at the last step.
 */
@Component({
  selector: 'app-draft-panel',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [MoneyPipe],
  templateUrl: './draft-panel.html',
  styleUrl: './draft-panel.css',
})
export class DraftPanel {
  protected readonly api = inject(ReviewsApi);

  /** Which action is one click from happening, or null. */
  protected readonly confirming = signal<'submitted' | null>(null);

  protected readonly detail = computed(() => this.api.detail.value());
  protected readonly loading = computed(() => this.api.detail.isLoading());

  protected readonly standing = computed(() => {
    const run = this.detail();
    return run ? standingOf(run.outcome, run.recommendation) : 'checked';
  });
  protected readonly wasRejected = computed(() => needsOverrideWarning(this.standing()));
  protected readonly noRepresentment = computed(() => this.standing() === 'no-case');
  protected readonly noDraft = computed(() => !canSubmit(this.standing()));
  /** The window has closed; the sweeper will record the dispute as expired. */
  protected readonly expired = computed(() => (this.detail()?.dispute.seconds_to_deadline ?? 0) < 0);
  protected readonly canDecide = computed(
    () => !this.api.deciding() && this.api.reviewer().trim().length > 0 && !this.detail()?.decided,
  );

  /**
   * Approving is irreversible in the way that matters - the letter goes to a
   * card network - so it is two clicks. Discarding only returns the dispute to
   * the queue, so it is one.
   */
  protected ask(decision: 'submitted' | 'discarded'): void {
    if (decision === 'discarded') {
      void this.decide('discarded');
      return;
    }
    this.confirming.set('submitted');
  }

  protected cancel(): void {
    this.confirming.set(null);
  }

  protected async decide(decision: 'submitted' | 'discarded'): Promise<void> {
    const id = this.api.selectedId();
    if (id === null) return;
    this.confirming.set(null);
    await this.api.decide(id, decision);
  }

  protected dollars = formatCost;
}
