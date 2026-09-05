import { BIN_PREFIX, DEADLINE_HOURS, NAME_POOL, NETWORK_MIX, REASON_CODES } from "./catalog.js";
import type { CardNetwork, DisputeKind, MerchantProfile, ReasonCode } from "./catalog.js";
import { chance, int, logNormal, pick, weighted } from "./rand.js";
import type { Rng } from "./rand.js";
import { toMinor } from "./money.js";
import type { Currency } from "./money.js";

export const DAY_MS = 86_400_000;
export const HOUR_MS = 3_600_000;

export interface SeededMerchant extends MerchantProfile {
  readonly id: number;
  readonly customerCount: number;
}

export interface GeneratedTransaction {
  readonly merchantId: number;
  readonly externalId: string;
  readonly amountMinor: number;
  readonly currency: Currency;
  readonly cardNetwork: CardNetwork;
  readonly cardBin: string;
  readonly cardLast4: string;
  readonly customerRef: string;
  readonly customerEmail: string;
  readonly descriptor: string;
  readonly capturedAt: Date;
}

export function generateTransaction(
  rng: Rng,
  merchant: SeededMerchant,
  index: number,
  windowStart: number,
  windowEnd: number,
): GeneratedTransaction {
  const network = weighted(rng, NETWORK_MIX);
  const bin = pick(rng, BIN_PREFIX[network]);

  // Long-tailed amounts around the merchant's median ticket.
  const major = Math.min(
    Math.max(logNormal(rng, Math.log(merchant.medianTicket), 0.85), 1),
    merchant.medianTicket * 40,
  );

  // A pool of repeat customers, so "has this buyer disputed before?" has signal.
  const customerIndex = int(rng, 1, merchant.customerCount);
  const first = pick(rng, NAME_POOL.FIRST_NAMES);
  const last = pick(rng, NAME_POOL.LAST_NAMES);

  return {
    merchantId: merchant.id,
    externalId: `txn_${merchant.externalId.slice(4)}_${index.toString(36).padStart(7, "0")}`,
    amountMinor: toMinor(major, merchant.currency),
    currency: merchant.currency,
    cardNetwork: network,
    cardBin: bin,
    cardLast4: int(rng, 0, 9999).toString().padStart(4, "0"),
    customerRef: `cus_${merchant.externalId.slice(4)}_${customerIndex.toString(36).padStart(5, "0")}`,
    customerEmail: `${first}.${last}${customerIndex}@${pick(rng, NAME_POOL.EMAIL_HOSTS)}`,
    descriptor: merchant.descriptor,
    capturedAt: new Date(int(rng, windowStart, windowEnd)),
  };
}

export type DisputeState =
  | "received" | "resolving" | "refunded" | "represented" | "won" | "lost" | "expired";

export interface GeneratedDispute {
  readonly merchantId: number;
  readonly transactionId: number;
  readonly externalId: string;
  readonly kind: DisputeKind;
  readonly cardNetwork: CardNetwork;
  readonly reasonCode: string;
  readonly amountMinor: number;
  readonly currency: Currency;
  readonly state: DisputeState;
  readonly openedAt: Date;
  readonly deadlineAt: Date;
  readonly resolvedAt: Date | null;
}

export interface DisputeSource {
  readonly id: number;
  readonly merchantId: number;
  readonly externalId: string;
  readonly amountMinor: number;
  readonly currency: Currency;
  readonly cardNetwork: CardNetwork;
  readonly capturedAt: Date;
}

function pickReason(rng: Rng, network: CardNetwork): ReasonCode {
  const codes = REASON_CODES[network];
  return weighted(rng, codes.map((code) => [code, code.weight] as const));
}

/**
 * Turns a captured transaction into the dispute filed against it.
 *
 * The state is derived from where the deadline falls relative to `now`, which
 * is what makes the seeded data useful: some disputes are genuinely still open
 * with a clock running, so the worker and the dashboard have live work on the
 * very first run.
 */
export function generateDispute(
  rng: Rng,
  source: DisputeSource,
  now: number,
): GeneratedDispute {
  const kind: DisputeKind = chance(rng, 0.7) ? "alert" : "chargeback";
  const reason = pickReason(rng, source.cardNetwork);

  // Cardholders notice a charge days to months later - but never in the future.
  // Callers only pass transactions captured at least two days ago, so clamping
  // to `now` can never pull openedAt back before the capture.
  const openedAt = Math.min(
    source.capturedAt.getTime() + int(rng, 2, 75) * DAY_MS,
    now - int(rng, 1, 6) * HOUR_MS,
  );
  const [minHours, maxHours] = DEADLINE_HOURS[kind];
  const deadlineAt = openedAt + int(rng, minHours, maxHours) * HOUR_MS;

  // Partial disputes exist; most are for the full amount.
  const amountMinor = chance(rng, 0.12)
    ? Math.max(100, Math.round(source.amountMinor * (0.3 + rng() * 0.5)))
    : source.amountMinor;

  let state: DisputeState;
  let resolvedAt: number | null = null;

  if (deadlineAt > now) {
    // Still on the clock. This is the queue the worker drains.
    state = chance(rng, 0.85) ? "received" : "resolving";
  } else if (kind === "alert") {
    state = weighted(rng, [
      ["refunded", 84],
      // Missed the window entirely. Should be near zero; it is the metric the
      // dashboard exists to drive down.
      ["expired", 7],
      ["lost", 9],
    ] as const);
    resolvedAt = state === "expired" ? deadlineAt : openedAt + int(rng, 1, 20) * HOUR_MS;
  } else {
    state = weighted(rng, [
      ["lost", 44],
      ["won", 24],
      // Evidence submitted, network has not ruled yet - open, but not on the
      // deadline sweeper's list.
      ["represented", 22],
      ["expired", 10],
    ] as const);
    if (state !== "represented") {
      resolvedAt = state === "expired" ? deadlineAt : deadlineAt + int(rng, 1, 45) * DAY_MS;
      // A ruling that lands in the future has not happened yet.
      if (resolvedAt > now) resolvedAt = now - int(rng, 1, 12) * HOUR_MS;
    }
  }

  return {
    merchantId: source.merchantId,
    transactionId: source.id,
    externalId: `dsp_${source.externalId.slice(4)}`,
    kind,
    cardNetwork: source.cardNetwork,
    reasonCode: reason.code,
    amountMinor,
    currency: source.currency,
    state,
    openedAt: new Date(openedAt),
    deadlineAt: new Date(deadlineAt),
    resolvedAt: resolvedAt === null ? null : new Date(resolvedAt),
  };
}
