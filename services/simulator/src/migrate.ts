import { readdir, readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { withClient } from "./db.js";

const here = dirname(fileURLToPath(import.meta.url));

const MIGRATIONS_DIR = resolve(here, "../../../db/migrations");

/**
 * Deliberately the same file layout golang-migrate expects
 * (NNNNNN_name.up.sql / .down.sql), so the Go services in Phase 1 can take
 * over migrations without anyone renaming anything.
 */
export async function migrate(): Promise<void> {
  const files = (await readdir(MIGRATIONS_DIR))
    .filter((name) => name.endsWith(".up.sql"))
    .sort();

  await withClient(async (client) => {
    await client.query(`
      CREATE TABLE IF NOT EXISTS schema_migrations (
        version     TEXT PRIMARY KEY,
        applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
      )
    `);

    const { rows } = await client.query<{ version: string }>("SELECT version FROM schema_migrations");
    const applied = new Set(rows.map((row) => row.version));

    for (const file of files) {
      const version = file.replace(/\.up\.sql$/, "");
      if (applied.has(version)) {
        console.log(`  = ${version} (already applied)`);
        continue;
      }

      const sql = await readFile(resolve(MIGRATIONS_DIR, file), "utf8");
      const started = Date.now();

      // The file and its bookkeeping row commit together. A migration that
      // fails halfway leaves the database exactly as it was, and never leaves
      // a version marked applied that isn't.
      await client.query("BEGIN");
      try {
        await client.query(sql);
        await client.query("INSERT INTO schema_migrations (version) VALUES ($1)", [version]);
        await client.query("COMMIT");
      } catch (error) {
        await client.query("ROLLBACK");
        throw new Error(`migration ${version} failed: ${(error as Error).message}`, { cause: error });
      }

      console.log(`  + ${version} (${Date.now() - started}ms)`);
    }
  });
}
