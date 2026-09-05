// Money is a BIGINT count of minor units plus a currency code. There is no
// float anywhere in this codebase, and no bare number that means "dollars".

export type Currency = "USD" | "EUR" | "GBP";

const MINOR_UNITS: Record<Currency, number> = { USD: 2, EUR: 2, GBP: 2 };

export function toMinor(major: number, currency: Currency): number {
  const scale = 10 ** MINOR_UNITS[currency];
  // Round once, at the boundary, and never again.
  return Math.round(major * scale);
}

export function formatMinor(minor: number | bigint, currency: Currency): string {
  const value = typeof minor === "bigint" ? minor : BigInt(Math.trunc(minor));
  const scale = BigInt(10 ** MINOR_UNITS[currency]);
  const sign = value < 0n ? "-" : "";
  const abs = value < 0n ? -value : value;
  const whole = abs / scale;
  const frac = (abs % scale).toString().padStart(MINOR_UNITS[currency], "0");
  return `${sign}${whole.toLocaleString("en-US")}.${frac} ${currency}`;
}
