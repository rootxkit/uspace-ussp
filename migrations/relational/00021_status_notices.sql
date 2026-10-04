-- Relational tree: this USSP's operating-status notices to the authority
-- (WP-15; Reg. (EU) 2021/664 Art. 7(6), spec 02 F7): start (once, when
-- the operator confirms it on the console with a certificate id
-- configured), cease and restart. 00005 created the table; nothing has
-- written it.
--
-- reference is this USSP's reference of the notice, sent with it: the
-- authority answers a retry with the same reference and state with the
-- notice it recorded first (200), so a notice is recorded once however
-- often it is sent. A start is stored once per certificate (a second
-- confirmation answers the first); a cease follows a start or a restart,
-- a restart follows a cease. state: pending (next_at the next try),
-- delivered (submitted_at, authority_ref the authority's notice id),
-- failed (refused for good, last_error says why).

-- +goose Up
ALTER TABLE operating_status_notices
    ADD COLUMN certificate_id text        NOT NULL CHECK (certificate_id ~ '^[0-9a-f]{32}$'),
    ADD COLUMN reference      text        NOT NULL UNIQUE CHECK (length(reference) BETWEEN 1 AND 200),
    ADD COLUMN requested_by   text        NOT NULL,
    ADD COLUMN state          text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'failed')),
    ADD COLUMN attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_at        timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_error     text,
    ADD COLUMN failed_at      timestamptz,
    ADD COLUMN created_at     timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT operating_status_notices_delivered_check CHECK ((state = 'delivered') = (submitted_at IS NOT NULL)),
    ADD CONSTRAINT operating_status_notices_failed_check CHECK ((state = 'failed') = (failed_at IS NOT NULL));
CREATE UNIQUE INDEX operating_status_notices_one_start_idx ON operating_status_notices (certificate_id)
    WHERE kind = 'start' AND state <> 'failed';
CREATE INDEX operating_status_notices_due_idx ON operating_status_notices (next_at) WHERE state = 'pending';
CREATE INDEX operating_status_notices_cert_idx ON operating_status_notices (certificate_id, created_at);

-- +goose Down
DROP INDEX operating_status_notices_cert_idx;
DROP INDEX operating_status_notices_due_idx;
DROP INDEX operating_status_notices_one_start_idx;
ALTER TABLE operating_status_notices
    DROP CONSTRAINT operating_status_notices_failed_check,
    DROP CONSTRAINT operating_status_notices_delivered_check,
    DROP COLUMN created_at,
    DROP COLUMN failed_at,
    DROP COLUMN last_error,
    DROP COLUMN next_at,
    DROP COLUMN attempts,
    DROP COLUMN state,
    DROP COLUMN requested_by,
    DROP COLUMN reference,
    DROP COLUMN certificate_id;
