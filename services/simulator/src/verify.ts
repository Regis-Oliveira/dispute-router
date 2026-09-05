import { pool, withClient } from "./db.js";
import { formatMinor } from "./money.js";
import type { Currency } from "./money.js";

interface Check {
  readonly name: string;
  readonly sql: string;
  /** The check passes when this query returns zero rows. */
  readonly describe: (row: Record<string, unknown>) => string;
}

// Each of these is an invariant the schema already enforces. Running them anyway
// is the point: a seeder that disables a trigger owes proof that it did not
// smuggle bad data past it.
const CHECKS: readonly Check[] = [
  {
    name: "every journal entry balances",
    sql: `
      SELECT lt.id, lt.external_ref,
             SUM(CASE WHEN e.direction = 'debit' THEN e.amount_minor ELSE -e.amount_minor END) AS delta
        FROM ledger_transactions lt
        JOIN ledger_entries e ON e.ledger_transaction_id = lt.id
       GROUP BY lt.id, lt.external_ref
      HAVING SUM(CASE WHEN e.direction = 'debit' THEN e.amount_minor ELSE -e.amount_minor END) <> 0
       LIMIT 5`,
    describe: (row) => `${row["external_ref"]} off by ${row["delta"]}`,
  },
  {
    name: "no journal entry has fewer than two postings",
    sql: `
      SELECT lt.id, lt.external_ref, count(e.id) AS postings
        FROM ledger_transactions lt
        LEFT JOIN ledger_entries e ON e.ledger_transaction_id = lt.id
       GROUP BY lt.id, lt.external_ref
      HAVING count(e.id) < 2
       LIMIT 5`,
    describe: (row) => `${row["external_ref"]} has ${row["postings"]} posting(s)`,
  },
  {
    name: "debits equal credits across the whole book, per currency",
    sql: `
      SELECT currency,
             SUM(CASE WHEN direction = 'debit' THEN amount_minor ELSE -amount_minor END) AS delta
        FROM ledger_entries
       GROUP BY currency
      HAVING SUM(CASE WHEN direction = 'debit' THEN amount_minor ELSE -amount_minor END) <> 0`,
    describe: (row) => `${row["currency"]} off by ${row["delta"]}`,
  },
  {
    name: "no dispute exceeds the transaction it points at",
    sql: `
      SELECT d.id, d.amount_minor, t.amount_minor AS txn_amount
        FROM disputes d
        JOIN transactions t ON t.id = d.transaction_id
       WHERE d.amount_minor > t.amount_minor
       LIMIT 5`,
    describe: (row) => `dispute ${row["id"]}: ${row["amount_minor"]} > ${row["txn_amount"]}`,
  },
  {
    name: "no dispute is in a currency its transaction is not",
    sql: `
      SELECT d.id FROM disputes d
        JOIN transactions t ON t.id = d.transaction_id
       WHERE d.currency <> t.currency
       LIMIT 5`,
    describe: (row) => `dispute ${row["id"]}`,
  },
  {
    name: "no dispute opened before the charge it disputes",
    sql: `
      SELECT d.id FROM disputes d
        JOIN transactions t ON t.id = d.transaction_id
       WHERE d.opened_at < t.captured_at
       LIMIT 5`,
    describe: (row) => `dispute ${row["id"]}`,
  },
  {
    name: "no dispute opened in the future",
    sql: `SELECT id, opened_at FROM disputes WHERE opened_at > now() LIMIT 5`,
    describe: (row) => `dispute ${row["id"]} opens ${String(row["opened_at"])}`,
  },
  {
    // The strongest check here: it ties the ledger to the domain tables. Every
    // chargeback holds funds on arrival and releases them when it resolves, so
    // disputes_payable must equal the disputed amount of exactly those
    // chargebacks still awaiting an outcome. A hold that is never released, or
    // a resolution that forgets to release one, shows up here and nowhere else
    // - both of those balance perfectly and are still wrong.
    name: "money held equals the chargebacks still open",
    sql: `
      WITH held AS (
        SELECT b.merchant_id, b.currency, b.balance_minor
          FROM ledger_account_balances b
         WHERE b.kind = 'disputes_payable'
      ),
      expected AS (
        SELECT d.merchant_id, d.currency, COALESCE(SUM(d.amount_minor), 0) AS amount
          FROM disputes d
         WHERE d.kind = 'chargeback'
           AND d.state IN ('received', 'resolving', 'represented')
         GROUP BY d.merchant_id, d.currency
      )
      SELECT COALESCE(h.merchant_id, e.merchant_id) AS merchant_id,
             COALESCE(h.currency, e.currency)       AS currency,
             COALESCE(h.balance_minor, 0)           AS held,
             COALESCE(e.amount, 0)                  AS expected
        FROM held h
        FULL OUTER JOIN expected e
          ON e.merchant_id = h.merchant_id AND e.currency = h.currency
       WHERE COALESCE(h.balance_minor, 0) <> COALESCE(e.amount, 0)
       LIMIT 5`,
    describe: (row) =>
      `merchant ${row["merchant_id"]} ${row["currency"]}: holding ${row["held"]}, should hold ${row["expected"]}`,
  },
  {
    name: "no chargeback resolved without releasing its hold",
    sql: `
      SELECT d.id, d.state
        FROM disputes d
       WHERE d.kind = 'chargeback'
         AND d.state IN ('won', 'lost', 'expired')
         AND EXISTS (SELECT 1 FROM ledger_transactions lt
                      WHERE lt.external_ref = 'dispute:' || d.id || ':hold')
         AND NOT EXISTS (SELECT 1 FROM ledger_transactions lt
                          WHERE lt.external_ref IN ('dispute:' || d.id || ':release',
                                                    'dispute:' || d.id || ':chargeback'))
       LIMIT 5`,
    describe: (row) => `dispute ${row["id"]} is ${row["state"]} with its hold still standing`,
  },
  {
    name: "refund totals never exceed the capture",
    sql: `SELECT id, refunded_minor, amount_minor FROM transactions WHERE refunded_minor > amount_minor LIMIT 5`,
    describe: (row) => `transaction ${row["id"]}`,
  },
];

