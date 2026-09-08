// Money is a BIGINT count of minor units plus a currency code. There is no
// float anywhere in this codebase, and no bare number that means "dollars".

export type Currency = "USD" | "EUR" | "GBP" | "JPY";

// Not every currency has cents. JPY has no minor unit at all, which is why
// this is a table and not the number two: ¥5,000 stored as 5000 minor units
// renders as ¥5,000, and a formatter that assumes two decimals renders ¥50.
const MINOR_UNITS: Record<Currency, number> = { USD: 2, EUR: 2, GBP: 2, JPY: 0 };

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
  const digits = MINOR_UNITS[currency];
  if (digits === 0) {
    // No fractional part, and therefore no separator. Without this the yen
    // amount comes out as "5,000. JPY".
    return `${sign}${whole.toLocaleString("en-US")} ${currency}`;
  }
  const frac = (abs % scale).toString().padStart(digits, "0");
  return `${sign}${whole.toLocaleString("en-US")}.${frac} ${currency}`;
}
