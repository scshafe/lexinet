-- Reverses the additive study persistence model from 0007_study.up.sql.
-- Historical study evidence is removed; take a backup before rollback outside
-- disposable development databases.

DROP TRIGGER IF EXISTS study_scheduler_card_projections_set_updated_at ON study_scheduler_card_projections;
DROP TRIGGER IF EXISTS study_scheduler_card_projections_validate_redirects ON study_scheduler_card_projections;
DROP TABLE IF EXISTS study_scheduler_card_projections;
DROP FUNCTION IF EXISTS study_validate_projection_identity_redirects();

DROP TRIGGER IF EXISTS study_card_identity_redirects_append_only ON study_card_identity_redirects;
DROP TRIGGER IF EXISTS study_card_identity_redirects_reject_cycle ON study_card_identity_redirects;
DROP TABLE IF EXISTS study_card_identity_redirects;
DROP FUNCTION IF EXISTS study_reject_card_identity_redirect_cycle();

DROP TRIGGER IF EXISTS study_flush_receipts_append_only ON study_flush_receipts;
DROP TABLE IF EXISTS study_flush_receipts;

DROP TRIGGER IF EXISTS study_event_receipts_append_only ON study_event_receipts;
DROP TRIGGER IF EXISTS study_event_receipts_validate_binding ON study_event_receipts;
DROP TABLE IF EXISTS study_event_receipts;
DROP FUNCTION IF EXISTS study_validate_event_receipt_binding();

DROP TRIGGER IF EXISTS study_events_append_only ON study_events;
DROP TRIGGER IF EXISTS study_events_validate_rating_void_target ON study_events;
DROP TABLE IF EXISTS study_events;
DROP FUNCTION IF EXISTS study_validate_rating_void_target();

DROP TRIGGER IF EXISTS study_event_id_bindings_append_only ON study_event_id_bindings;
DROP TABLE IF EXISTS study_event_id_bindings;

DROP TRIGGER IF EXISTS study_client_streams_set_updated_at ON study_client_streams;
DROP TABLE IF EXISTS study_client_streams;

DROP TRIGGER IF EXISTS study_session_items_reject_delete ON study_session_items;
DROP TRIGGER IF EXISTS study_session_items_guard_update ON study_session_items;
DROP TABLE IF EXISTS study_session_items;
DROP FUNCTION IF EXISTS study_guard_session_item_update();

DROP TRIGGER IF EXISTS study_session_creation_receipts_append_only ON study_session_creation_receipts;
DROP TABLE IF EXISTS study_session_creation_receipts;

DROP TRIGGER IF EXISTS study_session_lifecycle_events_append_only ON study_session_lifecycle_events;
DROP TABLE IF EXISTS study_session_lifecycle_events;

DROP TRIGGER IF EXISTS study_sessions_set_updated_at ON study_sessions;
DROP TABLE IF EXISTS study_sessions;

DROP TRIGGER IF EXISTS study_learner_profiles_set_updated_at ON study_learner_profiles;
DROP TABLE IF EXISTS study_learner_profiles;

DROP FUNCTION IF EXISTS study_reject_append_only_mutation();
DROP FUNCTION IF EXISTS study_topic_refs_are_valid(TEXT[]);
DROP FUNCTION IF EXISTS study_text_array_is_strictly_sorted_unique(TEXT[]);
DROP FUNCTION IF EXISTS study_uuid_array_is_strictly_sorted_unique(UUID[]);
