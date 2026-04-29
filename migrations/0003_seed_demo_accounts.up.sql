-- Seed three demo user accounts with initial DEPOSIT transactions.
-- Each deposit is modelled as:
--   DEBIT  EXTERNAL   amount  (EXTERNAL pays out)
--   CREDIT demo_acct  amount  (demo account receives)
--
-- Balance arithmetic after seeding:
--   Alice   balance =  10000.0000  (sum of CREDIT entries for Alice)
--   Bob     balance =  10000.0000  (sum of CREDIT entries for Bob)
--   Charlie balance =  10000.0000  (sum of CREDIT entries for Charlie)
--   EXTERNAL balance = -30000.0000 (-(10000+10000+10000); represents the
--   aggregate liability to all funded accounts — canonical double-entry
--   semantics for a system source account)
--
-- ON CONFLICT DO NOTHING on accounts and transactions makes this idempotent.

-- Insert demo accounts with balances matching their CREDIT entries below.
INSERT INTO accounts (id, name, is_system, balance)
VALUES
    ('00000000-0000-0000-0000-000000000002', 'Alice',   FALSE, 10000.0000),
    ('00000000-0000-0000-0000-000000000003', 'Bob',     FALSE, 10000.0000),
    ('00000000-0000-0000-0000-000000000004', 'Charlie', FALSE, 10000.0000)
ON CONFLICT DO NOTHING;

-- Update EXTERNAL balance to reflect the aggregate liability.
-- After these three deposits, EXTERNAL has debited 30000 total.
-- This UPDATE is skipped if the accounts INSERT was a no-op (idempotent re-run).
UPDATE accounts
SET balance    = -30000.0000,
    updated_at = now()
WHERE id = '00000000-0000-0000-0000-000000000001'
  AND NOT EXISTS (
      SELECT 1 FROM transactions WHERE idempotency_key = 'seed-deposit-alice-001'
  );

-- Insert seed deposit transactions (idempotent via ON CONFLICT DO NOTHING).
INSERT INTO transactions (id, kind, idempotency_key)
VALUES
    ('01900000-0000-7000-8000-000000000001', 'DEPOSIT', 'seed-deposit-alice-001'),
    ('01900000-0000-7000-8000-000000000002', 'DEPOSIT', 'seed-deposit-bob-001'),
    ('01900000-0000-7000-8000-000000000003', 'DEPOSIT', 'seed-deposit-charlie-001')
ON CONFLICT DO NOTHING;

-- Insert entries for Alice's deposit.
-- Uses INSERT ... ON CONFLICT DO NOTHING is not possible for BIGSERIAL PK without
-- a unique constraint on (transaction_id, account_id, direction). Instead, guard
-- with NOT EXISTS to remain idempotent.
INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000001', '00000000-0000-0000-0000-000000000001', 'DEBIT',  10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000001'
      AND account_id     = '00000000-0000-0000-0000-000000000001'
      AND direction      = 'DEBIT'
);

INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000001', '00000000-0000-0000-0000-000000000002', 'CREDIT', 10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000001'
      AND account_id     = '00000000-0000-0000-0000-000000000002'
      AND direction      = 'CREDIT'
);

-- Insert entries for Bob's deposit.
INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000002', '00000000-0000-0000-0000-000000000001', 'DEBIT',  10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000002'
      AND account_id     = '00000000-0000-0000-0000-000000000001'
      AND direction      = 'DEBIT'
);

INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000002', '00000000-0000-0000-0000-000000000003', 'CREDIT', 10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000002'
      AND account_id     = '00000000-0000-0000-0000-000000000003'
      AND direction      = 'CREDIT'
);

-- Insert entries for Charlie's deposit.
INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000003', '00000000-0000-0000-0000-000000000001', 'DEBIT',  10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000003'
      AND account_id     = '00000000-0000-0000-0000-000000000001'
      AND direction      = 'DEBIT'
);

INSERT INTO entries (transaction_id, account_id, direction, amount)
SELECT '01900000-0000-7000-8000-000000000003', '00000000-0000-0000-0000-000000000004', 'CREDIT', 10000.0000
WHERE NOT EXISTS (
    SELECT 1 FROM entries
    WHERE transaction_id = '01900000-0000-7000-8000-000000000003'
      AND account_id     = '00000000-0000-0000-0000-000000000004'
      AND direction      = 'CREDIT'
);
