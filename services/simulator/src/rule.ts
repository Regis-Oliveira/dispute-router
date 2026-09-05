import { pool, withClient } from "./db.js";
import { env } from "./env.js";
import { chance, makeRng, pick } from "./rand.js";
import { formatMinor } from "./money.js";
import type { Currency } from "./money.js";
import { newIdempotencyKey, sign } from "./signing.js";

/**
 * The network coming back with a verdict on a representment.
 *
 * Weeks pass between a merchant submitting evidence and Visa or Mastercard
 * deciding. This collapses that into a command, so the last state transition in
 * the lifecycle can actually be exercised.
 */
interface RulingWebhook {
  readonly id: string;
  readonly type: "dispute.resolved";
  readonly created_at: string;
  readonly data: {
    readonly dispute_id: string;
    readonly merchant_id: string;
    readonly outcome: "won" | "lost";
    readonly decided_at: string;
    readonly note: string;
  };
}

interface Candidate {
  dispute_external_id: string;
  merchant_external_id: string;
  webhook_secret: string;
  reason_code: string;
  amount_minor: number;
  currency: Currency;
}

/**
 * Win rates by reason category, roughly as the industry reports them.
 *
 * Evidence-led disputes are winnable because the merchant holds the evidence:
 * a delivery confirmation answers "it never arrived". Fraud claims are not,
 * because the merchant cannot prove who was holding the card.
 */
const WIN_RATE: Record<string, number> = {
  service: 0.42,
  processing: 0.55,
  fraud: 0.12,
  unknown: 0.2,
};

const CATEGORY: Record<string, keyof typeof WIN_RATE> = {
  "10.4": "fraud", "4837": "fraud", F29: "fraud", UA02: "fraud", UA01: "fraud",
  "13.1": "service", "13.3": "service", "13.6": "service", "13.7": "service",
  "4853": "service", "4855": "service", "4841": "service",
  C08: "service", C31: "service", C02: "service", RG: "service", RM: "service", AP: "service",
  "12.5": "processing", "12.6.1": "processing", "4834": "processing", P08: "processing", DP: "processing",
};

const WON_NOTES = [
  "compelling evidence accepted",
  "delivery confirmation matched the billing address",
  "prior undisputed transactions established a pattern",
];

const LOST_NOTES = [
  "evidence did not address the cardholder's claim",
  "no proof of authorisation",
  "cardholder maintained the dispute",
];

async function post(event: RulingWebhook, secret: string): Promise<string> {
  const body = JSON.stringify(event);
  const response = await fetch(env.ingestUrl, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-processor-signature": sign(secret, body),
    },
    body,
  });
  return `${response.status} ${response.statusText}`;
}

/**
 * Rules on disputes that are awaiting one.
 *
 * Only 'represented' disputes are eligible, which is the same condition the
 * ingest service enforces - a ruling for anything else is a 422 and is kept as
 * evidence rather than acted on.
 */
export async function rule(options: { count: number; rate: number }): Promise<void> {
  const rng = makeRng(Date.now() & 0xffffffff);
  const intervalMs = Math.max(50, Math.round(60_000 / options.rate));

  const { rows: candidates } = await withClient((client) =>
    client.query<Candidate>(`
      SELECT d.external_id AS dispute_external_id,
             m.external_id AS merchant_external_id,
             m.webhook_secret,
             d.reason_code, d.amount_minor, d.currency
        FROM disputes d
        JOIN merchants m ON m.id = d.merchant_id
       WHERE d.state = 'represented'
       ORDER BY random()
       LIMIT $1`, [Math.max(options.count, 1)]),
  );

  if (candidates.length === 0) {
    console.error(
      "no disputes are awaiting a ruling.\n" +
        "  Run the worker first: it moves evidence-led chargebacks to 'represented'.",
    );
    await pool.end();
    process.exitCode = 1;
    return;
  }

  console.log(`ruling on ${candidates.length} represented disputes`);
  const tally = { won: 0, lost: 0 };

  for (const candidate of candidates) {
    const category = CATEGORY[candidate.reason_code] ?? "unknown";
    const outcome: "won" | "lost" = chance(rng, WIN_RATE[category] ?? 0.2) ? "won" : "lost";
    tally[outcome] += 1;

    const event: RulingWebhook = {
      id: newIdempotencyKey(),
      type: "dispute.resolved",
      created_at: new Date().toISOString(),
      data: {
        dispute_id: candidate.dispute_external_id,
        merchant_id: candidate.merchant_external_id,
        outcome,
        decided_at: new Date().toISOString(),
        note: pick(rng, outcome === "won" ? WON_NOTES : LOST_NOTES),
      },
    };

    try {
      const result = await post(event, candidate.webhook_secret);
      console.log(
        `  ${candidate.dispute_external_id.padEnd(26)} ${category.padEnd(11)} ` +
          `${outcome.padEnd(5)} ${formatMinor(candidate.amount_minor, candidate.currency).padStart(15)}  -> ${result}`,
      );
    } catch (error) {
      console.error(`  ${candidate.dispute_external_id}  (not delivered: ${(error as Error).message})`);
    }

    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }

  console.log(`\n${tally.won} won, ${tally.lost} lost`);
  console.log("a lost representment costs the sale plus a 15.00 network fee, posted to the ledger");
  await pool.end();
}
