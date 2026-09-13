import { ErrorHandler, type Provider } from '@angular/core';
import * as Sentry from '@sentry/angular';

/**
 * The Sentry DSN. Empty means Sentry is off.
 *
 * Off is the default and the local truth: with this empty the SDK is never
 * initialised, no client exists, Angular keeps its own ErrorHandler, and
 * nothing is sent anywhere. Paste a project DSN here and rebuild to turn it on.
 *
 * A constant rather than an `environments/` scaffold with `fileReplacements`,
 * because the one thing that scaffold buys - a production value the
 * development build does not see - buys nothing for a DSN. A DSN is public by
 * design; it ships in the browser bundle whichever file it started in. The app
 * already configures its one other external address the same cheap way, in
 * `api.config.ts`.
 */
export const SENTRY_DSN: string = '';

/**
 * Starts Sentry, unless there is no DSN.
 *
 * Returns whether it started, so the caller and the tests can tell the two
 * states apart without reaching into the SDK.
 *
 * No TraceService, and no browserTracingIntegration it would need. On its
 * defaults that integration puts `sentry-trace` and `baggage` headers on every
 * cross-origin request matching `localhost` - which is every call this app
 * makes to the read API on :8081 - and that API answers a preflight with
 * `Access-Control-Allow-Headers: Content-Type`. Turning Sentry on would stop
 * the dashboard loading data. Three flags fix it (`traceFetch: false`,
 * `traceXHR: false`, `tracePropagationTargets: []`), and what is left is how
 * many milliseconds a lazy chunk took to arrive from localhost, on three
 * routes, against a 10,000-unit monthly budget. Not worth the trap. When the
 * Go side starts tracing and the propagation is the point, add the integration
 * and widen that allow-list in the same change.
 */
export function initSentry(dsn: string = SENTRY_DSN): boolean {
  if (dsn.trim() === '') return false;

  Sentry.init({
    dsn,

    // This dashboard shows merchant names, dispute amounts and free-text
    // search. None of the following is on by default in v10, but each is one
    // option away from being on, and the cost of being wrong is that data
    // leaving the laptop:
    //
    // - no session replay: replayIntegration is never added, so no recording
    //   of the screen, and no replay envelopes.
    // - sendDefaultPii stays false: no IP address, no cookies, no request
    //   headers or bodies attached to events.
    // - no browserTracingIntegration, so no fetch or XHR instrumentation and
    //   no request URLs captured as spans.
    sendDefaultPii: false,

    // A long trail is a long transcript of what an operator was looking at.
    // Twenty is enough to see how an error was reached.
    maxBreadcrumbs: 20,

    beforeBreadcrumb: scrubBreadcrumb,
    beforeSend: scrubEvent,
  });

  return true;
}

/**
 * The ErrorHandler Angular should use.
 *
 * A factory rather than a conditional in `appConfig`'s array, because that
 * array is built when its module is imported and ESM finishes every import
 * before main.ts runs `initSentry` - a check made while building the array
 * would always see no client and always lose.
 *
 * With Sentry off this hands back Angular's own handler, so an uncaught error
 * prints what it prints today. Sentry's handler formats the console line
 * differently, which is reason enough not to install it when it has nowhere to
 * send anything.
 */
export function sentryErrorHandler(): ErrorHandler {
  return Sentry.getClient() ? Sentry.createErrorHandler() : new ErrorHandler();
}

/** The provider `appConfig` installs; `sentryErrorHandler` is what it calls. */
export const sentryErrorHandlerProvider: Provider = {
  provide: ErrorHandler,
  useFactory: sentryErrorHandler,
};

/**
 * A URL with its query and fragment removed.
 *
 * FiltersStore mirrors every filter into the browser's query string, so the
 * dashboard's own URL carries the operator's search box (`q`) and the merchant
 * names they picked. httpContextIntegration stamps that URL onto every event
 * and the history breadcrumb records it again on each debounced keystroke. The
 * path is what says which screen broke; the query is the part that identifies
 * who was being looked at.
 */
function withoutQuery(url: string): string {
  const cut = url.search(/[?#]/);
  return cut === -1 ? url : url.slice(0, cut);
}

function scrubBreadcrumb(breadcrumb: Sentry.Breadcrumb): Sentry.Breadcrumb | null {
  // A console breadcrumb replays whatever the app logged, and ui.input records
  // the field a person typed into. Both are cheaper to drop than to audit.
  if (breadcrumb.category === 'console' || breadcrumb.category === 'ui.input') return null;

  const data = breadcrumb.data;
  if (data) {
    // fetch and xhr breadcrumbs carry `url`; history carries `from` and `to`.
    for (const key of ['url', 'from', 'to']) {
      const value = data[key];
      if (typeof value === 'string') data[key] = withoutQuery(value);
    }
  }
  return breadcrumb;
}

function scrubEvent(event: Sentry.ErrorEvent): Sentry.ErrorEvent {
  const request = event.request;
  if (request?.url) request.url = withoutQuery(request.url);
  return event;
}
