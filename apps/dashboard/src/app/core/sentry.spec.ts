import { createServer, type Server } from 'node:http';
import { ErrorHandler } from '@angular/core';
import * as Sentry from '@sentry/angular';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { SENTRY_DSN, initSentry, sentryErrorHandler, sentryErrorHandlerProvider } from './sentry';

/**
 * A stand-in for Sentry's ingest host.
 *
 * A DSN's host is an ordinary HTTP endpoint, so pointing one at 127.0.0.1 is
 * enough to see whether anything is actually sent - no account, no network.
 */
interface Received {
  readonly url: string;
  readonly body: string;
}

let server: Server;
let received: Received[] = [];
let origin = '';

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      received.push({ url: req.url ?? '', body: Buffer.concat(chunks).toString('utf8') });
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{}');
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('no server port');
  origin = `127.0.0.1:${address.port}`;
});

afterAll(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((err) => (err ? reject(err) : resolve())),
  );
});

afterEach(async () => {
  // Sentry's client is module-global, so each test has to hand it back or the
  // order of the tests in this file would decide their results.
  await Sentry.getClient()?.close();
  Sentry.getCurrentScope().setClient(undefined);
  received = [];
});

/** The DSN the tests use. The public key is arbitrary; only the host matters. */
const localDsn = (): string => `http://0123456789abcdef0123456789abcdef@${origin}/1`;

describe('initSentry with no DSN', () => {
  /**
   * The property the whole feature rests on: on a laptop with nothing
   * configured, this file may as well not exist.
   */
  it('does not initialise and sends nothing', async () => {
    expect(initSentry('')).toBe(false);
    expect(Sentry.getClient()).toBeUndefined();

    Sentry.captureException(new Error('nobody is listening'));
    await Sentry.flush(500);

    expect(received).toEqual([]);
  });

  it('treats whitespace as absent', () => {
    expect(initSentry('   ')).toBe(false);
    expect(Sentry.getClient()).toBeUndefined();
  });

  it('ships with the DSN empty, so off is the default', () => {
    expect(SENTRY_DSN).toBe('');
  });

  /** Angular's own handler, so the console reads as it did before Sentry. */
  it('leaves Angular its own ErrorHandler', () => {
    const handler = sentryErrorHandler();

    expect(handler).toBeInstanceOf(ErrorHandler);
    expect(handler).not.toBeInstanceOf(Sentry.SentryErrorHandler);
  });

  /** The factory these tests exercise is the one appConfig actually installs. */
  it('is wired into the provider appConfig uses', () => {
    expect(sentryErrorHandlerProvider).toMatchObject({
      provide: ErrorHandler,
      useFactory: sentryErrorHandler,
    });
  });
});

describe('initSentry with a DSN', () => {
  it('sends an envelope to the DSN host', async () => {
    expect(initSentry(localDsn())).toBe(true);
    expect(Sentry.getClient()).toBeDefined();

    Sentry.captureException(new Error('deliberate: proving the wire works'));
    await Sentry.flush(2000);

    const envelopes = received.filter((r) => r.url.includes('/api/1/envelope/'));
    expect(envelopes.length).toBeGreaterThan(0);
    expect(envelopes.map((e) => e.body).join('')).toContain('deliberate: proving the wire works');
  });

  it('installs Sentry as the ErrorHandler', () => {
    initSentry(localDsn());

    expect(sentryErrorHandler()).toBeInstanceOf(Sentry.SentryErrorHandler);
  });

  /**
   * FiltersStore puts the operator's search box and merchant picks in the page
   * URL, so a breadcrumb trail is a transcript of who was being looked at.
   */
  it('strips query strings and drops console breadcrumbs', async () => {
    initSentry(localDsn());

    Sentry.addBreadcrumb({
      category: 'navigation',
      data: { from: '/disputes', to: '/disputes?q=alice&merchant=acme' },
    });
    Sentry.addBreadcrumb({ category: 'console', message: 'merchant acme owes 4999' });
    Sentry.addBreadcrumb({ category: 'ui.input', message: 'input#search' });
    Sentry.captureException(new Error('after the trail'));
    await Sentry.flush(2000);

    const body = received.map((r) => r.body).join('');
    expect(body).toContain('/disputes');
    expect(body).not.toContain('q=alice');
    expect(body).not.toContain('merchant=acme');
    expect(body).not.toContain('owes 4999');
    expect(body).not.toContain('input#search');
  });
});
