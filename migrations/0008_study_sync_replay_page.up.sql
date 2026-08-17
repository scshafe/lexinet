-- Adds exact immutable SessionPage replay evidence to every new study flush.
--
-- Why: study.v1 promises that an identical sync_id replays the original page
-- bytes even after live source links, names, or lifecycle state change.
-- Depends on: 0007 study_flush_receipts and study_reject_append_only_mutation.
-- Deployment: forward-only additive migration; legacy receipts remain readable
-- only as explicit missing-evidence failures. Production never rolls 0008 back
-- in place. The down file exists solely for disposable full-teardown proofs.

ALTER TABLE study_flush_receipts
    ADD COLUMN session_page_artifact_required BOOLEAN NOT NULL DEFAULT FALSE;

-- PostgreSQL materializes FALSE for every pre-0008 row above. Switch the
-- insert default only after that legacy backfill so raw and future writers
-- cannot silently create a receipt without exact replay evidence.
ALTER TABLE study_flush_receipts
    ALTER COLUMN session_page_artifact_required SET DEFAULT TRUE;

COMMENT ON COLUMN study_flush_receipts.session_page_artifact_required IS 'False only for rows that predate migration 0008; every future flush defaults true and must commit one exact SessionPage artifact.';

CREATE FUNCTION study_reject_new_flush_without_session_page_requirement()
RETURNS trigger AS $$
BEGIN
    IF NOT NEW.session_page_artifact_required THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'new study flush receipt must require exact SessionPage evidence';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_flush_receipts_require_page_marker_on_insert
    BEFORE INSERT ON study_flush_receipts
    FOR EACH ROW EXECUTE FUNCTION study_reject_new_flush_without_session_page_requirement();

CREATE TABLE study_flush_receipt_session_pages (
    study_flush_receipt_id UUID PRIMARY KEY
        REFERENCES study_flush_receipts(study_flush_receipt_id) ON DELETE RESTRICT,
    page_after_position INTEGER NOT NULL,
    page_limit INTEGER NOT NULL,
    canonical_session_page TEXT NOT NULL,
    canonical_session_page_sha256 TEXT NOT NULL,
    session_page_artifact_created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_flush_receipt_session_pages_after_position_bounded CHECK (
        page_after_position BETWEEN -1 AND 2147483647
    ),
    CONSTRAINT study_flush_receipt_session_pages_limit_bounded CHECK (
        page_limit BETWEEN 1 AND 64
    ),
    CONSTRAINT study_flush_receipt_session_pages_json_bounded CHECK (
        octet_length(canonical_session_page) BETWEEN 1 AND 983040
    ),
    CONSTRAINT study_flush_receipt_session_pages_json_is_object CHECK (
        jsonb_typeof(canonical_session_page::jsonb) = 'object'
    ),
    CONSTRAINT study_flush_receipt_session_pages_sha256_format CHECK (
        canonical_session_page_sha256 ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT study_flush_receipt_session_pages_sha256_matches CHECK (
        canonical_session_page_sha256 = encode(
            digest(convert_to(canonical_session_page, 'UTF8'), 'sha256'),
            'hex'
        )
    )
);

COMMENT ON TABLE study_flush_receipt_session_pages IS 'One immutable exact deterministic typed-JSON SessionPage artifact for each post-0008 study sync flush receipt.';
COMMENT ON COLUMN study_flush_receipt_session_pages.study_flush_receipt_id IS 'Flush receipt whose idempotent replay owns these exact page bytes.';
COMMENT ON COLUMN study_flush_receipt_session_pages.page_after_position IS 'Original bounded page cursor requested by the sync.';
COMMENT ON COLUMN study_flush_receipt_session_pages.page_limit IS 'Original bounded maximum item count requested by the sync.';
COMMENT ON COLUMN study_flush_receipt_session_pages.canonical_session_page IS 'Exact deterministic typed encoding/json SessionPage text returned for the original sync and every identical replay.';
COMMENT ON COLUMN study_flush_receipt_session_pages.canonical_session_page_sha256 IS 'SHA-256 of the exact UTF-8 canonical_session_page bytes.';
COMMENT ON COLUMN study_flush_receipt_session_pages.session_page_artifact_created_at IS 'UTC time the immutable page replay artifact was committed with its flush receipt.';

CREATE TRIGGER study_flush_receipt_session_pages_append_only
    BEFORE UPDATE OR DELETE ON study_flush_receipt_session_pages
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE FUNCTION study_validate_required_flush_session_page()
RETURNS trigger AS $$
BEGIN
    IF NEW.session_page_artifact_required AND NOT EXISTS (
        SELECT 1
        FROM study_flush_receipt_session_pages page
        WHERE page.study_flush_receipt_id = NEW.study_flush_receipt_id
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'study flush receipt requires one immutable SessionPage artifact';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER study_flush_receipts_require_session_page
    AFTER INSERT OR UPDATE OF session_page_artifact_required ON study_flush_receipts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION study_validate_required_flush_session_page();
