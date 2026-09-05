DROP VIEW IF EXISTS ledger_account_balances;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;
DROP FUNCTION IF EXISTS assert_ledger_balanced();
DROP FUNCTION IF EXISTS forbid_mutation();