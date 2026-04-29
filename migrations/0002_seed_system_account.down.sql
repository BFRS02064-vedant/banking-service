-- Dev-only down migration. FK-safe order: entries first, then transactions,
-- then the account row itself. The subquery covers transactions that the
-- EXTERNAL account participated in as either payer or payee.
DELETE FROM entries
WHERE transaction_id IN (
    SELECT DISTINCT transaction_id FROM entries
    WHERE account_id = '00000000-0000-0000-0000-000000000001'
);

DELETE FROM transactions
WHERE id IN (
    SELECT DISTINCT transaction_id FROM entries
    WHERE account_id = '00000000-0000-0000-0000-000000000001'
);

DELETE FROM accounts WHERE id = '00000000-0000-0000-0000-000000000001';
