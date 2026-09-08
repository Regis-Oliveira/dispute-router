import { describe, expect, it } from 'vitest';
import { formatCost, needsOverrideWarning, standingOf } from './standing';

describe('standingOf', () => {
  it('marks a verifier-approved draft as checked', () => {
    expect(standingOf('drafted', 'represent')).toBe('checked');
  });

  /**
   * The rule the screen exists to protect. A rejected run is in this queue
   * because the retries ran out and the deadline did not - not because anything
   * approved it - and it must not be presented as checked.
   */
  it('never presents a rejected run as checked', () => {
    expect(standingOf('rejected', 'represent')).toBe('rejected');
    // Even though the recommendation still says represent, which is what the
    // generator wanted before the verifier disagreed.
    expect(standingOf('rejected', 'represent')).not.toBe('checked');
  });

  it('separates "no case to make" from "the draft is wrong"', () => {
    expect(standingOf('insufficient_evidence', 'insufficient_evidence')).toBe('no-case');
    expect(standingOf('rejected', 'insufficient_evidence')).toBe('rejected');
  });

  it('asks for a second click only when overriding a rejection', () => {
    expect(needsOverrideWarning(standingOf('rejected', 'represent'))).toBe(true);
    expect(needsOverrideWarning(standingOf('drafted', 'represent'))).toBe(false);
    expect(needsOverrideWarning(standingOf('insufficient_evidence', 'insufficient_evidence'))).toBe(false);
  });
});

describe('formatCost', () => {
  /**
   * Two decimal places would print $0.00 for every run in the system, which is
   * the same as printing nothing at all.
   */
  it('keeps enough digits for a fraction of a cent', () => {
    expect(formatCost(15_000)).toBe('$0.0150');
    expect(formatCost(250)).toBe('$0.0003');
    expect(formatCost(0)).toBe('$0.0000');
  });

  it('still reads at the scale of a real bill', () => {
    expect(formatCost(2_500_000)).toBe('$2.5000');
  });
});
