import { createHmac, randomUUID, timingSafeEqual } from "node:crypto";

// Stripe-style webhook signatures. Worth copying rather than inventing, because
// the two mistakes it avoids are the two everyone makes:
//   * signing the body alone, so a captured request replays forever
//   * comparing with ===, which leaks the correct prefix through timing
//
// The Go ingest service in Phase 1 reimplements the verify side of this.
// Getting both ends to agree is the first thing that will break; that is the
// exercise.

const TOLERANCE_SECONDS = 300;

export function sign(secret: string, body: string, timestampSeconds = Math.floor(Date.now() / 1000)): string {
  const signature = createHmac("sha256", secret)
    .update(`${timestampSeconds}.${body}`)
    .digest("hex");
  return `t=${timestampSeconds},v1=${signature}`;
}

export function verifySignature(secret: string, body: string, header: string): boolean {
  const parts = new Map(
    header.split(",").map((part) => {
      const [key = "", value = ""] = part.split("=", 2);
      return [key.trim(), value.trim()] as const;
    }),
  );

  const timestamp = Number(parts.get("t"));
  const provided = parts.get("v1");
  if (!Number.isFinite(timestamp) || !provided) return false;

  // Reject anything outside the replay window before spending a hash on it.
  if (Math.abs(Math.floor(Date.now() / 1000) - timestamp) > TOLERANCE_SECONDS) return false;

  const expected = createHmac("sha256", secret).update(`${timestamp}.${body}`).digest();
  const actual = Buffer.from(provided, "hex");
  if (actual.length !== expected.length) return false;

  return timingSafeEqual(expected, actual);
}

export function newIdempotencyKey(): string {
  return `evt_${randomUUID()}`;
}
