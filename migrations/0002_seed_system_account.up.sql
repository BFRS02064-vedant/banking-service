-- Seed the EXTERNAL system account used as the source for deposits.
-- is_system = TRUE allows this account to carry a negative balance.
-- ON CONFLICT DO NOTHING makes this migration idempotent.
INSERT INTO accounts (id, name, is_system, balance)
VALUES ('00000000-0000-0000-0000-000000000001', 'EXTERNAL', TRUE, 0)
ON CONFLICT DO NOTHING;
