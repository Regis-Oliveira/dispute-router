import { pipeline } from "node:stream/promises";
import { Readable } from "node:stream";
import copyStreams from "pg-copy-streams";
import type { PoolClient } from "pg";

// COPY ... FROM STDIN in text format. Roughly an order of magnitude faster than
// batched multi-row INSERTs, and the reason seeding 500k rows takes seconds
// rather than minutes.

export type CopyValue = string | number | bigint | boolean | Date | null | undefined;

const ESCAPES: Readonly<Record<string, string>> = {
  "\\": "\\\\",
  "\n": "\\n",
  "\r": "\\r",
  "\t": "\\t",
};

function encode(value: CopyValue): string {
  if (value === null || value === undefined) return "\\N";
  if (value instanceof Date) return value.toISOString();
  if (typeof value === "boolean") return value ? "t" : "f";
  if (typeof value === "number" || typeof value === "bigint") return String(value);
  return value.replace(/[\\\n\r\t]/g, (char) => ESCAPES[char] ?? char);
}

export function encodeRow(values: readonly CopyValue[]): string {
  return `${values.map(encode).join("\t")}\n`;
}

/**
 * Streams rows into `table`. The source is an async iterable so the generator
 * and the socket run concurrently and no more than a chunk of rows is ever
 * resident in memory - the same shape you'd use for a 50M-row load.
 */
export async function copyInto(
  client: PoolClient,
  table: string,
  columns: readonly string[],
  rows: AsyncIterable<readonly CopyValue[]> | Iterable<readonly CopyValue[]>,
): Promise<void> {
  const sql = `COPY ${table} (${columns.join(", ")}) FROM STDIN WITH (FORMAT text)`;
  const target = client.query(copyStreams.from(sql));

  async function* serialize(): AsyncGenerator<string> {
    // Batch into ~64KB chunks; one write() per row throttles on backpressure.
    let buffer = "";
    for await (const row of rows as AsyncIterable<readonly CopyValue[]>) {
      buffer += encodeRow(row);
      if (buffer.length >= 65_536) {
        yield buffer;
        buffer = "";
      }
    }
    if (buffer.length > 0) yield buffer;
  }

  await pipeline(Readable.from(serialize()), target);
}
