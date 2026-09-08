import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { PoolClient } from "pg";
import { copyInto } from "./copy.js";
import type { CopyValue } from "./copy.js";
import { withClient } from "./db.js";
import { env } from "./env.js";
import { MERCHANT_PROFILES } from "./catalog.js";
import type { CardNetwork } from "./catalog.js";
import { generateDispute, generateTransaction, DAY_MS } from "./generate.js";
import type { DisputeSource, GeneratedDispute, SeededMerchant } from "./generate.js";
import { toMinor } from "./money.js";
import type { Currency } from "./money.js";
import { chance, makeRng, weighted } from "./rand.js";

const here = dirname(fileURLToPath(import.meta.url));

const SEED_SQL_DIR = resolve(here, "../../../db/seed");

// Relative transaction volume per merchant, so the dashboard has a couple of
// whales and a long tail rather than eight identical rows.
const VOLUME_WEIGHTS = [30, 22, 14, 10, 8, 7, 5, 4] as const;

const TRUNCATE_TABLES = [
  // agent_runs cascades from disputes anyway, but naming it here is the point:
  // a reader of this list should be able to see everything a reseed destroys
  // without tracing foreign keys to find out.
  "agent_runs",
  "ledger_entries",
  "ledger_transactions",
  "ledger_accounts",
  "dispute_events",
  "disputes",
  "outbox",
  "webhook_events",
  "transactions",
  "merchants",
] as const;

function elapsed(started: number): string {
  return `${((Date.now() - started) / 1000).toFixed(1)}s`;
}

/**
 * COPY does not update planner statistics. Until a table is analysed, the
 * planner works from defaults and will happily pick a nested loop over the
 * 500k-row table it thinks is tiny - which is how the refund backfill went
 * from under a second to eight minutes. Analyse each table as soon as it is
 * loaded, not once at the end.
 */
async function analyze(client: PoolClient, table: string): Promise<void> {
  const started = Date.now();
  await client.query(`ANALYZE ${table}`);
  console.log(`  analyze ${table.padEnd(26)} ${elapsed(started).padStart(15)}`);
}

async function runSqlFile(client: PoolClient, file: string): Promise<void> {
  const started = Date.now();
  const sql = await readFile(resolve(SEED_SQL_DIR, file), "utf8");
  // A multi-statement query returns one result per statement; report the total
  // so a file that inserts accounts *and* entries doesn't under-report itself.
  const result = await client.query(sql);
  const affected = Array.isArray(result)
    ? result.reduce((sum, one) => sum + (one.rowCount ?? 0), 0)
    : (result.rowCount ?? 0);
  console.log(`  ${file.padEnd(34)} ${String(affected ?? 0).padStart(9)} rows  ${elapsed(started)}`);
}

async function insertMerchants(client: PoolClient, rng: () => number): Promise<SeededMerchant[]> {
  const totalWeight = VOLUME_WEIGHTS.reduce((sum, weight) => sum + weight, 0);
  const merchants: SeededMerchant[] = [];

  const profiles = MERCHANT_PROFILES.slice(0, Math.max(1, Math.min(env.merchants, MERCHANT_PROFILES.length)));

  for (const [index, profile] of profiles.entries()) {
    const weight = VOLUME_WEIGHTS[index] ?? 3;
    const expectedTransactions = Math.round((env.transactions * weight) / totalWeight);

    const { rows } = await client.query<{ id: number }>(
      `INSERT INTO merchants (external_id, name, webhook_secret, currency, auto_refund_ceiling_minor)
       VALUES ($1, $2, $3, $4, $5)
       RETURNING id`,
      [
        profile.externalId,
        profile.name,
        // Deterministic from SEED, so the Go ingest service can be configured
        // with the same secret without a handoff step.
        `whsec_${Math.floor(rng() * 0xffffffff).toString(16).padStart(8, "0")}${Math.floor(rng() * 0xffffffff).toString(16).padStart(8, "0")}`,
        profile.currency,
        profile.autoRefundCeiling === null ? null : toMinor(profile.autoRefundCeiling, profile.currency),
      ],
    );

    const id = rows[0]?.id;
    if (id === undefined) throw new Error(`failed to insert merchant ${profile.externalId}`);

    merchants.push({
      ...profile,
      id,
      customerCount: Math.max(50, Math.floor(expectedTransactions / 6)),
    });
  }

  return merchants;
}

async function copyTransactions(client: PoolClient, merchants: SeededMerchant[], rng: () => number): Promise<void> {
  const now = Date.now();
  const windowStart = now - 365 * DAY_MS;
  const windowEnd = now - 60_000;

  const merchantMix = merchants.map(
    (merchant, index) => [merchant, VOLUME_WEIGHTS[index] ?? 3] as const,
  );

  function* rows(): Generator<readonly CopyValue[]> {
    for (let index = 0; index < env.transactions; index += 1) {
      const merchant = weighted(rng, merchantMix);
      const txn = generateTransaction(rng, merchant, index, windowStart, windowEnd);
      yield [
        txn.merchantId,
        txn.externalId,
        txn.amountMinor,
        txn.currency,
        0,             // refunded_minor - backfilled from disputes later
        "captured",
        txn.cardNetwork,
        txn.cardBin,
        txn.cardLast4,
        txn.customerRef,
        txn.customerEmail,
        txn.descriptor,
        txn.capturedAt,
      ];
    }
  }

  await copyInto(
    client,
    "transactions",
    [
      "merchant_id", "external_id", "amount_minor", "currency", "refunded_minor", "status",
      "card_network", "card_bin", "card_last4", "customer_ref", "customer_email",
      "descriptor", "captured_at",
    ],
    rows(),
  );
}

