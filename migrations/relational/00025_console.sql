-- Relational tree: the USSP console of WP-18 (internal/admin; spec 01
-- §3 S11, 02 F5, 07 S-M5).
--
-- alerts gains the console's own facts, each written by api on the
-- database clock with an events row in the same transaction: a
-- supervisor's escalation (escalated_by, escalation_reason; the
-- automatic escalation of WP-11 leaves them NULL), the console's close
-- (closed_at, closed_by, close_reason: the supervisor handled it; a close
-- never clears the alert, which the monitor alone clears), and
-- messages_recorded, how many alert/v1 messages of the alert api
-- recorded (the monitor republishes an active one every second). The
-- indexes of the console's reads of alerts are 00026's (alerts is live:
-- built concurrently, outside a transaction). alerts is live, so its
-- three CHECKs are added NOT VALID: new and updated rows are checked at
-- once and the ALTER scans nothing under its ACCESS EXCLUSIVE lock; the
-- existing rows are validated by 00027, a separate step that holds only
-- a SHARE UPDATE EXCLUSIVE lock.
--
-- emergency_cases and emergency_notes are the emergency workflow: a
-- communication checklist with timestamps for one flight (nothing in it
-- reaches an aircraft). One case per flight is open at a time. The
-- emergency contact is the intent's reference, resolved by the
-- authority: no PII is copied here (CLAUDE.md rule 8). Notes are
-- append-only for ussp_app.
--
-- staff_mfa_challenges holds the second step of a staff admin's
-- sign-in: the SHA-256 of an opaque challenge, its account, its expiry
-- on the database clock and the codes tried against it. One live
-- challenge per account; expired rows are swept with the sessions.

-- +goose Up
ALTER TABLE alerts
    ADD COLUMN escalated_by      text,
    ADD COLUMN escalation_reason text,
    ADD COLUMN closed_at         timestamptz,
    ADD COLUMN closed_by         text,
    ADD COLUMN close_reason      text,
    ADD COLUMN messages_recorded bigint NOT NULL DEFAULT 1,
    ADD CONSTRAINT alerts_messages_recorded_check CHECK (messages_recorded >= 1) NOT VALID,
    ADD CONSTRAINT alerts_escalated_by_check CHECK (escalated_by IS NULL OR (escalated_at IS NOT NULL AND escalation_reason IS NOT NULL)) NOT VALID,
    ADD CONSTRAINT alerts_closed_check CHECK ((closed_at IS NULL) = (closed_by IS NULL) AND (closed_at IS NULL) = (close_reason IS NULL)) NOT VALID;

CREATE TABLE emergency_cases (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    flight_id  uuid        NOT NULL REFERENCES flights (id),
    opened_at  timestamptz NOT NULL DEFAULT now(),
    opened_by  text        NOT NULL CHECK (opened_by <> ''),
    reason     text        NOT NULL CHECK (reason <> ''),
    closed_at  timestamptz,
    closed_by  text,
    outcome    text,
    CHECK ((closed_at IS NULL) = (closed_by IS NULL) AND (closed_at IS NULL) = (outcome IS NULL)),
    CHECK (closed_at IS NULL OR closed_at >= opened_at)
);
CREATE UNIQUE INDEX emergency_cases_open_idx ON emergency_cases (flight_id) WHERE closed_at IS NULL;
CREATE INDEX emergency_cases_flight_idx ON emergency_cases (flight_id, opened_at);

CREATE TABLE emergency_notes (
    id      bigserial   PRIMARY KEY,
    case_id uuid        NOT NULL REFERENCES emergency_cases (id),
    at      timestamptz NOT NULL DEFAULT now(),
    author  text        NOT NULL CHECK (author <> ''),
    step    text        CHECK (step <> ''),
    text    text        NOT NULL CHECK (text <> '')
);
CREATE INDEX emergency_notes_case_idx ON emergency_notes (case_id, at);

CREATE TABLE staff_mfa_challenges (
    token_hash bytea       PRIMARY KEY CHECK (length(token_hash) = 32),
    account_id uuid        NOT NULL UNIQUE REFERENCES staff_accounts (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    attempts   integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    remote_ip  text,
    CHECK (expires_at > created_at)
);
CREATE INDEX staff_mfa_challenges_expiry_idx ON staff_mfa_challenges (expires_at);

GRANT SELECT, INSERT, UPDATE ON emergency_cases TO ussp_app;
GRANT SELECT, INSERT ON emergency_notes TO ussp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON staff_mfa_challenges TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE emergency_notes_id_seq TO ussp_app;

-- +goose Down
DROP TABLE staff_mfa_challenges;
DROP TABLE emergency_notes;
DROP TABLE emergency_cases;
ALTER TABLE alerts
    DROP CONSTRAINT alerts_closed_check,
    DROP CONSTRAINT alerts_escalated_by_check,
    DROP CONSTRAINT alerts_messages_recorded_check,
    DROP COLUMN messages_recorded,
    DROP COLUMN close_reason,
    DROP COLUMN closed_by,
    DROP COLUMN closed_at,
    DROP COLUMN escalation_reason,
    DROP COLUMN escalated_by;
