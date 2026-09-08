import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';

/**
 * The shell: three screens and the way between them.
 *
 * Deliberately not a home page. A landing screen whose only content is three
 * links is a click every visit buys nothing, and at this size a persistent nav
 * is what people actually want - they arrive knowing which screen they need.
 *
 * It also lives here rather than in each page's own header, which is how the
 * link to Decisions came to exist on one screen and not the other: a cross-link
 * written into a masthead is a link somebody has to remember to add again.
 */
@Component({
  selector: 'app-root',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  template: `
    <nav class="nav" aria-label="Sections">
      <span class="nav__brand">Dispute Router</span>
      <a routerLink="/disputes" routerLinkActive="nav__link--on">Disputes</a>
      <a routerLink="/reviews" routerLinkActive="nav__link--on">Review queue</a>
      <a routerLink="/decisions" routerLinkActive="nav__link--on">Decisions</a>
    </nav>
    <router-outlet />
  `,
  styles: `
    .nav {
      display: flex;
      align-items: center;
      gap: 1.1rem;
      padding: 0.55rem 1.5rem;
      border-bottom: 1px solid var(--rule);
      background: var(--surface);
      font-size: 0.84rem;
    }
    .nav__brand {
      font-weight: 600;
      letter-spacing: -0.01em;
      color: var(--ink);
      margin-right: 0.4rem;
    }
    .nav a {
      color: var(--ink-muted);
      text-decoration: none;
      padding: 0.15rem 0;
      border-bottom: 2px solid transparent;
    }
    .nav a:hover { color: var(--ink); }
    /* The current section, marked by the router rather than by each page
       knowing its own name. */
    .nav__link--on { color: var(--ink); border-bottom-color: var(--accent); }
  `,
})
export class App {}
