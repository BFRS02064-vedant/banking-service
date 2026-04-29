-- accounts holds all ledger accounts, including the EXTERNAL system account.
CREATE TABLE accounts (
    id         UUID          PRIMARY KEY,
    name       TEXT          NOT NULL,
    balance    NUMERIC(20,4) NOT NULL DEFAULT 0,
    -- System accounts (e.g. EXTERNAL) are permitted to carry negative balances
    -- because they represent the aggregate liability to all user accounts.
    -- This is the canonical double-entry liability semantic.
    is_system  BOOLEAN       NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT non_negative_user_balance CHECK (is_system OR balance >= 0)
);

-- transactions is the header row for each double-entry event.
-- WITHDRAWAL is included in the kind CHECK as a reserved value for M4; no
-- service method or handler exists for it yet.
CREATE TABLE transactions (
    id                      UUID        PRIMARY KEY,
    kind                    TEXT        NOT NULL CHECK (kind IN ('TRANSFER','REVERSAL','DEPOSIT','WITHDRAWAL')),
    idempotency_key         TEXT        NOT NULL UNIQUE,
    reverses_transaction_id UUID        UNIQUE REFERENCES transactions(id),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- entries are the individual debit/credit legs of each transaction.
-- amount must be strictly positive; sign is captured by direction.
CREATE TABLE entries (
    id             BIGSERIAL     PRIMARY KEY,
    transaction_id UUID          NOT NULL REFERENCES transactions(id),
    account_id     UUID          NOT NULL REFERENCES accounts(id),
    direction      TEXT          NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount         NUMERIC(20,4) NOT NULL CHECK (amount > 0),
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now()
);

CREATE INDEX idx_entries_account     ON entries(account_id);
CREATE INDEX idx_entries_transaction ON entries(transaction_id);

-- audit_log stores domain-level operation attempts (both successes and failures).
-- from_account/to_account have no FK intentionally: failed lookups must still log.
CREATE TABLE audit_log (
    id             BIGSERIAL     PRIMARY KEY,
    operation      TEXT          NOT NULL,
    from_account   UUID,
    to_account     UUID,
    amount         NUMERIC(20,4),
    outcome        TEXT          NOT NULL CHECK (outcome IN ('SUCCESS','FAILURE')),
    error_reason   TEXT,
    transaction_id UUID,
    request_id     TEXT,
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_from ON audit_log(from_account);
CREATE INDEX idx_audit_to   ON audit_log(to_account);
CREATE INDEX idx_audit_time ON audit_log(created_at DESC);

-- check_double_entry_balance is a deferred constraint trigger that asserts the
-- double-entry invariant at COMMIT time: for every transaction, the sum of
-- CREDIT amounts must equal the sum of DEBIT amounts.
-- Only CONSTRAINT TRIGGERs can be DEFERRABLE; a regular CREATE TRIGGER cannot.
CREATE OR REPLACE FUNCTION check_double_entry_balance()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_sum NUMERIC;
BEGIN
    SELECT COALESCE(SUM(amount * CASE direction WHEN 'CREDIT' THEN 1 ELSE -1 END), 0)
    INTO v_sum
    FROM entries
    WHERE transaction_id = NEW.transaction_id;

    IF v_sum <> 0 THEN
        RAISE EXCEPTION 'double-entry invariant violated for transaction %: net sum is %',
            NEW.transaction_id, v_sum;
    END IF;

    RETURN NEW;
END;
$$;

-- trig_check_double_entry fires AFTER INSERT OR UPDATE on each entry row, but is
-- DEFERRABLE INITIALLY DEFERRED so the check runs at COMMIT rather than per-row.
-- This allows both legs of a transaction to be inserted before the invariant is
-- evaluated, which is the correct double-entry semantics.
CREATE CONSTRAINT TRIGGER trig_check_double_entry
    AFTER INSERT OR UPDATE ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_double_entry_balance();
