import { computed, effect, inject, Injectable, signal } from '@angular/core';
import { httpResource } from '@angular/common/http';
import { API_BASE } from './api.config';
import type { DecisionList } from './api.types';

const EMPTY: DecisionList = {
  rows: [],
  page: { offset: 0, limit: 0, total: 0 },
  reviewers: [],
  took_ms: 0,
};

/**
 * The decision history.
 *
 * Read-only, and deliberately separate from ReviewsApi: the queue is work to be
 * done and this is a record of work done. Sharing a service would mean one
 * reload invalidating the other for no reason.
 */
@Injectable({ providedIn: 'root' })
export class DecisionsApi {
  private readonly base = inject(API_BASE);

  /**
   * The reviewer is held as an exact identity, never a search string. It is a
   * typed name today and a user id once there is a login - the filter is
   * written for the second case so that arriving at it changes only how the
   * name is displayed.
   */
  readonly reviewer = signal<string | null>(null);
  readonly decision = signal<'submitted' | 'discarded' | null>(null);
  readonly onlyOverrides = signal(false);

  private readonly query = computed(() => {
    const params = new URLSearchParams();
    const who = this.reviewer();
    if (who !== null) params.set('reviewer', who);
    if (this.decision() !== null) params.set('decision', this.decision()!);
    if (this.onlyOverrides()) params.set('overrides', 'true');
    return params.toString();
  });

  private readonly list = httpResource<DecisionList>(
    () => `${this.base}/api/decisions?${this.query()}`,
    { defaultValue: EMPTY },
  );

  /**
   * The last answer that actually arrived.
   *
   * Changing the request puts the resource back into loading and its value back
   * to the default, so reading it directly makes the table empty itself on
   * every click and fill again a moment later. Against a local API that is a
   * flash; against a slow one it is a table that keeps disappearing. Holding
   * the previous answer means a filter change re-renders rows in place, and
   * isLoading only dims them.
   */
  private readonly held = signal<DecisionList>(EMPTY);

  constructor() {
    effect(() => {
      const value = this.list.value();
      if (!this.list.isLoading()) this.held.set(value);
    });
  }

  readonly rows = computed(() => this.held().rows);
  readonly total = computed(() => this.held().page.total);
  readonly reviewers = computed(() => this.held().reviewers);
  readonly isLoading = computed(() => this.list.isLoading());

  /** Clicking the active value clears it, so a filter is never a one-way door. */
  toggleReviewer(who: string): void {
    this.reviewer.update((current) => (current === who ? null : who));
  }

  toggleDecision(value: 'submitted' | 'discarded'): void {
    this.decision.update((current) => (current === value ? null : value));
  }

  toggleOverrides(): void {
    this.onlyOverrides.update((v) => !v);
  }

  clear(): void {
    this.reviewer.set(null);
    this.decision.set(null);
    this.onlyOverrides.set(false);
  }
}
