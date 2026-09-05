import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { DisputesApi } from '../core/disputes.api';
import { LiveFeed } from '../core/live.feed';
import { formatMinor } from '../core/money';
import type { LiveEvent } from '../core/api.types';

@Component({
  selector: 'app-live-feed',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="live">
      <span class="live__dot" [attr.data-status]="feed.connection()" aria-hidden="true"></span>
      <span class="live__label">{{ feed.connection() }}</span>

      @if (feed.sinceLoad() > 0) {
        <!-- New rows are announced, never injected. Silently reordering the
             table under someone who is reading it is how you lose their place
             and their trust in what they are looking at. -->
        <button type="button" class="live__refresh" (click)="refresh()">
          {{ feed.sinceLoad() }} new — refresh
        </button>
      }

      @if (feed.recent().length > 0) {
        <span class="live__last">latest {{ describe(feed.recent()[0]) }}</span>
      }
    </div>
  `,
  styles: `
    .live {
      display: flex;
      align-items: center;
      gap: 9px;
      font-size: 0.78rem;
      color: var(--ink-muted);
    }

    .live__dot {
      width: 7px;
      height: 7px;
      border-radius: 50%;
      background: var(--rule-strong);
      flex: none;
    }

    .live__dot[data-status='live'] { background: var(--ok); }
    .live__dot[data-status='offline'] { background: var(--crit); }

    .live__label { text-transform: uppercase; letter-spacing: 0.08em; font-size: 0.68rem; }

    .live__refresh {
      font: inherit;
      font-size: 0.76rem;
      padding: 3px 10px;
      border: 1px solid var(--accent);
      border-radius: 999px;
      background: var(--accent-soft);
      color: var(--accent);
      cursor: pointer;
    }

    .live__refresh:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }

    .live__last {
      font-family: var(--mono);
      font-size: 0.72rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
  `,
})
export class LiveFeedBar {
  protected readonly feed = inject(LiveFeed);
  private readonly api = inject(DisputesApi);

  protected refresh(): void {
    this.api.refresh();
    this.feed.acknowledge();
  }

  protected describe(event: LiveEvent): string {
    const { kind, amount_minor, currency } = event.payload;
    return `${kind} ${formatMinor(amount_minor, currency)}`;
  }
}
