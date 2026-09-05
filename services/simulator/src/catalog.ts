import type { Currency } from "./money.js";

export type CardNetwork = "visa" | "mastercard" | "amex" | "discover";
export type DisputeKind = "alert" | "chargeback";

export interface ReasonCode {
  readonly code: string;
  readonly label: string;
  /** Fraud reasons are rarely winnable; service reasons often are. */
  readonly category: "fraud" | "service" | "processing";
  readonly weight: number;
}

/**
 * Real reason codes, because the categories drive the auto-decision rules and
 * fake codes would let a wrong rule look right.
 */
export const REASON_CODES: Record<CardNetwork, readonly ReasonCode[]> = {
  visa: [
    { code: "10.4", label: "Other Fraud - Card Absent Environment", category: "fraud", weight: 40 },
    { code: "13.1", label: "Merchandise/Services Not Received", category: "service", weight: 20 },
    { code: "13.3", label: "Not as Described or Defective", category: "service", weight: 12 },
    { code: "13.6", label: "Credit Not Processed", category: "service", weight: 10 },
    { code: "13.7", label: "Cancelled Merchandise/Services", category: "service", weight: 10 },
    { code: "12.5", label: "Incorrect Amount", category: "processing", weight: 5 },
    { code: "12.6.1", label: "Duplicate Processing", category: "processing", weight: 3 },
  ],
  mastercard: [
    { code: "4837", label: "No Cardholder Authorization", category: "fraud", weight: 38 },
    { code: "4853", label: "Cardholder Dispute", category: "service", weight: 25 },
    { code: "4855", label: "Goods or Services Not Provided", category: "service", weight: 18 },
    { code: "4834", label: "Point-of-Interaction Error", category: "processing", weight: 9 },
    { code: "4841", label: "Cancelled Recurring Transaction", category: "service", weight: 10 },
  ],
  amex: [
    { code: "F29", label: "Card Not Present", category: "fraud", weight: 42 },
    { code: "C08", label: "Goods/Services Not Received", category: "service", weight: 24 },
    { code: "C31", label: "Goods/Services Not As Described", category: "service", weight: 16 },
    { code: "P08", label: "Duplicate Charge", category: "processing", weight: 8 },
    { code: "C02", label: "Credit Not Processed", category: "service", weight: 10 },
  ],
  discover: [
    { code: "UA02", label: "Fraud - Card Not Present", category: "fraud", weight: 40 },
    { code: "RG", label: "Non-Receipt of Goods or Services", category: "service", weight: 24 },
    { code: "RM", label: "Quality of Goods or Services", category: "service", weight: 18 },
    { code: "DP", label: "Duplicate Processing", category: "processing", weight: 8 },
    { code: "AP", label: "Cancelled Recurring Transaction", category: "service", weight: 10 },
  ],
};

/** Issuer BIN ranges, only realistic enough that the first digit matches the network. */
export const BIN_PREFIX: Record<CardNetwork, readonly string[]> = {
  visa: ["400934", "411111", "424242", "453210", "476173"],
  mastercard: ["510510", "522222", "540123", "552345", "545454"],
  amex: ["340000", "371449", "378282", "379999"],
  discover: ["601100", "644564", "650000", "601109"],
};

export const NETWORK_MIX = [
  ["visa", 52],
  ["mastercard", 28],
  ["amex", 13],
  ["discover", 7],
] as const satisfies readonly (readonly [CardNetwork, number])[];

/**
 * Response windows, in hours.
 *
 * Alerts are the reason this product exists: a day or two to refund before a
 * chargeback is ever filed. Chargebacks give you weeks, but the money is
 * already gone and you are arguing to get it back.
 */
export const DEADLINE_HOURS: Record<DisputeKind, readonly [number, number]> = {
  alert: [24, 72],
  chargeback: [20 * 24, 30 * 24],
};

export interface MerchantProfile {
  readonly externalId: string;
  readonly name: string;
  readonly currency: Currency;
  readonly descriptor: string;
  /** Multiplies the base dispute rate. Subscription businesses get hit harder. */
  readonly disputeMultiplier: number;
  /** Median charge in major units; feeds the log-normal amount draw. */
  readonly medianTicket: number;
  /** Auto-refund ceiling in major units, or null to leave the automation off. */
  readonly autoRefundCeiling: number | null;
}

export const MERCHANT_PROFILES: readonly MerchantProfile[] = [
  { externalId: "mrc_northwind",  name: "Northwind Supply",      currency: "USD", descriptor: "NORTHWIND SUPPLY",  disputeMultiplier: 0.6, medianTicket: 84,  autoRefundCeiling: 50 },
  { externalId: "mrc_lumen",      name: "Lumen Fitness",         currency: "USD", descriptor: "LUMEN FITNESS",     disputeMultiplier: 2.4, medianTicket: 39,  autoRefundCeiling: 60 },
  { externalId: "mrc_paperclip",  name: "Paperclip Software",    currency: "USD", descriptor: "PAPERCLIP SW",      disputeMultiplier: 1.7, medianTicket: 129, autoRefundCeiling: 150 },
  { externalId: "mrc_tidepool",   name: "Tidepool Cosmetics",    currency: "USD", descriptor: "TIDEPOOL BEAUTY",   disputeMultiplier: 1.1, medianTicket: 56,  autoRefundCeiling: 40 },
  { externalId: "mrc_havenroast", name: "Haven Roasters",        currency: "USD", descriptor: "HAVEN ROASTERS",    disputeMultiplier: 0.4, medianTicket: 27,  autoRefundCeiling: 30 },
  { externalId: "mrc_orbital",    name: "Orbital Gear",          currency: "USD", descriptor: "ORBITAL GEAR",      disputeMultiplier: 0.9, medianTicket: 210, autoRefundCeiling: null },
  { externalId: "mrc_bellweather",name: "Bellweather Travel",    currency: "EUR", descriptor: "BELLWEATHER TRVL",  disputeMultiplier: 1.9, medianTicket: 430, autoRefundCeiling: null },
  { externalId: "mrc_kestrel",    name: "Kestrel Audio",         currency: "GBP", descriptor: "KESTREL AUDIO",     disputeMultiplier: 1.3, medianTicket: 95,  autoRefundCeiling: 75 },
];

const FIRST_NAMES = ["ana","bruno","clara","diego","elena","felix","gina","hugo","iris","joao","kira","luca","mara","nuno","olga","paulo","rita","sofia","tomas","vera","wes","yara","zeca","noah","mia","liam","emma","oliver","ava","ethan"] as const;
const LAST_NAMES  = ["alves","barros","costa","duarte","esteves","fonseca","gomes","henriques","imperato","jardim","klein","lopes","matos","neves","oliveira","pinto","queiroz","ramos","santos","teixeira","valente","weber","xavier","young","zanetti"] as const;
const EMAIL_HOSTS = ["example.com","mail.test","inbox.test","proton.test","fastmail.test"] as const;

export const NAME_POOL = { FIRST_NAMES, LAST_NAMES, EMAIL_HOSTS } as const;
