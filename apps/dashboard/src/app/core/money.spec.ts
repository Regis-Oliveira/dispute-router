import { describe, expect, it } from 'vitest';
import { formatCountdown, formatMinor, minorDigits, urgencyOf } from './money';

describe('formatMinor', () => {
  it('renders two-digit currencies from their minor units', () => {
    expect(formatMinor(4999, 'USD')).toBe('$49.99');
    expect(formatMinor(0, 'USD')).toBe('$0.00');
    expect(formatMinor(1, 'USD')).toBe('$0.01');
  });

  it('groups large amounts', () => {
    expect(formatMinor(5_272_999_877, 'USD')).toBe('$52,729,998.77');
  });

  /**
   * The reason the digits map exists. A blanket "divide by 100" turns 5,000 yen
   * into 50 yen, and nothing about the result looks wrong.
   */
  it('does not invent decimal places for zero-decimal currencies', () => {
    expect(formatMinor(5000, 'JPY')).toBe('¥5,000');
    expect(minorDigits('JPY')).toBe(0);
  });

  it('falls back to two digits for a currency it has never seen', () => {
    expect(minorDigits('XYZ')).toBe(2);
  });

  it('never loses a cent to floating point', () => {
    // 0.1 + 0.2 territory: these are the amounts that expose a float pipeline.
    expect(formatMinor(1010, 'USD')).toBe('$10.10');
    expect(formatMinor(2030, 'USD')).toBe('$20.30');
    expect(formatMinor(999_999_999, 'USD')).toBe('$9,999,999.99');
  });
});

describe('formatCountdown', () => {
  it('reads in the units a person acts on', () => {
    expect(formatCountdown(45 * 60)).toBe('45m');
    expect(formatCountdown(4 * 3600 + 12 * 60)).toBe('4h 12m');
    expect(formatCountdown(3 * 86_400 + 5 * 3600)).toBe('3d 5h');
  });

  it('labels a passed deadline rather than showing a negative number', () => {
    expect(formatCountdown(-2 * 3600)).toBe('overdue 2h 0m');
    expect(formatCountdown(-30 * 60)).toBe('overdue 30m');
  });
});

describe('urgencyOf', () => {
  it('bands an open dispute by how much clock is left', () => {
    expect(urgencyOf(-1, null)).toBe('overdue');
    expect(urgencyOf(3 * 3600, null)).toBe('critical');
    expect(urgencyOf(12 * 3600, null)).toBe('soon');
    expect(urgencyOf(5 * 86_400, null)).toBe('ok');
  });

  /** A resolved dispute has no clock, however far past its deadline it is. */
  it('treats a resolved dispute as closed regardless of its deadline', () => {
    expect(urgencyOf(-99_999, '2026-01-01T00:00:00Z')).toBe('closed');
  });
});
