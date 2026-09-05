import { withClient, pool } from "./db.js";
import { env } from "./env.js";
import { makeRng, int, pick, weighted, chance } from "./rand.js";
import { DEADLINE_HOURS, REASON_CODES } from "./catalog.js";
import type { CardNetwork, DisputeKind } from "./catalog.js";
import { formatMinor } from "./money.js";
import type { Currency } from "./money.js";
import { newIdempotencyKey, sign } from "./signing.js";
import { HOUR_MS } from "./generate.js";

interface Candidate {
  transaction_external_id: string;
  merchant_external_id: string;
  webhook_secret: string;
  amount_minor: number;
  currency: Currency;
  card_network: CardNetwork;
  card_last4: string;
  customer_ref: string;
  captured_at: Date;
}

/** The webhook body. This is the contract the Go ingest service parses. */
interface DisputeWebhook {
  readonly id: string;
  readonly type: "dispute.opened";
  readonly created_at: string;
  readonly data: {
    readonly dispute_id: string;
    readonly merchant_id: string;
    readonly transaction_id: string;
    readonly kind: DisputeKind;
    readonly card_network: CardNetwork;
    readonly reason_code: string;
    readonly amount_minor: number;
    readonly currency: Currency;
    readonly opened_at: string;
    readonly respond_by: string;
  };
}

function buildEvent(rng: () => number, candidate: Candidate): DisputeWebhook {
  const kind: DisputeKind = chance(rng, 0.7) ? "alert" : "chargeback";
  const codes = REASON_CODES[candidate.card_network];
  const reason = weighted(rng, codes.map((code) => [code, code.weight] as const));
  const [minHours, maxHours] = DEADLINE_HOURS[kind];

  const openedAt = new Date();
  const respondBy = new Date(openedAt.getTime() + int(rng, minHours, maxHours) * HOUR_MS);

  return {
    id: newIdempotencyKey(),
    type: "dispute.opened",
    created_at: openedAt.toISOString(),
    data: {
      dispute_id: `dsp_live_${Math.floor(rng() * 0xffffffff).toString(36)}`,
      merchant_id: candidate.merchant_external_id,
      transaction_id: candidate.transaction_external_id,
      kind,
      card_network: candidate.card_network,
      reason_code: reason.code,
      amount_minor: candidate.amount_minor,
      currency: candidate.currency,
      opened_at: openedAt.toISOString(),
      respond_by: respondBy.toISOString(),
    },
  };
}

async function post(event: DisputeWebhook, secret: string): Promise<string> {
  const body = JSON.stringify(event);
  const response = await fetch(env.ingestUrl, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-processor-signature": sign(secret, body),
      // Belt and braces: the signature already covers the body, but a separate
      // idempotency header is what the ingest service keys its Redis SETNX on.
      "idempotency-key": event.id,
    },
    body,
  });
  return `${response.status} ${response.statusText}`;
}

/**
 * Emits signed dispute webhooks at `--rate` per minute until stopped.
 *
 * `--replay` resends every event a second time. Until Phase 1's Redis dedupe
 * exists, that is how you see the bug: two disputes, one event.
 */
export async function emit(options: { rate: number; count: number; replay: boolean }): Promise<void> {
  const rng = makeRng(Date.now() & 0xffffffff);
  const intervalMs = Math.max(50, Math.round(60_000 / options.rate));

  const { rows: candidates } = await withClient((client) =>
    client.query<Candidate>(`
      SELECT t.external_id AS transaction_external_id,
             m.external_id AS merchant_external_id,
             m.webhook_secret,
             t.amount_minor, t.currency, t.card_network, t.card_last4,
             t.customer_ref, t.captured_at
        FROM transactions t
        JOIN merchants m ON m.id = t.merchant_id
       WHERE t.captured_at > now() - INTERVAL '120 days'
         AND NOT EXISTS (SELECT 1 FROM disputes d WHERE d.transaction_id = t.id)
       ORDER BY random()
       LIMIT 2000`),
  );

  if (candidates.length === 0) {
    console.error("no undisputed transactions to draw from - run `yarn seed` first");
    await pool.end();
    process.exitCode = 1;
    return;
  }

  console.log(`emitting to ${env.ingestUrl} at ~${options.rate}/min (${candidates.length} candidates)`);
  let sent = 0;
  let reachable = true;

  while (options.count === 0 || sent < options.count) {
    const candidate = pick(rng, candidates);
    const event = buildEvent(rng, candidate);
    const attempts = options.replay ? 2 : 1;

    for (let attempt = 1; attempt <= attempts; attempt += 1) {
      try {
        const result = await post(event, candidate.webhook_secret);
        console.log(
          `  ${event.id}  ${event.data.kind.padEnd(10)} ${event.data.reason_code.padEnd(6)} ` +
            `${formatMinor(event.data.amount_minor, event.data.currency).padStart(16)}  ` +
            `-> ${result}${attempt > 1 ? "  (replay)" : ""}`,
        );
        reachable = true;
      } catch (error) {
        if (reachable) {
          console.error(
            `  ingest unreachable at ${env.ingestUrl} (${(error as Error).message}). ` +
              `That service is Phase 1 - printing events instead.`,
          );
          reachable = false;
        }
        console.log(`  ${event.id}  ${event.data.kind}  ${event.data.reason_code}  (not delivered)`);
      }
    }

    sent += 1;
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }

  await pool.end();
}
