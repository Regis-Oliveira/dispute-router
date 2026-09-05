import { Pipe, type PipeTransform } from '@angular/core';
import type { Money } from './api.types';

/**
 * Minor-unit digits per currency. Not every currency has two: JPY has none, and
 * dividing a yen amount by 100 produces a number that is wrong by a factor of
 * one hundred without ever looking wrong.
 */
const MINOR_DIGITS: Readonly<Record<string, number>> = {
  USD: 2,
  EUR: 2,
  GBP: 2,
  JPY: 0,
  KWD: 3,
};

export function minorDigits(currency: string): number {
  return MINOR_DIGITS[currency] ?? 2;
}

/**
 * Renders an integer count of minor units.
 *
 * The division here is the only place in the client where money becomes a
 * fractional number, it happens at render time, and its result is thrown away
 * immediately. No arithmetic is ever done on the divided value - a total is
 * summed in minor units and formatted once.
 */
export function formatMinor(amountMinor: number, currency: string, locale = 'en-US'): string {
  const digits = minorDigits(currency);
  return new Intl.NumberFormat(locale, {
    style: 'currency',
    currency,
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  }).format(amountMinor / 10 ** digits);
}

@Pipe({ name: 'money' })
export class MoneyPipe implements PipeTransform {
  transform(value: Money | null | undefined): string {
    if (!value) return '—';
    return formatMinor(value.amount_minor, value.currency);
  }
}

/**
 * A deadline as a person reads it: "4h 12m", or "overdue 2h" once it has passed.
 * Deliberately coarse - nobody acts on seconds, and a ticking second counter on
 * fifty rows is motion without information.
 */
export function formatCountdown(seconds: number): string {
  const overdue = seconds < 0;
  const total = Math.abs(seconds);

  const days = Math.floor(total / 86_400);
  const hours = Math.floor((total % 86_400) / 3_600);
  const minutes = Math.floor((total % 3_600) / 60);

  let text: string;
  if (days > 0) text = `${days}d ${hours}h`;
  else if (hours > 0) text = `${hours}h ${minutes}m`;
  else text = `${minutes}m`;

  return overdue ? `overdue ${text}` : text;
}

/** Urgency band, used for the row's colour and its sort-by-attention order. */
export type Urgency = 'overdue' | 'critical' | 'soon' | 'ok' | 'closed';

export function urgencyOf(secondsToDeadline: number, resolvedAt: string | null): Urgency {
  if (resolvedAt !== null) return 'closed';
  if (secondsToDeadline < 0) return 'overdue';
  if (secondsToDeadline < 6 * 3_600) return 'critical';
  if (secondsToDeadline < 24 * 3_600) return 'soon';
  return 'ok';
}
