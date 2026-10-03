-- Relational tree: the alerts record of WP-11 (internal/alerts). api
-- records every alert/v1 of alrt.v1 by its alert_id (a UUID derived from
-- the condition and its raise time, so a republish, a restart and a
-- handover are one row). cell5 is where the flight was at the last
-- message, for the subject an escalation or an acknowledgement is
-- republished on (internal: the cell never leaves this system).
-- recorded_at is the database clock when the row was first written:
-- escalation counts from the earlier of it and raised_at, so a monitor
-- clock ahead never delays an escalation. delivery holds, per client,
-- when the alert was first sent to it and when it acknowledged it.

-- +goose Up
ALTER TABLE alerts
    ADD COLUMN cell5       text,
    ADD COLUMN recorded_at timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT alerts_state_check CHECK (state IN ('raised', 'updated', 'cleared')),
    ADD CONSTRAINT alerts_severity_check CHECK (severity IN ('info', 'warning', 'critical')),
    ADD CONSTRAINT alerts_cleared_state_check CHECK ((state = 'cleared') = (cleared_at IS NOT NULL));
CREATE INDEX alerts_escalation_idx ON alerts (raised_at)
    WHERE severity = 'critical' AND cleared_at IS NULL AND acked_at IS NULL AND escalated_at IS NULL;

-- +goose Down
DROP INDEX alerts_escalation_idx;
ALTER TABLE alerts
    DROP CONSTRAINT alerts_cleared_state_check,
    DROP CONSTRAINT alerts_severity_check,
    DROP CONSTRAINT alerts_state_check,
    DROP COLUMN recorded_at,
    DROP COLUMN cell5;