async function runChecks(): Promise<boolean> {
  console.log("invariants");
  let allPassed = true;

  await withClient(async (client) => {
    for (const check of CHECKS) {
      const { rows } = await client.query<Record<string, unknown>>(check.sql);
      if (rows.length === 0) {
        console.log(`  PASS  ${check.name}`);
      } else {
        allPassed = false;
        console.log(`  FAIL  ${check.name}`);
        for (const row of rows) console.log(`          ${check.describe(row)}`);
      }
    }
  });

  return allPassed;
}

/**
 * The invariants above prove the data is clean. This proves the database would
 * have stopped it if it weren't - a seeder that disabled the trigger and forgot
 * to switch it back on would pass every check above and fail this one.
 */
async function checkTriggerIsArmed(): Promise<boolean> {
  return withClient(async (client) => {
    await client.query("BEGIN");
    try {
      const { rows } = await client.query<{ id: number; currency: string }>(
        "SELECT id, currency FROM ledger_accounts LIMIT 1",
      );
      const account = rows[0];
      if (!account) throw new Error("no ledger accounts to test against");

      const { rows: txnRows } = await client.query<{ id: number }>(
        `INSERT INTO ledger_transactions (external_ref, kind, currency, occurred_at)
         VALUES ('probe:' || gen_random_uuid(), 'fee', $1, now()) RETURNING id`,
        [account.currency],
      );
      const txnId = txnRows[0]?.id;

      // One posting, no counterpart. This must not be committable.
      await client.query(
        `INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
         VALUES ($1, $2, 'debit', 100, $3)`,
        [txnId, account.id, account.currency],
      );

      await client.query("COMMIT");
      console.log("  FAIL  unbalanced entry was accepted - the balance trigger is NOT armed");
      return false;
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      const message = (error as Error).message;
      if (message.includes("double-entry needs at least 2") || message.includes("unbalanced")) {
        console.log("  PASS  balance trigger rejects an unbalanced entry at COMMIT");
        return true;
      }
      console.log(`  FAIL  unexpected error while probing the trigger: ${message}`);
      return false;
    }
  });
}

