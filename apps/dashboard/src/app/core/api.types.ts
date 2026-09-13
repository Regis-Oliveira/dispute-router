/**
 * The shape the Go read API returns. Kept in one file so a change on the server
 * has exactly one place to land on the client.
 */

/**
 * Money never crosses the wire as a formatted string or a float. It is an
 * integer count of minor units plus its currency, and it stays that way until
 * the moment it is rendered.
 */
export interface Money {
  amount_minor: number;
  currency: string;
}

export type DisputeKind = 'alert' | 'chargeback';

export type DisputeState =
  | 'received'
  | 'resolving'
  | 'draft_ready'
  | 'refunded'
  | 'represented'
  | 'won'
  | 'lost'
  | 'expired';

export type CardNetwork = 'visa' | 'mastercard' | 'amex' | 'discover';

export interface DisputeRow {
  id: number;
  external_id: string;
  merchant_id: string;
  merchant_name: string;
  kind: DisputeKind;
  state: DisputeState;
  card_network: CardNetwork;
  reason_code: string;
  amount: Money;
  opened_at: string;
  deadline_at: string;
  resolved_at: string | null;
  transaction_ref: string;
  card_last4: string;
  customer_ref: string;
  /** Negative once the deadline has passed. Measured server-side so every row on a page shares one clock. */
  seconds_to_deadline: number;
}

export interface Page {
  offset: number;
  limit: number;
  total: number;
}

export interface DisputeList {
  rows: DisputeRow[];
  page: Page;
  took_ms: number;
}

export interface DisputeEvent {
  from_state: string | null;
  to_state: string;
  actor: string;
  detail: Record<string, unknown>;
  occurred_at: string;
}

export interface LedgerPosting {
  external_ref: string;
  kind: string;
  account: string;
  direction: 'debit' | 'credit';
  amount: Money;
  occurred_at: string;
}

export interface DisputeDetail extends DisputeRow {
  customer_email: string;
  descriptor: string;
  captured_at: string;
  original_amount: Money;
  refunded_minor: number;
  events: DisputeEvent[];
  ledger_postings: LedgerPosting[];
}

export interface StateCount {
  state: DisputeState;
  kind: DisputeKind;
  count: number;
}

export interface Exposure {
  currency: string;
  amount_minor: number;
  count: number;
}

export type DeadlineBucketName = 'overdue' | '6h' | '24h' | '72h' | 'later';

export interface DeadlineBucket {
  bucket: DeadlineBucketName;
  count: number;
}

export interface DailyPoint {
  day: string;
  alerts: number;
  chargebacks: number;
}

export interface Summary {
  states: StateCount[];
  open_value: Exposure[];
  deadlines: DeadlineBucket[];
  daily: DailyPoint[];
  took_ms: number;
}

export interface MerchantOption {
  external_id: string;
  name: string;
  currency: string;
  disputes: number;
}

/** One arrival on the live feed, forwarded from the ingest service's outbox. */
export interface LiveEvent {
  outbox_id: number;
  event_type: string;
  aggregate_id: number;
  payload: {
    dispute_id: number;
    merchant_id: number;
    kind: DisputeKind;
    amount_minor: number;
    currency: string;
    deadline_at: string;
  };
  at: string;
}

/** A file already filed against a dispute. */
export interface EvidenceFile {
  key: string;
  name: string;
  size_bytes: number;
  uploaded_at: string;
  /** Short-lived presigned GET. Never a public link: evidence is a customer's receipt. */
  url: string;
}

/**
 * A signed policy the browser posts a form to.
 *
 * `fields` carry the policy and its signature. They go into the form first and
 * the file goes last, because S3 stops reading fields once it reaches the file
 * part - a field after it is simply never seen.
 */
export interface UploadTarget {
  key: string;
  url: string;
  expires_at: string;
  fields: Record<string, string>;
  /** The same limit the signed policy encodes. S3 enforces it either way. */
  max_bytes: number;
  content_type: string;
}

/**
 * A rule the draft broke, from the verifier or from the host-side citation
 * check. The two are indistinguishable here on purpose: a reviewer cares what
 * is wrong with the letter, not which stage noticed.
 */
export interface ReviewFinding {
  check: string;
  quote: string;
  why: string;
}

/** One entry in the review queue. */
export interface ReviewRow {
  run_id: number;
  dispute_id: number;
  reference: string;
  merchant: string;
  reason_code: string;
  amount: Money;

  /**
   * 'drafted' means the verifier passed it. 'rejected' means it did not, and
   * the run reached this queue by escalation - the retries ran out and the
   * deadline did not stop moving. The distinction has to survive to the screen:
   * a letter the verifier refused must never look checked.
   */
  outcome: string;
  recommendation: string;
  findings: number;
  cost_micros: number;
  attempt: number;

  seconds_to_deadline: number;
  finished_at: string;
}

/** Everything a reviewer needs on one screen. */
export interface ReviewDetail {
  run_id: number;
  dispute_id: number;
  attempt: number;

  outcome: string;
  recommendation: string;
  letter: string;

  cited_evidence: string[];
  findings: ReviewFinding[];

  model: string;
  prompt_fingerprint: string;
  tool_surface: string[];
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;

  started_at: string;
  finished_at: string;

  decided?: string;
  decided_by?: string;
  decided_at?: string;

  /** Read fresh, not stored on the run: the record can move between drafting and reviewing. */
  dispute: DisputeDetail;
}

/** One decision a person made about one draft. */
export interface DecisionRow {
  run_id: number;
  dispute_id: number;
  reference: string;
  merchant: string;
  amount: Money;

  /** What the agent concluded. */
  outcome: string;
  recommendation: string;
  findings: number;
  attempt: number;
  cost_micros: number;

  /** What the person decided. */
  review: string;
  reviewed_by: string;
  reviewed_at: string;

  /**
   * The verifier refused this letter and a person sent it anyway. Computed by
   * the API, not derived here, so every reader agrees on what one is.
   */
  override: boolean;

  dispute_state: string;
}

export interface DecisionList {
  rows: DecisionRow[];
  page: Page;
  /**
   * Every identity that has ever decided, over the whole history rather than
   * the current page or filter - a control built from filtered results removes
   * the people who selected it.
   */
  reviewers: string[];
  took_ms: number;
}
