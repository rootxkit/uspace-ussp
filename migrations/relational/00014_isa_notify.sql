-- Relational tree: subscriber notifications of F3411 ISA writes are
-- their own outbox kind (WP-9 review). The ISA worker queued nothing
-- for them and posted them inline, so a subscriber that did not answer
-- held every ISA put and delete behind it; now the ISA write records
-- what the DSS holds and queues one isa_notify item per subscriber in
-- the same transaction, and a separate loop with a total time budget
-- posts them.

-- +goose Up
ALTER TABLE dss_outbox DROP CONSTRAINT dss_outbox_kind_check;
ALTER TABLE dss_outbox ADD CONSTRAINT dss_outbox_kind_check
    CHECK (kind IN ('oir_put', 'oir_delete', 'isa_put', 'isa_delete', 'isa_notify', 'peer_notify', 'uss_report'));

-- +goose Down
DELETE FROM dss_outbox WHERE kind = 'isa_notify';
ALTER TABLE dss_outbox DROP CONSTRAINT dss_outbox_kind_check;
ALTER TABLE dss_outbox ADD CONSTRAINT dss_outbox_kind_check
    CHECK (kind IN ('oir_put', 'oir_delete', 'isa_put', 'isa_delete', 'peer_notify', 'uss_report'));
