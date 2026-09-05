import pg from "pg";
import { env } from "./env.js";

// BIGINT (oid 20) arrives as a string so that ids past 2^53 survive the trip.
// Every count and amount this service reads back is small enough for Number,
// so parse it here rather than sprinkling Number() over the call sites.
pg.types.setTypeParser(pg.types.builtins.INT8, (value) => Number(value));

export const pool = new pg.Pool({
  connectionString: env.databaseUrl,
  max: 4,
  application_name: "dispute-router-simulator",
});

export async function withClient<T>(fn: (client: pg.PoolClient) => Promise<T>): Promise<T> {
  const client = await pool.connect();
  try {
    return await fn(client);
  } finally {
    client.release();
  }
}

/** Runs fn inside a transaction, rolling back on any throw. */
export async function withTransaction<T>(fn: (client: pg.PoolClient) => Promise<T>): Promise<T> {
  return withClient(async (client) => {
    await client.query("BEGIN");
    try {
      const result = await fn(client);
      await client.query("COMMIT");
      return result;
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    }
  });
}

export { pg };
