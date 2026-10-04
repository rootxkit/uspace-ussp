-- Relational tree: occurrence reports to the authority (WP-15; spec 02
-- F7, 04 §3.3 occurrence/v1; Reg. (EU) 376/2014 Art. 4(8): within 72 h
-- of awareness). 00005 created the table; nothing has written it.
--
-- source_kind and source_ref name what the report is about (an alert by
-- its id, or a flight), unique together: an alert is reported once,
-- whoever or whatever flags it first. flight_ids and intent_ids are the
-- flights and intents it names; a flight named here is held past its
-- retention (record_holds). reporter_ref is an opaque reference of the
-- person who flagged it (a staff account id) or "system", sent in clear
-- over TLS (reconciliation M13: the authority encrypts it at rest).
-- state: pending (not yet delivered; next_at is the next try), delivered
-- (submitted_at, authority_ref), failed (refused for good, or out of
-- tries). A report past deadline_at that is not delivered is a critical
-- console item. Times are the database clock.

-- +goose Up
ALTER TABLE occurrence_reports
    ADD COLUMN source_kind  text        NOT NULL CHECK (source_kind IN ('alert', 'flight')),
    ADD COLUMN source_ref   text        NOT NULL,
    ADD COLUMN channel      text        NOT NULL CHECK (channel IN ('mandatory', 'voluntary')),
    ADD COLUMN flagged_by   text        NOT NULL CHECK (flagged_by IN ('system', 'supervisor')),
    ADD COLUMN reporter_ref text        NOT NULL,
    ADD COLUMN flight_ids   uuid[]      NOT NULL DEFAULT '{}',
    ADD COLUMN intent_ids   uuid[]      NOT NULL DEFAULT '{}',
    ADD COLUMN state        text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'failed')),
    ADD COLUMN next_at      timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN failed_at    timestamptz,
    ADD COLUMN created_at   timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT occurrence_reports_kind_check
        CHECK (kind IN ('airprox', 'nonconformance_in_prohibited', 'lost_link_in_uspace', 'emergency', 'other')),
    ADD CONSTRAINT occurrence_reports_source_unique UNIQUE (source_kind, source_ref),
    ADD CONSTRAINT occurrence_reports_delivered_check CHECK ((state = 'delivered') = (submitted_at IS NOT NULL)),
    ADD CONSTRAINT occurrence_reports_failed_check CHECK ((state = 'failed') = (failed_at IS NOT NULL));
DROP INDEX occurrence_reports_undelivered_idx;
CREATE INDEX occurrence_reports_due_idx ON occurrence_reports (next_at) WHERE state = 'pending';
CREATE INDEX occurrence_reports_open_idx ON occurrence_reports (deadline_at) WHERE state <> 'delivered';
CREATE INDEX occurrence_reports_flights_idx ON occurrence_reports USING gin (flight_ids);
-- The automatic detection reads the recent alerts by kind.
CREATE INDEX alerts_kind_raised_idx ON alerts (kind, raised_at);

-- +goose Down
DROP INDEX alerts_kind_raised_idx;
DROP INDEX occurrence_reports_flights_idx;
DROP INDEX occurrence_reports_open_idx;
DROP INDEX occurrence_reports_due_idx;
CREATE INDEX occurrence_reports_undelivered_idx ON occurrence_reports (deadline_at) WHERE submitted_at IS NULL;
ALTER TABLE occurrence_reports
    DROP CONSTRAINT occurrence_reports_failed_check,
    DROP CONSTRAINT occurrence_reports_delivered_check,
    DROP CONSTRAINT occurrence_reports_source_unique,
    DROP CONSTRAINT occurrence_reports_kind_check,
    DROP COLUMN created_at,
    DROP COLUMN failed_at,
    DROP COLUMN next_at,
    DROP COLUMN state,
    DROP COLUMN intent_ids,
    DROP COLUMN flight_ids,
    DROP COLUMN reporter_ref,
    DROP COLUMN flagged_by,
    DROP COLUMN channel,
    DROP COLUMN source_ref,
    DROP COLUMN source_kind;
