-- Relational tree: the CIS change-notification receiver of WP-4
-- (docs/PLAN.md §5.1, §6.1 POST /v1/cis/notifications; brief WP-4).
--
-- cis_notification_jtis: the delivery ids (jti) of the verified
-- notifications, per issuer, kept until expires_at (set from the
-- database clock: longer than the 5 min iat window plus the skew, so a
-- replayed delivery is caught on every replica for as long as its
-- signature could still verify). The receiver refuses a new row while
-- the live rows reach its bound (E-10) and sweeps the expired ones.
--
-- cis_notifications gains who sent it (issuer, the subscription it
-- names, its delivery id and change id), so the webhook log says which
-- path a change came by (the CISP, or the ANSP's degraded direct path).

-- +goose Up
CREATE TABLE cis_notification_jtis (
    issuer      text        NOT NULL,
    jti         text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    PRIMARY KEY (issuer, jti),
    CHECK (expires_at > received_at)
);
CREATE INDEX cis_notification_jtis_expires_idx ON cis_notification_jtis (expires_at);

ALTER TABLE cis_notifications
    ADD COLUMN issuer       text,
    ADD COLUMN jti          text,
    ADD COLUMN subscription text,
    ADD COLUMN msg_id       text;
CREATE INDEX cis_notifications_pending_idx ON cis_notifications (dataset) WHERE pulled_at IS NULL;

GRANT SELECT, INSERT, DELETE ON cis_notification_jtis TO ussp_app;

-- +goose Down
DROP INDEX cis_notifications_pending_idx;
ALTER TABLE cis_notifications
    DROP COLUMN msg_id,
    DROP COLUMN subscription,
    DROP COLUMN jti,
    DROP COLUMN issuer;
DROP TABLE cis_notification_jtis;
