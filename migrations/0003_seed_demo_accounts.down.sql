-- FK-safe order: entries first, then transactions, then accounts.
DELETE FROM entries
WHERE transaction_id IN (
    '01900000-0000-7000-8000-000000000001',
    '01900000-0000-7000-8000-000000000002',
    '01900000-0000-7000-8000-000000000003'
);

DELETE FROM transactions
WHERE idempotency_key IN (
    'seed-deposit-alice-001',
    'seed-deposit-bob-001',
    'seed-deposit-charlie-001'
);

DELETE FROM accounts
WHERE id IN (
    '00000000-0000-0000-0000-000000000002',
    '00000000-0000-0000-0000-000000000003',
    '00000000-0000-0000-0000-000000000004'
);

-- Restore EXTERNAL balance to 0 now that the demo deposits are removed.
UPDATE accounts SET balance = 0, updated_at = now()
WHERE id = '00000000-0000-0000-0000-000000000001';
