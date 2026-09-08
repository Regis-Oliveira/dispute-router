import { describe, expect, it } from 'vitest';
import type { DisputeState } from './api.types';

/**
 * The contradiction, as a pure function of the two filters involved.
 *
 * Mirrors FiltersStore.openOnlyExcludesEverything, which cannot be exercised
 * here without a TestBed - this project's specs are function-level, so the rule
 * is tested at the level it can be. What it protects is a real dead end: an
 * operator ticks a state that exists 708 times, gets an empty table, and the
 * only thing standing between them and the rows is a checkbox they never
 * touched.
 */
const SETTLED: readonly DisputeState[] = ['represented', 'refunded', 'won', 'lost', 'expired'];

function contradicts(openOnly: boolean, states: readonly DisputeState[]): boolean {
  return openOnly && states.length > 0 && states.every((s) => SETTLED.includes(s));
}

describe('open-only against settled states', () => {
  it('flags the combination that can never match', () => {
    expect(contradicts(true, ['represented'])).toBe(true);
    expect(contradicts(true, ['won', 'lost'])).toBe(true);
  });

  it('says nothing when one selected state is still reachable while open', () => {
    // received is an open state, so the filters are satisfiable and the empty
    // table would mean what it says.
    expect(contradicts(true, ['represented', 'received'])).toBe(false);
  });

  it('says nothing once closed disputes are included', () => {
    expect(contradicts(false, ['represented'])).toBe(false);
  });

  it('says nothing when no state is selected at all', () => {
    expect(contradicts(true, [])).toBe(false);
  });
});