async function report(): Promise<void> {
  await withClient(async (client) => {
    const { rows: counts } = await client.query<{ table_name: string; rows: number }>(`
      SELECT 'merchants' AS table_name, count(*) AS rows FROM merchants
      UNION ALL SELECT 'transactions', count(*) FROM transactions
      UNION ALL SELECT 'disputes', count(*) FROM disputes
      UNION ALL SELECT 'dispute_events', count(*) FROM dispute_events
      UNION ALL SELECT 'ledger_transactions', count(*) FROM ledger_transactions
      UNION ALL SELECT 'ledger_entries', count(*) FROM ledger_entries
    `);

    console.log("\nrow counts");
    for (const row of counts) {
      console.log(`  ${row.table_name.padEnd(22)} ${row.rows.toLocaleString("en-US").padStart(12)}`);
    }

    const { rows: states } = await client.query<{ kind: string; state: string; n: number; exposure: number; currency: Currency }>(`
      SELECT kind, state, count(*) AS n, SUM(amount_minor) AS exposure, currency
        FROM disputes
       GROUP BY kind, state, currency
       ORDER BY kind, count(*) DESC`);

    console.log("\ndisputes by kind and state");
    for (const row of states) {
      console.log(
        `  ${row.kind.padEnd(11)} ${row.state.padEnd(12)} ${String(row.n).padStart(7)}  ` +
          `${formatMinor(row.exposure, row.currency).padStart(20)}`,
      );
    }

    const { rows: urgent } = await client.query<{ n: number; soonest: Date | null }>(`
      SELECT count(*) AS n, min(deadline_at) AS soonest
        FROM disputes
       WHERE state IN ('received','resolving') AND deadline_at > now()`);
    const first = urgent[0];
    console.log(
      `\nopen and on the clock: ${first?.n ?? 0} disputes` +
        (first?.soonest ? `, next deadline ${first.soonest.toISOString()}` : ""),
    );

    const { rows: overdue } = await client.query<{ n: number }>(`
      SELECT count(*) AS n FROM disputes
       WHERE state IN ('received','resolving') AND deadline_at <= now()`);
    // A dispute past its deadline that is still 'received' means nobody acted
    // in time and nobody recorded that either. The seeder never produces one;
    // if this is ever non-zero once Phase 3 is running, the worker is stuck.
    console.log(`past deadline but still unhandled: ${overdue[0]?.n ?? 0} (should always be 0)`);

    const { rows: balances } = await client.query<{
      name: string; kind: string; currency: Currency; balance_minor: number;
    }>(`
      SELECT COALESCE(m.name, 'PLATFORM') AS name, b.kind, b.currency, b.balance_minor
        FROM ledger_account_balances b
        LEFT JOIN merchants m ON m.id = b.merchant_id
       WHERE b.balance_minor <> 0
       ORDER BY m.name NULLS FIRST, b.kind`);

    const { rows: held } = await client.query<{ n: number; amount: number; currency: Currency }>(`
      SELECT count(*) AS n, COALESCE(SUM(amount_minor), 0) AS amount, currency
        FROM disputes
       WHERE kind = 'chargeback' AND state IN ('received','resolving','represented')
       GROUP BY currency ORDER BY 3`);

    console.log("\nfunds held pending a decision");
    for (const row of held) {
      console.log(`  ${String(row.n).padStart(6)} chargebacks  ${formatMinor(row.amount, row.currency).padStart(20)}`);
    }

    console.log("\nledger balances (derived, never stored)");
    for (const row of balances) {
      console.log(
        `  ${row.name.padEnd(20)} ${row.kind.padEnd(20)} ${formatMinor(row.balance_minor, row.currency).padStart(22)}`,
      );
    }
  });
}

export async function verify(): Promise<void> {
  const invariantsPassed = await runChecks();
  const triggerArmed = await checkTriggerIsArmed();
  await report();

  if (!invariantsPassed || !triggerArmed) {
    console.error("\nVERIFY FAILED");
    process.exitCode = 1;
  } else {
    console.log("\nall checks passed");
  }
  await pool.end();
}