interface TransactionRow {
  id: number;
  merchant_id: number;
  external_id: string;
  amount_minor: number;
  currency: Currency;
  card_network: CardNetwork;
  captured_at: Date;
}

/**
 * Walks the transaction table by keyset (`WHERE id > $last ORDER BY id LIMIT n`)
 * rather than OFFSET, which degrades to a full scan the deeper it pages. Only
 * the ~2% that become disputes are held in memory.
 */
async function copyDisputes(client: PoolClient, merchants: SeededMerchant[], rng: () => number): Promise<number> {
  const now = Date.now();
  const eligibleBefore = new Date(now - 2 * DAY_MS);
  const multipliers = new Map(merchants.map((merchant) => [merchant.id, merchant.disputeMultiplier]));

  const disputes: GeneratedDispute[] = [];
  const pageSize = 25_000;
  let lastId = 0;

  for (;;) {
    const { rows } = await client.query<TransactionRow>(
      `SELECT id, merchant_id, external_id, amount_minor, currency, card_network, captured_at
         FROM transactions
        WHERE id > $1 AND captured_at < $2
        ORDER BY id
        LIMIT $3`,
      [lastId, eligibleBefore, pageSize],
    );
    if (rows.length === 0) break;

    for (const row of rows) {
      const rate = env.disputeRate * (multipliers.get(row.merchant_id) ?? 1);
      if (!chance(rng, rate)) continue;

      const source: DisputeSource = {
        id: row.id,
        merchantId: row.merchant_id,
        externalId: row.external_id,
        amountMinor: row.amount_minor,
        currency: row.currency,
        cardNetwork: row.card_network,
        capturedAt: row.captured_at,
      };
      disputes.push(generateDispute(rng, source, now));
    }

    lastId = rows.at(-1)?.id ?? lastId;
  }

  await copyInto(
    client,
    "disputes",
    [
      "merchant_id", "transaction_id", "external_id", "kind", "card_network", "reason_code",
      "amount_minor", "currency", "state", "deadline_at", "opened_at", "resolved_at",
    ],
    disputes.map((dispute) => [
      dispute.merchantId,
      dispute.transactionId,
      dispute.externalId,
      dispute.kind,
      dispute.cardNetwork,
      dispute.reasonCode,
      dispute.amountMinor,
      dispute.currency,
      dispute.state,
      dispute.deadlineAt,
      dispute.openedAt,
      dispute.resolvedAt,
    ]),
  );

  return disputes.length;
}

export async function seed(): Promise<void> {
  const rng = makeRng(env.seed);
  const startedAll = Date.now();

  console.log(
    `seeding: ${env.transactions.toLocaleString("en-US")} transactions, ` +
      `${(env.disputeRate * 100).toFixed(1)}% base dispute rate, SEED=${env.seed}`,
  );

  await withClient(async (client) => {
    let started = Date.now();
    await client.query(`TRUNCATE ${TRUNCATE_TABLES.join(", ")} RESTART IDENTITY CASCADE`);
    console.log(`  truncate                           ${elapsed(started)}`);

    started = Date.now();
    const merchants = await insertMerchants(client, rng);
    console.log(`  merchants                          ${String(merchants.length).padStart(9)} rows  ${elapsed(started)}`);

    await runSqlFile(client, "010_ledger_accounts.sql");

    started = Date.now();
    await copyTransactions(client, merchants, rng);
    console.log(`  transactions (COPY)                ${String(env.transactions).padStart(9)} rows  ${elapsed(started)}`);
    await analyze(client, "transactions");

    started = Date.now();
    const disputeCount = await copyDisputes(client, merchants, rng);
    console.log(`  disputes (COPY)                    ${String(disputeCount).padStart(9)} rows  ${elapsed(started)}`);
    await analyze(client, "disputes");

    // The balance trigger is a per-row constraint trigger. Firing it a million
    // times during a bulk load costs minutes; disabling it for the load and
    // proving the invariant with one set-based query afterwards costs seconds.
    // `verify` re-checks the same invariant, so nothing is taken on trust.
    await client.query("ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_must_balance");
    try {
      await runSqlFile(client, "020_ledger_captures.sql");
      await runSqlFile(client, "030_ledger_refunds.sql");
      await runSqlFile(client, "040_ledger_chargebacks.sql");
    } finally {
      await client.query("ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_must_balance");
    }

    await analyze(client, "ledger_transactions");
    await analyze(client, "ledger_entries");

    await runSqlFile(client, "050_backfill_refund_totals.sql");
    await runSqlFile(client, "060_dispute_events.sql");
    await analyze(client, "dispute_events");

    // The one untrusted field on a dispute. Last, because it reads dispute ids
    // and every one of them has to exist first.
    await runSqlFile(client, "070_cardholder_claims.sql");
  });

  console.log(`done in ${elapsed(startedAll)}`);
}
