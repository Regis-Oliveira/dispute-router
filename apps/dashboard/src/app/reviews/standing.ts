/**
 * How a finished run should be presented to the person deciding about it.
 *
 * This is one function rather than a pair of conditions in two templates
 * because it encodes the rule the whole screen exists to protect: a run that
 * reached this queue with outcome 'rejected' was refused by the verifier and
 * escalated here only because the retries ran out and the deadline did not
 * stop. It must never be shown the same way as one that passed. Duplicated in
 * two templates, that rule survives exactly until somebody edits one of them.
 */
export type Standing = 'checked' | 'rejected' | 'no-case';

export function standingOf(outcome: string, recommendation: string): Standing {
  if (outcome === 'rejected') return 'rejected';
  if (recommendation === 'insufficient_evidence') return 'no-case';
  return 'checked';
}

/** Whether approving this needs the extra confirmation step. */
export function needsOverrideWarning(standing: Standing): boolean {
  return standing === 'rejected';
}

/**
 * Micro-dollars as a person reads them.
 *
 * Four decimal places, not two: a run costs a fraction of a cent and rounding
 * it to currency would print $0.00 for every one of them, which is the same as
 * printing nothing. This is the only division in the app's cost path, at the
 * edge, for the same reason money.ts divides exactly once.
 */
export function formatCost(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(4)}`;
}
