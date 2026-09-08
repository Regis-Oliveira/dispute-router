import { computed, inject, Injectable, signal } from '@angular/core';
import { httpResource, HttpClient } from '@angular/common/http';
import { firstValueFrom } from 'rxjs';
import { API_BASE } from './api.config';
import type { ReviewDetail, ReviewRow } from './api.types';

/**
 * The review queue.
 *
 * This is the only service in the app that writes. Everything else reads a
 * dashboard; `decide` moves a dispute, and submitting is the only path in the
 * whole system that reaches 'represented' - the agent deliberately cannot get
 * there on its own.
 */
@Injectable({ providedIn: 'root' })
export class ReviewsApi {
  private readonly base = inject(API_BASE);
  private readonly http = inject(HttpClient);

  readonly queue = httpResource<ReviewRow[]>(() => `${this.base}/api/reviews`, {
    defaultValue: [],
  });

  readonly selectedId = signal<number | null>(null);

  readonly detail = httpResource<ReviewDetail | undefined>(
    () => {
      const id = this.selectedId();
      return id === null ? undefined : `${this.base}/api/reviews/${id}`;
    },
    { defaultValue: undefined },
  );

  /**
   * Who is deciding. There is no login in this app, so this is a name typed
   * into a box - it records who somebody CLAIMED to be, which is enough for a
   * local dashboard and nowhere near enough for a deployed one.
   */
  readonly reviewer = signal(localStorage.getItem('reviewer') ?? '');

  /** Set while a decision is in flight, so the buttons cannot be pressed twice. */
  readonly deciding = signal(false);

  /** The last thing that went wrong, shown next to the buttons rather than in a toast. */
  readonly error = signal<string | null>(null);

  readonly rows = computed(() => this.queue.value());
  readonly isLoading = computed(() => this.queue.isLoading());

  select(runId: number): void {
    this.error.set(null);
    this.selectedId.set(runId);
  }

  close(): void {
    this.selectedId.set(null);
  }

  setReviewer(name: string): void {
    this.reviewer.set(name);
    localStorage.setItem('reviewer', name);
  }

  /**
   * Records a decision.
   *
   * A 409 means somebody else got there first, and it is reported as exactly
   * that rather than as a generic failure: the reviewer's page is stale, not
   * broken, and telling them otherwise sends them looking for the wrong
   * problem. Either way the queue is reloaded, because whatever the outcome the
   * list in front of them is now out of date.
   */
  async decide(runId: number, decision: 'submitted' | 'discarded'): Promise<boolean> {
    const reviewer = this.reviewer().trim();
    if (!reviewer) {
      this.error.set('Enter your name before deciding.');
      return false;
    }

    this.deciding.set(true);
    this.error.set(null);
    try {
      await firstValueFrom(
        this.http.post(`${this.base}/api/reviews/${runId}/decision`, { decision, reviewer }),
      );
      this.selectedId.set(null);
      this.queue.reload();
      return true;
    } catch (err: unknown) {
      const status = (err as { status?: number }).status;
      this.error.set(
        status === 409
          ? 'Someone already decided this one. Reloading the queue.'
          : 'Could not record the decision.',
      );
      this.queue.reload();
      return false;
    } finally {
      this.deciding.set(false);
    }
  }
}
