import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { DecisionsApi } from '../core/decisions.api';
import { MoneyPipe } from '../core/money';
import { formatCost } from './standing';

/**
 * What people decided, and where they disagreed with the agent.
 *
 * The pair of columns is the point: the agent's verdict beside the human's
 * decision. A run the verifier rejected and a person submitted anyway is an
 * override, and a list of overrides is the most actionable thing this system
 * can produce about itself - it is how you find out whether reviews are real
 * or whether somebody is clicking through.
 */
@Component({
  selector: 'app-decisions-page',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, MoneyPipe],
  templateUrl: './decisions-page.html',
  styleUrl: './decisions-page.css',
})
export class DecisionsPage {
  protected readonly api = inject(DecisionsApi);
  protected readonly cost = formatCost;

  protected readonly overrideCount = computed(
    () => this.api.rows().filter((row) => row.override).length,
  );

  protected readonly isFiltered = computed(
    () => this.api.reviewer() !== null || this.api.decision() !== null || this.api.onlyOverrides(),
  );

  protected when(iso: string): string {
    return new Date(iso).toLocaleString(undefined, {
      day: '2-digit',
      month: 'short',
      hour: '2-digit',
      minute: '2-digit',
    });
  }
}
