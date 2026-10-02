-- Relational tree: what flight authorisation (WP-7, internal/intent)
-- needs of operational_intents beyond docs/PLAN.md §5.1.
--
-- cis_version_checked becomes text: the CIS cache's version is one label
-- per ED-318 dataset ("zones:5,uspace_airspace:3,restrictions:7"), not
-- one number. version counts the intent's versions (intent_versions
-- keeps each one). request is intent/request/v1 as received (canonical
-- JSON), request_hash its SHA-256 for the (client_id, client_ref)
-- idempotency (the same body answers the same decision, another one is
-- refused), decision_body the intent/decision/v1 as it stands. filed_at
-- is when the volumes judged were filed: first come, first served ranks
-- on it (Art. 10(9)), on the database clock. cell_set is the intent's
-- cells in intent_active. subcategory, privately_built and mtom_kg
-- complete Annex IV item 4 (the Art. 1(3) exemption reads them).
-- update_required is set when a later intent with precedence (a special
-- operation) overlaps this authorisation: WP-12 updates or withdraws it
-- (Art. 10(10)).

-- +goose Up
ALTER TABLE operational_intents
    ALTER COLUMN cis_version_checked TYPE text USING cis_version_checked::text,
    ADD COLUMN version         integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    ADD COLUMN request         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN request_hash    text,
    ADD COLUMN decision_body   jsonb,
    ADD COLUMN filed_at        timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN cell_set        text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN subcategory     text,
    ADD COLUMN privately_built boolean     NOT NULL DEFAULT false,
    ADD COLUMN mtom_kg         double precision CHECK (mtom_kg > 0),
    ADD COLUMN update_required jsonb;

-- The intents a decision is deconflicted against: active and not
-- exempt, by window (the envelope has its gist index already).
CREATE INDEX operational_intents_active_idx ON operational_intents (time_start, time_end)
    WHERE local_state IN ('accepted', 'activated', 'nonconforming', 'contingent') AND NOT exempt_art_1_3;
-- The open intents of an operator (its bound) and the ones past
-- time_end (the sweep).
CREATE INDEX operational_intents_open_idx ON operational_intents (operator_id)
    WHERE local_state IN ('pending_validation', 'pending_dss', 'pending_authority', 'accepted', 'activated', 'nonconforming', 'contingent');
CREATE INDEX operational_intents_due_idx ON operational_intents (time_end)
    WHERE local_state IN ('pending_validation', 'pending_dss', 'pending_authority', 'accepted', 'activated', 'nonconforming', 'contingent');

-- +goose Down
DROP INDEX operational_intents_due_idx;
DROP INDEX operational_intents_open_idx;
DROP INDEX operational_intents_active_idx;
ALTER TABLE operational_intents
    DROP COLUMN update_required,
    DROP COLUMN mtom_kg,
    DROP COLUMN privately_built,
    DROP COLUMN subcategory,
    DROP COLUMN cell_set,
    DROP COLUMN filed_at,
    DROP COLUMN decision_body,
    DROP COLUMN request_hash,
    DROP COLUMN request,
    DROP COLUMN version,
    ALTER COLUMN cis_version_checked TYPE bigint USING NULL;
