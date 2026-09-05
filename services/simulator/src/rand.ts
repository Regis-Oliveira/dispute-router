// Deterministic PRNG. Every generated row is a pure function of SEED, so
// "reproduce the bug on my machine" is `SEED=42 yarn seed`, not a database dump.

export type Rng = () => number;

export function makeRng(seed: number): Rng {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Integer in [min, max], inclusive. */
export function int(rng: Rng, min: number, max: number): number {
  return min + Math.floor(rng() * (max - min + 1));
}

export function pick<T>(rng: Rng, items: readonly T[]): T {
  const item = items[Math.floor(rng() * items.length)];
  if (item === undefined) throw new Error("pick() called on an empty array");
  return item;
}

/** Picks by relative weight. Weights need not sum to anything in particular. */
export function weighted<T>(rng: Rng, entries: readonly (readonly [T, number])[]): T {
  const total = entries.reduce((sum, [, weight]) => sum + weight, 0);
  let target = rng() * total;
  for (const [value, weight] of entries) {
    target -= weight;
    if (target <= 0) return value;
  }
  const last = entries.at(-1);
  if (!last) throw new Error("weighted() called on an empty array");
  return last[0];
}

export function chance(rng: Rng, probability: number): boolean {
  return rng() < probability;
}

/**
 * Log-normal draw. Real transaction amounts are not uniform: a long tail of
 * small charges with a thin tail of large ones. Uniform amounts make every
 * aggregate look wrong in a way that is hard to notice.
 */
export function logNormal(rng: Rng, mu: number, sigma: number): number {
  // Box-Muller.
  const u1 = Math.max(rng(), Number.EPSILON);
  const u2 = rng();
  const normal = Math.sqrt(-2 * Math.log(u1)) * Math.cos(2 * Math.PI * u2);
  return Math.exp(mu + sigma * normal);
}
