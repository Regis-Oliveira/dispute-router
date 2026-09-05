import { DestroyRef, inject, Injectable, signal } from '@angular/core';
import { API_BASE } from './api.config';
import type { LiveEvent } from './api.types';

const MAX_EVENTS = 25;

/**
 * The live arrivals feed, over Server-Sent Events.
 *
 * EventSource rather than a WebSocket because the data only travels one way and
 * EventSource reconnects on its own. Nothing here touches change detection: the
 * app is zoneless, so a signal set from inside a network callback is exactly as
 * good as one set from a click.
 */
@Injectable({ providedIn: 'root' })
export class LiveFeed {
  private readonly base = inject(API_BASE);

  private readonly events = signal<readonly LiveEvent[]>([]);
  private readonly status = signal<'connecting' | 'live' | 'offline'>('connecting');

  readonly recent = this.events.asReadonly();
  readonly connection = this.status.asReadonly();

  /** Counts arrivals since the page was opened, so the table can offer a refresh. */
  private readonly arrived = signal(0);
  readonly sinceLoad = this.arrived.asReadonly();

  constructor() {
    const source = new EventSource(`${this.base}/api/stream`);

    source.addEventListener('open', () => this.status.set('live'));

    source.addEventListener('dispute', (event) => {
      try {
        const parsed = JSON.parse((event as MessageEvent<string>).data) as LiveEvent;
        // Newest first, capped. An unbounded feed on a busy day is a memory
        // leak with a scrollbar.
        this.events.update((current) => [parsed, ...current].slice(0, MAX_EVENTS));
        this.arrived.update((n) => n + 1);
      } catch {
        // A malformed frame is not worth breaking the feed over.
      }
    });

    // EventSource retries by itself; this only reflects that in the UI so the
    // operator knows the feed is stale rather than quiet.
    source.addEventListener('error', () => this.status.set('offline'));

    inject(DestroyRef).onDestroy(() => source.close());
  }

  acknowledge(): void {
    this.arrived.set(0);
  }
}
