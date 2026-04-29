-- Drop in dependency order: entries and audit_log reference transactions and accounts.
DROP TRIGGER IF EXISTS trig_check_double_entry ON entries;
DROP FUNCTION IF EXISTS check_double_entry_balance();

DROP TABLE IF EXISTS entries;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;
