-- Relational tree: the append-only audit log (docs/PLAN.md §5.1, §8;
-- spec 06 T7).
--
-- Every write, token issuance and refusal, source switch, policy change
-- and PII-bearing export is an events row with actor and purpose,
-- written by internal/store Audit inside the caller's transaction (the
-- only writer). ussp_app may SELECT and INSERT on the parent and nothing
-- else; it has no grant at all on the partitions, so an UPDATE or DELETE
-- addressed to a partition directly is refused as well.
--
-- Partitioned by month on ts (UTC). events_ensure_partition creates the
-- month's partition on demand; it is SECURITY DEFINER (owned by
-- ussp_api) so ussp_app, which may not create tables, can call it, and
-- it grants nothing on what it creates.

-- +goose Up
CREATE TABLE events (
    id          bigserial   NOT NULL,
    ts          timestamptz NOT NULL DEFAULT now(),
    actor_type  text        NOT NULL CHECK (actor_type <> ''),
    actor_id    text        NOT NULL CHECK (actor_id <> ''),
    purpose     text,
    entity_type text        NOT NULL CHECK (entity_type <> ''),
    entity_id   text,
    event_type  text        NOT NULL CHECK (event_type <> ''),
    payload     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);
CREATE INDEX events_entity_idx ON events (entity_type, entity_id, ts);
CREATE INDEX events_actor_idx ON events (actor_id, ts);

-- +goose StatementBegin
CREATE FUNCTION events_ensure_partition(at timestamptz) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    month_start timestamptz := date_trunc('month', at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    month_end   timestamptz := (date_trunc('month', at AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC';
    part        text        := 'events_' || to_char(at AT TIME ZONE 'UTC', 'YYYY_MM');
BEGIN
    IF to_regclass(part) IS NULL THEN
        -- Two writers in the same new month: the second waits here and
        -- then finds the partition.
        PERFORM pg_advisory_xact_lock(hashtext('events_ensure_partition'), hashtext(part));
        IF to_regclass(part) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF events FOR VALUES FROM (%L) TO (%L)',
                           part, month_start, month_end);
        END IF;
    END IF;
    RETURN part;
END
$$;
-- +goose StatementEnd

SELECT events_ensure_partition(now());

REVOKE ALL ON FUNCTION events_ensure_partition(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION events_ensure_partition(timestamptz) TO ussp_app;
GRANT SELECT, INSERT ON events TO ussp_app;
GRANT USAGE ON SEQUENCE events_id_seq TO ussp_app;

-- +goose Down
DROP TABLE events;
DROP FUNCTION events_ensure_partition(timestamptz);
