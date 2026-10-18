-- Test-only/destructive reversal of exact SessionPage replay evidence.
-- Production migration 0008 is forward-only and must never be rolled back in
-- place. This file supports disposable full teardown (8 -> 6 -> 8) proofs.

DROP TRIGGER IF EXISTS study_flush_receipts_require_session_page
    ON study_flush_receipts;
DROP TRIGGER IF EXISTS study_flush_receipts_require_page_marker_on_insert
    ON study_flush_receipts;
DROP TRIGGER IF EXISTS study_flush_receipt_session_pages_append_only
    ON study_flush_receipt_session_pages;
DROP TABLE IF EXISTS study_flush_receipt_session_pages;
DROP FUNCTION IF EXISTS study_validate_required_flush_session_page();
DROP FUNCTION IF EXISTS study_reject_new_flush_without_session_page_requirement();

ALTER TABLE study_flush_receipts
    DROP COLUMN IF EXISTS session_page_artifact_required;
