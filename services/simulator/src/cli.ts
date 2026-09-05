import { pool } from "./db.js";
import { migrate } from "./migrate.js";
import { seed } from "./seed.js";
import { verify } from "./verify.js";
import { emit } from "./emit.js";
import { rule } from "./rule.js";

function flag(name: string): boolean {
  return process.argv.includes(`--${name}`);
}

function option(name: string, fallback: number): number {
  const index = process.argv.indexOf(`--${name}`);
  if (index === -1) return fallback;
  const value = Number(process.argv[index + 1]);
  return Number.isFinite(value) ? value : fallback;
}

const USAGE = `
dispute-router simulator

  migrate   apply db/migrations/*.up.sql
  seed      truncate and regenerate the whole dataset (deterministic per SEED)
  verify    assert the money invariants and print a summary
  emit      stream signed dispute webhooks at the ingest service
  rule      send network rulings for disputes awaiting one (won/lost)

    --rate <n>    events per minute       (default 30)
    --count <n>   stop after n events     (default 0 = forever)
    --replay      send every event twice  (proves idempotency, or its absence)

  rule options
    --count <n>   how many disputes to rule on (default 25)
    --rate <n>    rulings per minute          (default 600)
`;

async function main(): Promise<void> {
  const command = process.argv[2];

  switch (command) {
    case "migrate":
      await migrate();
      await pool.end();
      break;
    case "seed":
      await seed();
      await pool.end();
      break;
    case "verify":
      // verify() closes the pool itself so it can set a non-zero exit code.
      await verify();
      break;
    case "emit":
      await emit({ rate: option("rate", 30), count: option("count", 0), replay: flag("replay") });
      break;
    case "rule":
      await rule({ count: option("count", 25), rate: option("rate", 600) });
      break;
    default:
      console.log(USAGE.trim());
      process.exitCode = command === undefined ? 0 : 1;
      await pool.end();
  }
}

main().catch(async (error: unknown) => {
  console.error(error);
  await pool.end().catch(() => undefined);
  process.exit(1);
});
