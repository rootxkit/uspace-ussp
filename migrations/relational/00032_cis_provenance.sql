-- Relational tree: the kind of evidence each stored CIS version was
-- installed on (WP-4, internal/cis provenance.go; spec 02 F3 erratum
-- 2026-10-16).
--
-- provenance is 'publisher' for a version whose publisher's signature
-- verified, 'cisp_expiry' for a restrictions version the CISP made when
-- a restriction reached its end and that passed the expiry rule, and
-- NULL for a version held untrusted. signature_ok stays the trusted
-- flag the pruning and the warm start read: it is true exactly when a
-- kind is recorded, which the CHECK holds.
--
-- Every existing trusted row was installed on its publisher's
-- signature, the only kind there was, and is backfilled so. cis_datasets
-- is live, so its CHECK is added NOT VALID (no scan under the ALTER's
-- lock; new and updated rows are checked) and validated by 00033.

-- +goose Up
ALTER TABLE cis_datasets ADD COLUMN provenance text;
UPDATE cis_datasets SET provenance = 'publisher' WHERE signature_ok;
-- The provenance IS NOT NULL is not redundant: a NULL IN (...) is NULL,
-- and a CHECK that evaluates to NULL passes.
ALTER TABLE cis_datasets ADD CONSTRAINT cis_datasets_provenance_check CHECK (
    (signature_ok AND provenance IS NOT NULL AND provenance IN ('publisher', 'cisp_expiry'))
    OR (NOT signature_ok AND provenance IS NULL)) NOT VALID;
COMMENT ON COLUMN cis_datasets.provenance IS
    'publisher or cisp_expiry: the evidence the version was installed on; NULL while held untrusted';

-- +goose Down
-- A cisp_expiry row stays trusted (signature_ok) after the column is
-- gone: the code before this migration loads it like a publisher one.
ALTER TABLE cis_datasets DROP CONSTRAINT cis_datasets_provenance_check;
ALTER TABLE cis_datasets DROP COLUMN provenance;
