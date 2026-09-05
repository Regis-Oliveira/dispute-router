import { config } from "dotenv";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// The repo root .env is the single source of truth; each service reads it.
config({ path: resolve(here, "../../../.env"), quiet: true });

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`missing required env var ${name}`);
  return value;
}

function num(name: string, fallback: number): number {
  const raw = process.env[name];
  if (raw === undefined || raw === "") return fallback;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed)) throw new Error(`env var ${name} is not a number: ${raw}`);
  return parsed;
}

export const env = {
  databaseUrl: required("DATABASE_URL"),
  redisUrl: process.env.REDIS_URL ?? "redis://localhost:6379",
  ingestUrl: process.env.INGEST_URL ?? "http://localhost:8080/webhooks/processor",

  seed: num("SEED", 42),
  merchants: num("SEED_MERCHANTS", 8),
  transactions: num("SEED_TRANSACTIONS", 500_000),
  disputeRate: num("SEED_DISPUTE_RATE", 0.02),
} as const;
