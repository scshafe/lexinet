-- Adds the durable study-session evidence model.
--
-- Why: the study.v1 contract requires server-authoritative sessions, immutable
-- issued card snapshots, append-only learning evidence, terminal stream
-- receipts, and rebuildable scheduler projections.
-- Depends on: 0001 ideas/names and 0005 topics.
-- Deployment: additive expand migration; no maintenance window is required.

CREATE FUNCTION study_uuid_array_is_strictly_sorted_unique(input_values UUID[])
RETURNS BOOLEAN AS $$
    SELECT array_position(input_values, NULL) IS NULL
       AND coalesce(bool_and(previous_value IS NULL OR previous_value < current_value), true)
    FROM (
        SELECT
            value AS current_value,
            lag(value) OVER (ORDER BY ordinal) AS previous_value
        FROM unnest(input_values) WITH ORDINALITY AS entries(value, ordinal)
    ) ordered_values;
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION study_text_array_is_strictly_sorted_unique(input_values TEXT[])
RETURNS BOOLEAN AS $$
    SELECT array_position(input_values, NULL) IS NULL
       AND coalesce(bool_and(previous_value IS NULL OR previous_value < current_value), true)
    FROM (
        SELECT
            value AS current_value,
            lag(value) OVER (ORDER BY ordinal) AS previous_value
        FROM unnest(input_values) WITH ORDINALITY AS entries(value, ordinal)
    ) ordered_values;
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION study_topic_refs_are_valid(input_values TEXT[])
RETURNS BOOLEAN AS $$
    SELECT array_position(input_values, NULL) IS NULL
       AND coalesce(bool_and(
           char_length(value) BETWEEN 1 AND 128
           AND value ~ '^[A-Za-z0-9][A-Za-z0-9._:-]*$'
       ), true)
    FROM unnest(input_values) AS entries(value);
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION study_reject_append_only_mutation()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = format('%I is append-only', TG_TABLE_NAME);
END;
$$ LANGUAGE plpgsql;

CREATE TABLE study_learner_profiles (
    learner_profile_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    learner_profile_ref TEXT NOT NULL,
    profile_ingestion_high_water BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_learner_profiles_ref_not_blank CHECK (btrim(learner_profile_ref) <> ''),
    CONSTRAINT study_learner_profiles_ref_length CHECK (char_length(learner_profile_ref) <= 128),
    CONSTRAINT study_learner_profiles_ref_format CHECK (learner_profile_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]*$'),
    CONSTRAINT study_learner_profiles_high_water_nonnegative CHECK (profile_ingestion_high_water >= 0)
);

CREATE UNIQUE INDEX study_learner_profiles_ref_uidx
    ON study_learner_profiles (learner_profile_ref);

COMMENT ON TABLE study_learner_profiles IS 'Stable learner identities and the lock anchor for gap-free accepted-event ingestion ordinals.';
COMMENT ON COLUMN study_learner_profiles.learner_profile_id IS 'Server-generated learner identity; independent of appliance identity.';
COMMENT ON COLUMN study_learner_profiles.learner_profile_ref IS 'Stable externally configured learner reference from study.v1.';
COMMENT ON COLUMN study_learner_profiles.profile_ingestion_high_water IS 'Largest accepted event ordinal allocated while holding this profile row lock.';
COMMENT ON COLUMN study_learner_profiles.created_at IS 'UTC creation time.';
COMMENT ON COLUMN study_learner_profiles.updated_at IS 'UTC time of the latest mutable profile state change.';

CREATE TRIGGER study_learner_profiles_set_updated_at
    BEFORE UPDATE ON study_learner_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE study_sessions (
    study_session_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    learner_profile_id UUID NOT NULL REFERENCES study_learner_profiles(learner_profile_id) ON DELETE RESTRICT,
    normalized_corpus_selector TEXT NOT NULL,
    normalized_corpus_selector_sha256 TEXT NOT NULL,
    normalized_topic_ids UUID[] NOT NULL,
    normalized_topic_refs TEXT[] NOT NULL,
    study_card_kind TEXT NOT NULL,
    study_session_lifecycle TEXT NOT NULL,
    study_corpus_sha256 TEXT NOT NULL,
    study_scheduler_name TEXT,
    study_scheduler_version TEXT,
    study_scheduler_configuration_sha256 TEXT,
    study_scheduler_input_ingestion_high_water BIGINT,
    study_scheduler_planned_at TIMESTAMPTZ,
    study_scheduler_seed TEXT,
    predecessor_study_session_id UUID,
    study_session_state_version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    study_session_terminalized_at TIMESTAMPTZ,
    CONSTRAINT study_sessions_selector_is_object CHECK (jsonb_typeof(normalized_corpus_selector::jsonb) = 'object'),
    CONSTRAINT study_sessions_selector_bounded CHECK (octet_length(normalized_corpus_selector) <= 8192),
    CONSTRAINT study_sessions_selector_sha256_format CHECK (normalized_corpus_selector_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_sessions_selector_sha256_matches CHECK (
        normalized_corpus_selector_sha256 = encode(digest(convert_to(normalized_corpus_selector, 'UTF8'), 'sha256'), 'hex')
    ),
    CONSTRAINT study_sessions_topic_count CHECK (cardinality(normalized_topic_ids) BETWEEN 1 AND 32),
    CONSTRAINT study_sessions_topic_ref_count CHECK (cardinality(normalized_topic_refs) = cardinality(normalized_topic_ids)),
    CONSTRAINT study_sessions_topic_ids_normalized CHECK (study_uuid_array_is_strictly_sorted_unique(normalized_topic_ids)),
    CONSTRAINT study_sessions_topic_refs_normalized CHECK (study_text_array_is_strictly_sorted_unique(normalized_topic_refs)),
    CONSTRAINT study_sessions_topic_refs_valid CHECK (study_topic_refs_are_valid(normalized_topic_refs)),
    CONSTRAINT study_sessions_card_kind_valid CHECK (study_card_kind = 'idea'),
    CONSTRAINT study_sessions_lifecycle_valid CHECK (study_session_lifecycle IN ('active', 'completed', 'waiting_for_corpus', 'superseded_empty', 'abandoned')),
    CONSTRAINT study_sessions_corpus_sha256_format CHECK (study_corpus_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_sessions_scheduler_bundle_consistent CHECK (
        (
            study_scheduler_name IS NULL
            AND study_scheduler_version IS NULL
            AND study_scheduler_configuration_sha256 IS NULL
            AND study_scheduler_input_ingestion_high_water IS NULL
            AND study_scheduler_planned_at IS NULL
            AND study_scheduler_seed IS NULL
        ) OR (
            study_scheduler_name IS NOT NULL
            AND study_scheduler_version IS NOT NULL
            AND study_scheduler_configuration_sha256 IS NOT NULL
            AND study_scheduler_input_ingestion_high_water IS NOT NULL
            AND study_scheduler_planned_at IS NOT NULL
            AND study_scheduler_seed IS NOT NULL
            AND btrim(study_scheduler_name) <> ''
            AND char_length(study_scheduler_name) <= 128
            AND btrim(study_scheduler_version) <> ''
            AND char_length(study_scheduler_version) <= 64
            AND study_scheduler_configuration_sha256 ~ '^[0-9a-f]{64}$'
            AND study_scheduler_input_ingestion_high_water >= 0
            AND btrim(study_scheduler_seed) <> ''
            AND char_length(study_scheduler_seed) <= 128
        )
    ),
    CONSTRAINT study_sessions_active_has_scheduler CHECK (study_session_lifecycle <> 'active' OR study_scheduler_name IS NOT NULL),
    CONSTRAINT study_sessions_waiting_has_no_scheduler CHECK (study_session_lifecycle <> 'waiting_for_corpus' OR study_scheduler_name IS NULL),
    CONSTRAINT study_sessions_state_version_positive CHECK (study_session_state_version >= 1),
    CONSTRAINT study_sessions_predecessor_not_self CHECK (predecessor_study_session_id IS NULL OR predecessor_study_session_id <> study_session_id),
    CONSTRAINT study_sessions_terminal_time_consistent CHECK (
        (study_session_lifecycle IN ('active', 'waiting_for_corpus') AND study_session_terminalized_at IS NULL)
        OR (study_session_lifecycle IN ('completed', 'superseded_empty', 'abandoned') AND study_session_terminalized_at IS NOT NULL)
    ),
    CONSTRAINT study_sessions_id_profile_unique UNIQUE (study_session_id, learner_profile_id),
    CONSTRAINT study_sessions_scope_tuple_unique UNIQUE (
        study_session_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind
    ),
    CONSTRAINT study_sessions_predecessor_same_scope_fk FOREIGN KEY (
        predecessor_study_session_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind
    ) REFERENCES study_sessions (
        study_session_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind
    ) ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE
);

CREATE UNIQUE INDEX study_sessions_one_nonterminal_scope_uidx
    ON study_sessions (learner_profile_id, normalized_corpus_selector_sha256, normalized_corpus_selector, study_card_kind)
    WHERE study_session_lifecycle IN ('active', 'waiting_for_corpus');
CREATE UNIQUE INDEX study_sessions_one_successor_per_predecessor_uidx
    ON study_sessions (predecessor_study_session_id)
    WHERE predecessor_study_session_id IS NOT NULL;
CREATE INDEX study_sessions_profile_created_idx
    ON study_sessions (learner_profile_id, created_at, study_session_id);

COMMENT ON TABLE study_sessions IS 'Authoritative finite study cycles; active scope excludes scheduler identity so scheduler changes apply only to successors.';
COMMENT ON COLUMN study_sessions.study_session_id IS 'Server-generated authoritative session identity, never a client creation UUID.';
COMMENT ON COLUMN study_sessions.learner_profile_id IS 'Learner that owns the session and all of its evidence.';
COMMENT ON COLUMN study_sessions.normalized_corpus_selector IS 'Canonical JSON selector containing stable topic identities and card kind.';
COMMENT ON COLUMN study_sessions.normalized_corpus_selector_sha256 IS 'SHA-256 of canonical selector bytes; participates in active-scope uniqueness.';
COMMENT ON COLUMN study_sessions.normalized_topic_ids IS 'Strictly sorted unique stable topic identities used for eligibility queries.';
COMMENT ON COLUMN study_sessions.normalized_topic_refs IS 'Strictly sorted unique canonical topic references returned to clients.';
COMMENT ON COLUMN study_sessions.study_card_kind IS 'Card family; study.v1 currently permits only idea.';
COMMENT ON COLUMN study_sessions.study_session_lifecycle IS 'Explicit active, empty-wait, or audited terminal lifecycle.';
COMMENT ON COLUMN study_sessions.study_corpus_sha256 IS 'Digest of the eligible corpus observed when this cycle was materialized.';
COMMENT ON COLUMN study_sessions.study_scheduler_name IS 'Named scheduler authority for this immutable cycle.';
COMMENT ON COLUMN study_sessions.study_scheduler_version IS 'Version of the named scheduler authority.';
COMMENT ON COLUMN study_sessions.study_scheduler_configuration_sha256 IS 'Digest of canonical scheduler configuration bytes.';
COMMENT ON COLUMN study_sessions.study_scheduler_input_ingestion_high_water IS 'Exact accepted-event prefix used to plan the cycle.';
COMMENT ON COLUMN study_sessions.study_scheduler_planned_at IS 'Injected UTC planning time recorded as provenance, never a ranking key.';
COMMENT ON COLUMN study_sessions.study_scheduler_seed IS 'Injected deterministic scheduler seed.';
COMMENT ON COLUMN study_sessions.predecessor_study_session_id IS 'Unique predecessor; the reverse lookup is the authoritative successor link.';
COMMENT ON COLUMN study_sessions.study_session_state_version IS 'Optimistic compare-and-swap version for mutable session state.';
COMMENT ON COLUMN study_sessions.created_at IS 'UTC creation time.';
COMMENT ON COLUMN study_sessions.updated_at IS 'UTC time of the latest mutable state change.';
COMMENT ON COLUMN study_sessions.study_session_terminalized_at IS 'Audited UTC transition time for terminal lifecycle states.';

CREATE TRIGGER study_sessions_set_updated_at
    BEFORE UPDATE ON study_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE study_session_lifecycle_events (
    study_session_lifecycle_event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    study_session_id UUID NOT NULL REFERENCES study_sessions(study_session_id) ON DELETE RESTRICT,
    lifecycle_from_state TEXT,
    lifecycle_to_state TEXT NOT NULL,
    lifecycle_reason_code TEXT NOT NULL,
    lifecycle_actor_ref TEXT NOT NULL,
    lifecycle_state_version BIGINT NOT NULL,
    lifecycle_recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_session_lifecycle_events_from_valid CHECK (lifecycle_from_state IS NULL OR lifecycle_from_state IN ('active', 'completed', 'waiting_for_corpus', 'superseded_empty', 'abandoned')),
    CONSTRAINT study_session_lifecycle_events_to_valid CHECK (lifecycle_to_state IN ('active', 'completed', 'waiting_for_corpus', 'superseded_empty', 'abandoned')),
    CONSTRAINT study_session_lifecycle_events_reason_valid CHECK (
        char_length(lifecycle_reason_code) BETWEEN 1 AND 128 AND btrim(lifecycle_reason_code) <> ''
    ),
    CONSTRAINT study_session_lifecycle_events_actor_valid CHECK (
        lifecycle_actor_ref = 'operator'
        OR (
            lifecycle_actor_ref ~ '^(tailnet-node|scoped-token|fixture):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND lifecycle_actor_ref NOT LIKE '%:00000000-0000-0000-0000-000000000000'
        )
    ),
    CONSTRAINT study_session_lifecycle_events_version_positive CHECK (lifecycle_state_version >= 1)
);

CREATE UNIQUE INDEX study_session_lifecycle_events_version_uidx
    ON study_session_lifecycle_events (study_session_id, lifecycle_state_version);

COMMENT ON TABLE study_session_lifecycle_events IS 'Append-only audit of every session lifecycle creation and transition, including abandonment.';
COMMENT ON COLUMN study_session_lifecycle_events.study_session_lifecycle_event_id IS 'Server-generated lifecycle audit fact identity.';
COMMENT ON COLUMN study_session_lifecycle_events.study_session_id IS 'Session whose lifecycle changed.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_from_state IS 'Prior state; null only for the creation fact.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_to_state IS 'Durable resulting state.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_reason_code IS 'Stable machine-readable reason supplied by orchestration.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_actor_ref IS 'Non-secret operator or subsystem identity responsible for the transition.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_state_version IS 'Session CAS version produced by the transition.';
COMMENT ON COLUMN study_session_lifecycle_events.lifecycle_recorded_at IS 'UTC audit time.';

CREATE TRIGGER study_session_lifecycle_events_append_only
    BEFORE UPDATE OR DELETE ON study_session_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_session_creation_receipts (
    study_principal_id TEXT NOT NULL,
    learner_profile_id UUID NOT NULL REFERENCES study_learner_profiles(learner_profile_id) ON DELETE RESTRICT,
    normalized_corpus_selector_sha256 TEXT NOT NULL,
    normalized_corpus_selector TEXT NOT NULL,
    study_card_kind TEXT NOT NULL,
    creation_request_id UUID NOT NULL,
    normalized_creation_request_sha256 TEXT NOT NULL,
    study_session_id UUID NOT NULL,
    creation_disposition TEXT NOT NULL,
    creation_receipt_created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (
        study_principal_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind,
        creation_request_id
    ),
    CONSTRAINT study_session_creation_receipts_principal_valid CHECK (
        study_principal_id = 'operator'
        OR (
            study_principal_id ~ '^(tailnet-node|scoped-token|fixture):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND study_principal_id NOT LIKE '%:00000000-0000-0000-0000-000000000000'
        )
    ),
    CONSTRAINT study_session_creation_receipts_selector_is_object CHECK (jsonb_typeof(normalized_corpus_selector::jsonb) = 'object'),
    CONSTRAINT study_session_creation_receipts_selector_bounded CHECK (octet_length(normalized_corpus_selector) <= 8192),
    CONSTRAINT study_session_creation_receipts_selector_sha256_format CHECK (normalized_corpus_selector_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_session_creation_receipts_selector_sha256_matches CHECK (
        normalized_corpus_selector_sha256 = encode(digest(convert_to(normalized_corpus_selector, 'UTF8'), 'sha256'), 'hex')
    ),
    CONSTRAINT study_session_creation_receipts_card_kind_valid CHECK (study_card_kind = 'idea'),
    CONSTRAINT study_session_creation_receipts_sha256_format CHECK (normalized_creation_request_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_session_creation_receipts_disposition_valid CHECK (creation_disposition IN ('created', 'resumed', 'converged_to_existing', 'replayed', 'successor')),
    CONSTRAINT study_session_creation_receipts_session_scope_fk FOREIGN KEY (
        study_session_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind
    ) REFERENCES study_sessions (
        study_session_id,
        learner_profile_id,
        normalized_corpus_selector_sha256,
        normalized_corpus_selector,
        study_card_kind
    ) ON DELETE RESTRICT
);

CREATE INDEX study_session_creation_receipts_session_idx
    ON study_session_creation_receipts (study_session_id, creation_receipt_created_at);

COMMENT ON TABLE study_session_creation_receipts IS 'Immutable binding from a client creation UUID and normalized hash to the authoritative server session.';
COMMENT ON COLUMN study_session_creation_receipts.study_principal_id IS 'Opaque non-secret authorized principal identity; bearer bytes are never persisted.';
COMMENT ON COLUMN study_session_creation_receipts.learner_profile_id IS 'Learner component of the idempotency namespace.';
COMMENT ON COLUMN study_session_creation_receipts.normalized_corpus_selector_sha256 IS 'Digest component of the active-scope idempotency namespace.';
COMMENT ON COLUMN study_session_creation_receipts.normalized_corpus_selector IS 'Canonical selector bytes compared alongside the digest to defeat false hash convergence.';
COMMENT ON COLUMN study_session_creation_receipts.study_card_kind IS 'Card-kind component of the active-scope idempotency namespace.';
COMMENT ON COLUMN study_session_creation_receipts.creation_request_id IS 'Client-generated idempotency UUID; distinct from study_session_id.';
COMMENT ON COLUMN study_session_creation_receipts.normalized_creation_request_sha256 IS 'Digest that makes changed-input UUID reuse an explicit conflict.';
COMMENT ON COLUMN study_session_creation_receipts.study_session_id IS 'Authoritative created or converged session.';
COMMENT ON COLUMN study_session_creation_receipts.creation_disposition IS 'Original request adjudication, retained across replay.';
COMMENT ON COLUMN study_session_creation_receipts.creation_receipt_created_at IS 'UTC adjudication time.';

CREATE TRIGGER study_session_creation_receipts_append_only
    BEFORE UPDATE OR DELETE ON study_session_creation_receipts
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_session_items (
    study_session_item_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    study_session_id UUID NOT NULL REFERENCES study_sessions(study_session_id) ON DELETE RESTRICT,
    study_session_item_position INTEGER NOT NULL,
    study_card_kind TEXT NOT NULL,
    study_card_identity UUID NOT NULL,
    historical_source_kind TEXT NOT NULL,
    historical_source_identity UUID NOT NULL,
    live_source_idea_id UUID REFERENCES ideas(id) ON DELETE SET NULL,
    source_content_revision TEXT NOT NULL,
    source_content_sha256 TEXT NOT NULL,
    card_front_canonical_name TEXT NOT NULL,
    card_back_definition TEXT NOT NULL,
    card_back_example TEXT,
    selection_rating_rank TEXT NOT NULL,
    selection_latest_presented_ingestion_ordinal BIGINT,
    selection_tie_breaker_sha256 TEXT NOT NULL,
    selection_boundary_adjustment TEXT NOT NULL,
    study_session_item_created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_session_items_position_nonnegative CHECK (study_session_item_position >= 0),
    CONSTRAINT study_session_items_card_kind_valid CHECK (study_card_kind = 'idea'),
    CONSTRAINT study_session_items_source_kind_valid CHECK (historical_source_kind = 'idea'),
    CONSTRAINT study_session_items_revision_valid CHECK (
        char_length(source_content_revision) BETWEEN 1 AND 128
        AND btrim(source_content_revision) <> ''
    ),
    CONSTRAINT study_session_items_content_sha256_format CHECK (source_content_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_session_items_front_valid CHECK (
        char_length(card_front_canonical_name) BETWEEN 1 AND 512
        AND btrim(card_front_canonical_name) <> ''
    ),
    CONSTRAINT study_session_items_back_valid CHECK (
        char_length(card_back_definition) BETWEEN 1 AND 16384
        AND btrim(card_back_definition) <> ''
    ),
    CONSTRAINT study_session_items_example_valid CHECK (
        card_back_example IS NULL
        OR (char_length(card_back_example) BETWEEN 1 AND 16384 AND btrim(card_back_example) <> '')
    ),
    CONSTRAINT study_session_items_rating_rank_valid CHECK (selection_rating_rank IN ('again', 'hard', 'unseen_or_unrated', 'good', 'easy')),
    CONSTRAINT study_session_items_latest_presented_positive CHECK (selection_latest_presented_ingestion_ordinal IS NULL OR selection_latest_presented_ingestion_ordinal >= 1),
    CONSTRAINT study_session_items_tie_sha256_format CHECK (selection_tie_breaker_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_session_items_boundary_valid CHECK (selection_boundary_adjustment IN ('none', 'boundary_repeat_rotated', 'boundary_repeat_due_to_priority')),
    CONSTRAINT study_session_items_session_pair UNIQUE (study_session_item_id, study_session_id),
    CONSTRAINT study_session_items_event_identity_unique UNIQUE (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ),
    CONSTRAINT study_session_items_position_unique UNIQUE (study_session_id, study_session_item_position),
    CONSTRAINT study_session_items_card_unique UNIQUE (study_session_id, study_card_identity)
);

CREATE INDEX study_session_items_live_source_idx
    ON study_session_items (live_source_idea_id)
    WHERE live_source_idea_id IS NOT NULL;

COMMENT ON TABLE study_session_items IS 'Immutable issued front/back/example evidence plus a nullable link to the current editable idea.';
COMMENT ON COLUMN study_session_items.study_session_item_id IS 'Stable issued item identity used by all observations.';
COMMENT ON COLUMN study_session_items.study_session_id IS 'Finite cycle that owns this item.';
COMMENT ON COLUMN study_session_items.study_session_item_position IS 'Zero-based immutable order within the materialized cycle.';
COMMENT ON COLUMN study_session_items.study_card_kind IS 'Issued card family.';
COMMENT ON COLUMN study_session_items.study_card_identity IS 'Historical card identity used by scheduler evidence.';
COMMENT ON COLUMN study_session_items.historical_source_kind IS 'Kind of editable source captured at issuance.';
COMMENT ON COLUMN study_session_items.historical_source_identity IS 'Source UUID at issuance; never cascades or rewrites after merge/delete.';
COMMENT ON COLUMN study_session_items.live_source_idea_id IS 'Optional current editable idea link; merge may repoint it and hard deletion clears it.';
COMMENT ON COLUMN study_session_items.source_content_revision IS 'Stable source revision descriptor captured at issuance.';
COMMENT ON COLUMN study_session_items.source_content_sha256 IS 'Digest of canonical immutable projection content.';
COMMENT ON COLUMN study_session_items.card_front_canonical_name IS 'Issued front text, independent of later vocabulary edits.';
COMMENT ON COLUMN study_session_items.card_back_definition IS 'Issued back definition, independent of later vocabulary edits.';
COMMENT ON COLUMN study_session_items.card_back_example IS 'Optional issued example, independent of later vocabulary edits.';
COMMENT ON COLUMN study_session_items.selection_rating_rank IS 'Scheduler rank that selected this item.';
COMMENT ON COLUMN study_session_items.selection_latest_presented_ingestion_ordinal IS 'Latest accepted presented ordinal visible to the scheduler, or null sentinel.';
COMMENT ON COLUMN study_session_items.selection_tie_breaker_sha256 IS 'Deterministic seeded tie-break evidence.';
COMMENT ON COLUMN study_session_items.selection_boundary_adjustment IS 'Recorded cycle-boundary adjustment provenance.';
COMMENT ON COLUMN study_session_items.study_session_item_created_at IS 'UTC issuance time.';

CREATE FUNCTION study_guard_session_item_update()
RETURNS trigger AS $$
BEGIN
    IF NEW.study_session_item_id IS DISTINCT FROM OLD.study_session_item_id
       OR NEW.study_session_id IS DISTINCT FROM OLD.study_session_id
       OR NEW.study_session_item_position IS DISTINCT FROM OLD.study_session_item_position
       OR NEW.study_card_kind IS DISTINCT FROM OLD.study_card_kind
       OR NEW.study_card_identity IS DISTINCT FROM OLD.study_card_identity
       OR NEW.historical_source_kind IS DISTINCT FROM OLD.historical_source_kind
       OR NEW.historical_source_identity IS DISTINCT FROM OLD.historical_source_identity
       OR NEW.source_content_revision IS DISTINCT FROM OLD.source_content_revision
       OR NEW.source_content_sha256 IS DISTINCT FROM OLD.source_content_sha256
       OR NEW.card_front_canonical_name IS DISTINCT FROM OLD.card_front_canonical_name
       OR NEW.card_back_definition IS DISTINCT FROM OLD.card_back_definition
       OR NEW.card_back_example IS DISTINCT FROM OLD.card_back_example
       OR NEW.selection_rating_rank IS DISTINCT FROM OLD.selection_rating_rank
       OR NEW.selection_latest_presented_ingestion_ordinal IS DISTINCT FROM OLD.selection_latest_presented_ingestion_ordinal
       OR NEW.selection_tie_breaker_sha256 IS DISTINCT FROM OLD.selection_tie_breaker_sha256
       OR NEW.selection_boundary_adjustment IS DISTINCT FROM OLD.selection_boundary_adjustment
       OR NEW.study_session_item_created_at IS DISTINCT FROM OLD.study_session_item_created_at THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'study_session_items immutable projection fields cannot be updated';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_session_items_guard_update
    BEFORE UPDATE ON study_session_items
    FOR EACH ROW EXECUTE FUNCTION study_guard_session_item_update();
CREATE TRIGGER study_session_items_reject_delete
    BEFORE DELETE ON study_session_items
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_client_streams (
    client_stream_id UUID PRIMARY KEY,
    learner_profile_id UUID NOT NULL REFERENCES study_learner_profiles(learner_profile_id) ON DELETE RESTRICT,
    adjudicated_through_client_sequence BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_client_streams_sequence_nonnegative CHECK (adjudicated_through_client_sequence >= 0),
    CONSTRAINT study_client_streams_id_profile_unique UNIQUE (client_stream_id, learner_profile_id)
);

CREATE INDEX study_client_streams_profile_idx
    ON study_client_streams (learner_profile_id, created_at, client_stream_id);

COMMENT ON TABLE study_client_streams IS 'Durable random client stream identities and their contiguous terminal-adjudication cursor.';
COMMENT ON COLUMN study_client_streams.client_stream_id IS 'Client-generated durable random stream identity; never derived from an appliance secret.';
COMMENT ON COLUMN study_client_streams.learner_profile_id IS 'Learner to which every event on this stream must belong.';
COMMENT ON COLUMN study_client_streams.adjudicated_through_client_sequence IS 'Largest contiguous sequence with an immutable terminal receipt.';
COMMENT ON COLUMN study_client_streams.created_at IS 'UTC first-seen time.';
COMMENT ON COLUMN study_client_streams.updated_at IS 'UTC time the contiguous cursor last changed.';

CREATE TRIGGER study_client_streams_set_updated_at
    BEFORE UPDATE ON study_client_streams
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE study_event_id_bindings (
    client_event_id UUID PRIMARY KEY,
    bound_event_payload_sha256 TEXT NOT NULL,
    learner_profile_id UUID NOT NULL,
    study_session_id UUID NOT NULL,
    study_session_item_id UUID NOT NULL,
    study_card_identity UUID NOT NULL,
    source_content_sha256 TEXT NOT NULL,
    first_client_stream_id UUID NOT NULL,
    first_client_sequence BIGINT NOT NULL,
    event_id_bound_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_event_id_bindings_payload_sha256_format CHECK (bound_event_payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_event_id_bindings_content_sha256_format CHECK (source_content_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_event_id_bindings_sequence_positive CHECK (first_client_sequence >= 1),
    CONSTRAINT study_event_id_bindings_full_tuple_unique UNIQUE (
        client_event_id,
        bound_event_payload_sha256,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        source_content_sha256
    ),
    CONSTRAINT study_event_id_bindings_id_payload_unique UNIQUE (
        client_event_id,
        bound_event_payload_sha256
    ),
    CONSTRAINT study_event_id_bindings_session_profile_fk FOREIGN KEY (study_session_id, learner_profile_id)
        REFERENCES study_sessions(study_session_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_event_id_bindings_item_identity_fk FOREIGN KEY (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) REFERENCES study_session_items (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) ON DELETE RESTRICT,
    CONSTRAINT study_event_id_bindings_stream_profile_fk FOREIGN KEY (first_client_stream_id, learner_profile_id)
        REFERENCES study_client_streams(client_stream_id, learner_profile_id) ON DELETE RESTRICT
);

COMMENT ON TABLE study_event_id_bindings IS 'Global immutable binding of every first-seen submitted event UUID, including terminal rejections, to one payload and study scope.';
COMMENT ON COLUMN study_event_id_bindings.client_event_id IS 'Globally unique client event UUID authority.';
COMMENT ON COLUMN study_event_id_bindings.bound_event_payload_sha256 IS 'First-seen canonical payload digest; changed reuse is STUDY_EVENT_ID_CONFLICT.';
COMMENT ON COLUMN study_event_id_bindings.learner_profile_id IS 'Learner scope permanently bound to the UUID.';
COMMENT ON COLUMN study_event_id_bindings.study_session_id IS 'Session scope permanently bound to the UUID.';
COMMENT ON COLUMN study_event_id_bindings.study_session_item_id IS 'Issued item permanently bound to the UUID.';
COMMENT ON COLUMN study_event_id_bindings.study_card_identity IS 'Historical card identity permanently bound to the UUID.';
COMMENT ON COLUMN study_event_id_bindings.source_content_sha256 IS 'Issued content digest permanently bound to the UUID.';
COMMENT ON COLUMN study_event_id_bindings.first_client_stream_id IS 'Stream on which the UUID was first terminally adjudicated.';
COMMENT ON COLUMN study_event_id_bindings.first_client_sequence IS 'Sequence on which the UUID was first terminally adjudicated.';
COMMENT ON COLUMN study_event_id_bindings.event_id_bound_at IS 'UTC first terminal adjudication time.';

CREATE TRIGGER study_event_id_bindings_append_only
    BEFORE UPDATE OR DELETE ON study_event_id_bindings
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_events (
    study_event_id UUID PRIMARY KEY,
    learner_profile_id UUID NOT NULL REFERENCES study_learner_profiles(learner_profile_id) ON DELETE RESTRICT,
    study_session_id UUID NOT NULL,
    study_session_item_id UUID NOT NULL,
    study_card_identity UUID NOT NULL,
    study_event_kind TEXT NOT NULL,
    study_rating TEXT,
    rating_event_id UUID REFERENCES study_events(study_event_id) ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE,
    source_content_sha256 TEXT NOT NULL,
    client_stream_id UUID NOT NULL,
    client_sequence BIGINT NOT NULL,
    normalized_event_payload_sha256 TEXT NOT NULL,
    client_observed_at TIMESTAMPTZ,
    client_clock_quality TEXT NOT NULL,
    profile_ingestion_ordinal BIGINT NOT NULL,
    study_event_received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_events_session_profile_fk FOREIGN KEY (study_session_id, learner_profile_id)
        REFERENCES study_sessions(study_session_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_events_item_identity_fk FOREIGN KEY (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) REFERENCES study_session_items (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) ON DELETE RESTRICT,
    CONSTRAINT study_events_stream_profile_fk FOREIGN KEY (client_stream_id, learner_profile_id)
        REFERENCES study_client_streams(client_stream_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_events_global_event_binding_fk FOREIGN KEY (
        study_event_id,
        normalized_event_payload_sha256,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        source_content_sha256
    ) REFERENCES study_event_id_bindings (
        client_event_id,
        bound_event_payload_sha256,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        source_content_sha256
    ) ON DELETE RESTRICT,
    CONSTRAINT study_events_kind_valid CHECK (study_event_kind IN ('presented', 'revealed', 'rated', 'rating_voided')),
    CONSTRAINT study_events_payload_valid CHECK (
        (study_event_kind IN ('presented', 'revealed') AND study_rating IS NULL AND rating_event_id IS NULL)
        OR (study_event_kind = 'rated' AND study_rating IS NOT NULL AND study_rating IN ('again', 'hard', 'good', 'easy') AND rating_event_id IS NULL)
        OR (study_event_kind = 'rating_voided' AND study_rating IS NULL AND rating_event_id IS NOT NULL)
    ),
    CONSTRAINT study_events_content_sha256_format CHECK (source_content_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_events_sequence_positive CHECK (client_sequence >= 1),
    CONSTRAINT study_events_payload_sha256_format CHECK (normalized_event_payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_events_clock_quality_valid CHECK (client_clock_quality IN ('trusted', 'bounded_uncertain', 'unknown')),
    CONSTRAINT study_events_ingestion_ordinal_positive CHECK (profile_ingestion_ordinal >= 1),
    CONSTRAINT study_events_stream_sequence_unique UNIQUE (client_stream_id, client_sequence),
    CONSTRAINT study_events_profile_ordinal_unique UNIQUE (learner_profile_id, profile_ingestion_ordinal),
    CONSTRAINT study_events_receipt_identity_unique UNIQUE (
        study_event_id,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        profile_ingestion_ordinal
    ),
    CONSTRAINT study_events_projection_identity_unique UNIQUE (
        study_event_id,
        learner_profile_id,
        study_card_identity,
        profile_ingestion_ordinal
    )
);

CREATE UNIQUE INDEX study_events_one_presented_per_item_uidx
    ON study_events (study_session_item_id)
    WHERE study_event_kind = 'presented';
CREATE UNIQUE INDEX study_events_one_revealed_per_item_uidx
    ON study_events (study_session_item_id)
    WHERE study_event_kind = 'revealed';
CREATE UNIQUE INDEX study_events_one_void_per_rating_uidx
    ON study_events (rating_event_id)
    WHERE study_event_kind = 'rating_voided';
CREATE INDEX study_events_profile_prefix_idx
    ON study_events (learner_profile_id, profile_ingestion_ordinal);
CREATE INDEX study_events_card_prefix_idx
    ON study_events (learner_profile_id, study_card_identity, profile_ingestion_ordinal);

COMMENT ON TABLE study_events IS 'Append-only accepted scheduler facts ordered by one gap-free per-profile ingestion ordinal.';
COMMENT ON COLUMN study_events.study_event_id IS 'Canonical accepted fact identity; normally the client event UUID.';
COMMENT ON COLUMN study_events.learner_profile_id IS 'Learner whose prefix includes this fact.';
COMMENT ON COLUMN study_events.study_session_id IS 'Issued session against which the retained fact was observed.';
COMMENT ON COLUMN study_events.study_session_item_id IS 'Exact issued item observed.';
COMMENT ON COLUMN study_events.study_card_identity IS 'Historical card identity retained even after source merge/delete.';
COMMENT ON COLUMN study_events.study_event_kind IS 'Presented, revealed, rated, or rating_voided fact kind.';
COMMENT ON COLUMN study_events.study_rating IS 'Rating value only for rated facts.';
COMMENT ON COLUMN study_events.rating_event_id IS 'Exact rated fact voided by a rating_voided fact.';
COMMENT ON COLUMN study_events.source_content_sha256 IS 'Issued content digest asserted by the observation.';
COMMENT ON COLUMN study_events.client_stream_id IS 'Durable source stream that first caused acceptance.';
COMMENT ON COLUMN study_events.client_sequence IS 'Source stream sequence that first caused acceptance.';
COMMENT ON COLUMN study_events.normalized_event_payload_sha256 IS 'Digest of canonical event bytes used for conflict detection.';
COMMENT ON COLUMN study_events.client_observed_at IS 'Optional client wall-clock evidence; never an ordering authority.';
COMMENT ON COLUMN study_events.client_clock_quality IS 'Declared quality of optional client wall-clock evidence.';
COMMENT ON COLUMN study_events.profile_ingestion_ordinal IS 'Gap-free authoritative per-profile scheduler order.';
COMMENT ON COLUMN study_events.study_event_received_at IS 'Server UTC acceptance time; evidence, not a selection key.';

CREATE FUNCTION study_validate_rating_void_target()
RETURNS trigger AS $$
BEGIN
    IF NEW.study_event_kind = 'rating_voided' AND NOT EXISTS (
        SELECT 1
        FROM study_events rated_event
        WHERE rated_event.study_event_id = NEW.rating_event_id
          AND rated_event.study_event_kind = 'rated'
          AND rated_event.learner_profile_id = NEW.learner_profile_id
          AND rated_event.study_session_id = NEW.study_session_id
          AND rated_event.study_session_item_id = NEW.study_session_item_id
          AND rated_event.study_card_identity = NEW.study_card_identity
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'rating_voided target must be a rated fact for the same learner, session item, and card';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_events_validate_rating_void_target
    BEFORE INSERT ON study_events
    FOR EACH ROW EXECUTE FUNCTION study_validate_rating_void_target();

CREATE TRIGGER study_events_append_only
    BEFORE UPDATE OR DELETE ON study_events
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_event_receipts (
    client_stream_id UUID NOT NULL,
    client_sequence BIGINT NOT NULL,
    learner_profile_id UUID NOT NULL,
    study_session_id UUID NOT NULL,
    study_session_item_id UUID NOT NULL,
    study_card_identity UUID NOT NULL,
    source_content_sha256 TEXT NOT NULL,
    client_event_id UUID NOT NULL,
    bound_event_payload_sha256 TEXT NOT NULL,
    normalized_event_payload_sha256 TEXT NOT NULL,
    event_id_binding_matches_input BOOLEAN NOT NULL,
    event_receipt_disposition TEXT NOT NULL,
    event_receipt_code TEXT NOT NULL,
    canonical_study_event_id UUID,
    canonical_profile_ingestion_ordinal BIGINT,
    study_event_receipt_created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (client_stream_id, client_sequence),
    CONSTRAINT study_event_receipts_sequence_positive CHECK (client_sequence >= 1),
    CONSTRAINT study_event_receipts_content_sha256_format CHECK (source_content_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_event_receipts_bound_sha256_format CHECK (bound_event_payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_event_receipts_payload_sha256_format CHECK (normalized_event_payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_event_receipts_disposition_valid CHECK (event_receipt_disposition IN ('accepted', 'semantic_duplicate', 'terminal_rejected')),
    CONSTRAINT study_event_receipts_code_valid CHECK (event_receipt_code IN (
        'STUDY_EVENT_ACCEPTED',
        'STUDY_EVENT_SEMANTIC_DUPLICATE',
        'STUDY_EVENT_TRANSITION_INVALID',
        'STUDY_EVENT_ID_CONFLICT',
        'STUDY_RATING_ALREADY_EFFECTIVE',
        'STUDY_RATING_VOID_TARGET_INVALID',
        'STUDY_CONTENT_REVISION_MISMATCH'
    )),
    CONSTRAINT study_event_receipts_canonical_shape CHECK (
        (event_receipt_disposition = 'accepted'
            AND event_receipt_code = 'STUDY_EVENT_ACCEPTED'
            AND canonical_study_event_id IS NOT NULL
            AND canonical_profile_ingestion_ordinal IS NOT NULL
            AND canonical_profile_ingestion_ordinal >= 1)
        OR (event_receipt_disposition = 'semantic_duplicate'
            AND event_receipt_code = 'STUDY_EVENT_SEMANTIC_DUPLICATE'
            AND canonical_study_event_id IS NOT NULL
            AND canonical_profile_ingestion_ordinal IS NOT NULL
            AND canonical_profile_ingestion_ordinal >= 1)
        OR (event_receipt_disposition = 'terminal_rejected'
            AND event_receipt_code IN (
                'STUDY_EVENT_TRANSITION_INVALID',
                'STUDY_EVENT_ID_CONFLICT',
                'STUDY_RATING_ALREADY_EFFECTIVE',
                'STUDY_RATING_VOID_TARGET_INVALID',
                'STUDY_CONTENT_REVISION_MISMATCH'
            )
            AND canonical_study_event_id IS NULL
            AND canonical_profile_ingestion_ordinal IS NULL)
    ),
    CONSTRAINT study_event_receipts_id_conflict_shape CHECK (
        (event_receipt_code = 'STUDY_EVENT_ID_CONFLICT' AND NOT event_id_binding_matches_input)
        OR (event_receipt_code <> 'STUDY_EVENT_ID_CONFLICT' AND event_id_binding_matches_input)
    ),
    CONSTRAINT study_event_receipts_stream_profile_fk FOREIGN KEY (client_stream_id, learner_profile_id)
        REFERENCES study_client_streams(client_stream_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_event_receipts_session_profile_fk FOREIGN KEY (study_session_id, learner_profile_id)
        REFERENCES study_sessions(study_session_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_event_receipts_item_identity_fk FOREIGN KEY (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) REFERENCES study_session_items (
        study_session_item_id,
        study_session_id,
        study_card_identity,
        source_content_sha256
    ) ON DELETE RESTRICT,
    CONSTRAINT study_event_receipts_global_event_binding_fk FOREIGN KEY (client_event_id, bound_event_payload_sha256)
        REFERENCES study_event_id_bindings(client_event_id, bound_event_payload_sha256) ON DELETE RESTRICT,
    CONSTRAINT study_event_receipts_canonical_event_fk FOREIGN KEY (
        canonical_study_event_id,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        canonical_profile_ingestion_ordinal
    ) REFERENCES study_events (
        study_event_id,
        learner_profile_id,
        study_session_id,
        study_session_item_id,
        study_card_identity,
        profile_ingestion_ordinal
    ) ON DELETE RESTRICT
);

CREATE INDEX study_event_receipts_client_event_idx
    ON study_event_receipts (client_event_id, normalized_event_payload_sha256);

COMMENT ON TABLE study_event_receipts IS 'Immutable terminal adjudication for every consumed contiguous client stream sequence.';
COMMENT ON COLUMN study_event_receipts.client_stream_id IS 'Durable client stream whose sequence was consumed.';
COMMENT ON COLUMN study_event_receipts.client_sequence IS 'Consumed contiguous stream sequence.';
COMMENT ON COLUMN study_event_receipts.learner_profile_id IS 'Learner ownership bound to the durable stream and session.';
COMMENT ON COLUMN study_event_receipts.study_session_id IS 'Session scope that prevents replay through a different session endpoint.';
COMMENT ON COLUMN study_event_receipts.study_session_item_id IS 'Exact issued item named by the adjudicated input.';
COMMENT ON COLUMN study_event_receipts.study_card_identity IS 'Exact historical card identity named by the adjudicated input.';
COMMENT ON COLUMN study_event_receipts.source_content_sha256 IS 'Exact immutable issued content digest named by the input.';
COMMENT ON COLUMN study_event_receipts.client_event_id IS 'Submitted event UUID, including rejected and semantic-duplicate inputs.';
COMMENT ON COLUMN study_event_receipts.bound_event_payload_sha256 IS 'First-seen global UUID binding used to identify changed-input conflicts.';
COMMENT ON COLUMN study_event_receipts.normalized_event_payload_sha256 IS 'Canonical payload digest bound to this sequence.';
COMMENT ON COLUMN study_event_receipts.event_id_binding_matches_input IS 'Stored result of comparing the entire current input tuple with the immutable first-seen UUID binding.';
COMMENT ON COLUMN study_event_receipts.event_receipt_disposition IS 'Accepted fact, semantic duplicate, or stable terminal rejection.';
COMMENT ON COLUMN study_event_receipts.event_receipt_code IS 'Stable study.v1 adjudication code.';
COMMENT ON COLUMN study_event_receipts.canonical_study_event_id IS 'Accepted scheduler fact, including the original fact for a duplicate.';
COMMENT ON COLUMN study_event_receipts.canonical_profile_ingestion_ordinal IS 'Original accepted fact ordinal; never allocated for rejection.';
COMMENT ON COLUMN study_event_receipts.study_event_receipt_created_at IS 'UTC adjudication time.';

CREATE FUNCTION study_validate_event_receipt_binding()
RETURNS trigger AS $$
DECLARE
    binding_matches BOOLEAN;
BEGIN
    SELECT
        binding.bound_event_payload_sha256 = NEW.normalized_event_payload_sha256
        AND binding.learner_profile_id = NEW.learner_profile_id
        AND binding.study_session_id = NEW.study_session_id
        AND binding.study_session_item_id = NEW.study_session_item_id
        AND binding.study_card_identity = NEW.study_card_identity
        AND binding.source_content_sha256 = NEW.source_content_sha256
    INTO STRICT binding_matches
    FROM study_event_id_bindings binding
    WHERE binding.client_event_id = NEW.client_event_id
      AND binding.bound_event_payload_sha256 = NEW.bound_event_payload_sha256;

    IF binding_matches IS DISTINCT FROM NEW.event_id_binding_matches_input THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'event receipt binding comparison does not match the immutable global event UUID binding';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_event_receipts_validate_binding
    BEFORE INSERT ON study_event_receipts
    FOR EACH ROW EXECUTE FUNCTION study_validate_event_receipt_binding();

CREATE TRIGGER study_event_receipts_append_only
    BEFORE UPDATE OR DELETE ON study_event_receipts
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_flush_receipts (
    study_flush_receipt_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    study_principal_id TEXT NOT NULL,
    learner_profile_id UUID NOT NULL,
    sync_request_id UUID NOT NULL,
    normalized_sync_request_sha256 TEXT NOT NULL,
    study_session_id UUID NOT NULL,
    client_stream_id UUID NOT NULL,
    first_client_sequence BIGINT,
    last_client_sequence BIGINT,
    acknowledged_through_client_sequence BIGINT NOT NULL,
    profile_ingestion_high_water BIGINT NOT NULL,
    study_session_state_version BIGINT NOT NULL,
    study_flush_receipt_issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_flush_receipts_sync_request_scope_unique UNIQUE (study_principal_id, study_session_id, sync_request_id),
    CONSTRAINT study_flush_receipts_principal_valid CHECK (
        study_principal_id = 'operator'
        OR (
            study_principal_id ~ '^(tailnet-node|scoped-token|fixture):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND study_principal_id NOT LIKE '%:00000000-0000-0000-0000-000000000000'
        )
    ),
    CONSTRAINT study_flush_receipts_sha256_format CHECK (normalized_sync_request_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_flush_receipts_range_consistent CHECK (
        (first_client_sequence IS NULL AND last_client_sequence IS NULL)
        OR (
            first_client_sequence IS NOT NULL
            AND last_client_sequence IS NOT NULL
            AND first_client_sequence >= 1
            AND last_client_sequence >= first_client_sequence
        )
    ),
    CONSTRAINT study_flush_receipts_ack_nonnegative CHECK (acknowledged_through_client_sequence >= 0),
    CONSTRAINT study_flush_receipts_high_water_nonnegative CHECK (profile_ingestion_high_water >= 0),
    CONSTRAINT study_flush_receipts_state_version_positive CHECK (study_session_state_version >= 1),
    CONSTRAINT study_flush_receipts_session_profile_fk FOREIGN KEY (study_session_id, learner_profile_id)
        REFERENCES study_sessions(study_session_id, learner_profile_id) ON DELETE RESTRICT,
    CONSTRAINT study_flush_receipts_stream_profile_fk FOREIGN KEY (client_stream_id, learner_profile_id)
        REFERENCES study_client_streams(client_stream_id, learner_profile_id) ON DELETE RESTRICT
);

CREATE INDEX study_flush_receipts_session_stream_idx
    ON study_flush_receipts (study_session_id, client_stream_id, acknowledged_through_client_sequence);

COMMENT ON TABLE study_flush_receipts IS 'Immutable sync-idempotency and predecessor-flush barrier facts.';
COMMENT ON COLUMN study_flush_receipts.study_flush_receipt_id IS 'Server-generated flush barrier identity.';
COMMENT ON COLUMN study_flush_receipts.study_principal_id IS 'Opaque non-secret authorized principal component of the sync idempotency namespace.';
COMMENT ON COLUMN study_flush_receipts.learner_profile_id IS 'Learner ownership shared by the session and durable stream.';
COMMENT ON COLUMN study_flush_receipts.sync_request_id IS 'Client-generated sync idempotency UUID.';
COMMENT ON COLUMN study_flush_receipts.normalized_sync_request_sha256 IS 'Digest that makes changed-input sync UUID reuse an explicit conflict.';
COMMENT ON COLUMN study_flush_receipts.study_session_id IS 'Session whose retained suffix was adjudicated.';
COMMENT ON COLUMN study_flush_receipts.client_stream_id IS 'Durable stream advanced or observed by this sync.';
COMMENT ON COLUMN study_flush_receipts.first_client_sequence IS 'First sequence in this sync, null for an empty suffix.';
COMMENT ON COLUMN study_flush_receipts.last_client_sequence IS 'Last sequence in this sync, null for an empty suffix.';
COMMENT ON COLUMN study_flush_receipts.acknowledged_through_client_sequence IS 'Contiguous stream high-water after adjudication.';
COMMENT ON COLUMN study_flush_receipts.profile_ingestion_high_water IS 'Accepted profile prefix after adjudication.';
COMMENT ON COLUMN study_flush_receipts.study_session_state_version IS 'Session CAS version returned by the sync.';
COMMENT ON COLUMN study_flush_receipts.study_flush_receipt_issued_at IS 'Stable UTC generated_at source for idempotent replay.';

CREATE TRIGGER study_flush_receipts_append_only
    BEFORE UPDATE OR DELETE ON study_flush_receipts
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_card_identity_redirects (
    historical_study_card_identity UUID PRIMARY KEY,
    survivor_study_card_identity UUID NOT NULL,
    identity_redirect_reason TEXT NOT NULL,
    identity_redirected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT study_card_identity_redirects_not_self CHECK (historical_study_card_identity <> survivor_study_card_identity),
    CONSTRAINT study_card_identity_redirects_reason_valid CHECK (identity_redirect_reason = 'idea_merged')
);

COMMENT ON TABLE study_card_identity_redirects IS 'Append-only identity map used to project historical source-card evidence onto a current merge survivor without rewriting events.';
COMMENT ON COLUMN study_card_identity_redirects.historical_study_card_identity IS 'Historical source/card identity retained by issued items and accepted events.';
COMMENT ON COLUMN study_card_identity_redirects.survivor_study_card_identity IS 'Current merge-survivor identity used for future eligibility.';
COMMENT ON COLUMN study_card_identity_redirects.identity_redirect_reason IS 'Stable reason for the identity mapping.';
COMMENT ON COLUMN study_card_identity_redirects.identity_redirected_at IS 'UTC merge-consolidation time.';

CREATE FUNCTION study_reject_card_identity_redirect_cycle()
RETURNS trigger AS $$
DECLARE
    creates_cycle BOOLEAN;
BEGIN
    WITH RECURSIVE survivor_path(identity) AS (
        SELECT NEW.survivor_study_card_identity
        UNION
        SELECT redirect.survivor_study_card_identity
        FROM survivor_path path
        JOIN study_card_identity_redirects redirect
          ON redirect.historical_study_card_identity = path.identity
    )
    SELECT EXISTS (
        SELECT 1
        FROM survivor_path
        WHERE identity = NEW.historical_study_card_identity
    ) INTO creates_cycle;

    IF creates_cycle THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'study card identity redirect would create a cycle';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_card_identity_redirects_reject_cycle
    BEFORE INSERT ON study_card_identity_redirects
    FOR EACH ROW EXECUTE FUNCTION study_reject_card_identity_redirect_cycle();

CREATE TRIGGER study_card_identity_redirects_append_only
    BEFORE UPDATE OR DELETE ON study_card_identity_redirects
    FOR EACH ROW EXECUTE FUNCTION study_reject_append_only_mutation();

CREATE TABLE study_scheduler_card_projections (
    learner_profile_id UUID NOT NULL REFERENCES study_learner_profiles(learner_profile_id) ON DELETE RESTRICT,
    projected_study_card_identity UUID NOT NULL,
    live_source_idea_id UUID REFERENCES ideas(id) ON DELETE SET NULL,
    study_scheduler_name TEXT NOT NULL,
    study_scheduler_version TEXT NOT NULL,
    projection_schema_version INTEGER NOT NULL,
    latest_effective_rating_event_id UUID REFERENCES study_events(study_event_id) ON DELETE RESTRICT,
    latest_effective_rating_evidence_card_identity UUID,
    latest_effective_rating TEXT,
    latest_effective_rating_ingestion_ordinal BIGINT,
    latest_presented_event_id UUID REFERENCES study_events(study_event_id) ON DELETE RESTRICT,
    latest_presented_evidence_card_identity UUID,
    latest_presented_ingestion_ordinal BIGINT,
    rebuilt_through_profile_ingestion_ordinal BIGINT NOT NULL,
    canonical_projection_payload TEXT NOT NULL,
    canonical_projection_sha256 TEXT NOT NULL,
    projection_revision BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (
        learner_profile_id,
        projected_study_card_identity,
        study_scheduler_name,
        study_scheduler_version,
        projection_schema_version
    ),
    CONSTRAINT study_scheduler_card_projections_scheduler_name_valid CHECK (
        char_length(study_scheduler_name) BETWEEN 1 AND 128 AND btrim(study_scheduler_name) <> ''
    ),
    CONSTRAINT study_scheduler_card_projections_scheduler_version_valid CHECK (
        char_length(study_scheduler_version) BETWEEN 1 AND 64 AND btrim(study_scheduler_version) <> ''
    ),
    CONSTRAINT study_scheduler_card_projections_schema_version_positive CHECK (projection_schema_version >= 1),
    CONSTRAINT study_scheduler_card_projections_rating_shape CHECK (
        (
            latest_effective_rating_event_id IS NULL
            AND latest_effective_rating_evidence_card_identity IS NULL
            AND latest_effective_rating IS NULL
            AND latest_effective_rating_ingestion_ordinal IS NULL
        )
        OR (
            latest_effective_rating_event_id IS NOT NULL
            AND latest_effective_rating_evidence_card_identity IS NOT NULL
            AND latest_effective_rating IS NOT NULL
            AND latest_effective_rating IN ('again', 'hard', 'good', 'easy')
            AND latest_effective_rating_ingestion_ordinal IS NOT NULL
            AND latest_effective_rating_ingestion_ordinal >= 1
        )
    ),
    CONSTRAINT study_scheduler_card_projections_presented_shape CHECK (
        (
            latest_presented_event_id IS NULL
            AND latest_presented_evidence_card_identity IS NULL
            AND latest_presented_ingestion_ordinal IS NULL
        )
        OR (
            latest_presented_event_id IS NOT NULL
            AND latest_presented_evidence_card_identity IS NOT NULL
            AND latest_presented_ingestion_ordinal IS NOT NULL
            AND latest_presented_ingestion_ordinal >= 1
        )
    ),
    CONSTRAINT study_scheduler_card_projections_rebuilt_nonnegative CHECK (rebuilt_through_profile_ingestion_ordinal >= 0),
    CONSTRAINT study_scheduler_card_projections_payload_is_object CHECK (jsonb_typeof(canonical_projection_payload::jsonb) = 'object'),
    CONSTRAINT study_scheduler_card_projections_payload_bounded CHECK (octet_length(canonical_projection_payload) <= 262144),
    CONSTRAINT study_scheduler_card_projections_sha256_format CHECK (canonical_projection_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT study_scheduler_card_projections_sha256_matches CHECK (
        canonical_projection_sha256 = encode(digest(convert_to(canonical_projection_payload, 'UTF8'), 'sha256'), 'hex')
    ),
    CONSTRAINT study_scheduler_card_projections_revision_positive CHECK (projection_revision >= 1),
    CONSTRAINT study_scheduler_card_projections_latest_rating_event_fk FOREIGN KEY (
        latest_effective_rating_event_id,
        learner_profile_id,
        latest_effective_rating_evidence_card_identity,
        latest_effective_rating_ingestion_ordinal
    ) REFERENCES study_events (
        study_event_id,
        learner_profile_id,
        study_card_identity,
        profile_ingestion_ordinal
    ) ON DELETE RESTRICT,
    CONSTRAINT study_scheduler_card_projections_latest_presented_event_fk FOREIGN KEY (
        latest_presented_event_id,
        learner_profile_id,
        latest_presented_evidence_card_identity,
        latest_presented_ingestion_ordinal
    ) REFERENCES study_events (
        study_event_id,
        learner_profile_id,
        study_card_identity,
        profile_ingestion_ordinal
    ) ON DELETE RESTRICT
);

CREATE INDEX study_scheduler_card_projections_live_source_idx
    ON study_scheduler_card_projections (live_source_idea_id)
    WHERE live_source_idea_id IS NOT NULL;

COMMENT ON TABLE study_scheduler_card_projections IS 'Named/versioned rebuildable per-card scheduler state; never the append-only authority.';
COMMENT ON COLUMN study_scheduler_card_projections.learner_profile_id IS 'Learner whose accepted prefix was projected.';
COMMENT ON COLUMN study_scheduler_card_projections.projected_study_card_identity IS 'Current scheduler identity; merge consolidation may move source state to a survivor.';
COMMENT ON COLUMN study_scheduler_card_projections.live_source_idea_id IS 'Nullable current editable source link.';
COMMENT ON COLUMN study_scheduler_card_projections.study_scheduler_name IS 'Projection algorithm name.';
COMMENT ON COLUMN study_scheduler_card_projections.study_scheduler_version IS 'Projection algorithm version.';
COMMENT ON COLUMN study_scheduler_card_projections.projection_schema_version IS 'Positive state-schema namespace; independent from mutable projection revision.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_effective_rating_event_id IS 'Latest unvoided rating fact selected by the projector.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_effective_rating_evidence_card_identity IS 'Historical event card identity; may reach the projected survivor through redirects.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_effective_rating IS 'Latest effective rating value.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_effective_rating_ingestion_ordinal IS 'Ordinal of the latest effective rating.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_presented_event_id IS 'Latest presented fact selected by the projector.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_presented_evidence_card_identity IS 'Historical presented-event card identity; may reach the projected survivor through redirects.';
COMMENT ON COLUMN study_scheduler_card_projections.latest_presented_ingestion_ordinal IS 'Ordinal of the latest presented fact.';
COMMENT ON COLUMN study_scheduler_card_projections.rebuilt_through_profile_ingestion_ordinal IS 'Exact accepted prefix included in canonical payload.';
COMMENT ON COLUMN study_scheduler_card_projections.canonical_projection_payload IS 'Canonical deterministic projector output; excludes wall-clock update metadata.';
COMMENT ON COLUMN study_scheduler_card_projections.canonical_projection_sha256 IS 'SHA-256 of canonical projection bytes for byte-identical replay proof.';
COMMENT ON COLUMN study_scheduler_card_projections.projection_revision IS 'Optimistic replacement version for derived state.';
COMMENT ON COLUMN study_scheduler_card_projections.updated_at IS 'Operational UTC update time excluded from canonical projection bytes.';

CREATE FUNCTION study_validate_projection_identity_redirects()
RETURNS trigger AS $$
DECLARE
    rating_identity_reaches_projection BOOLEAN;
    presented_identity_reaches_projection BOOLEAN;
BEGIN
    IF NEW.latest_effective_rating_evidence_card_identity IS NOT NULL THEN
        WITH RECURSIVE identity_path(identity) AS (
            SELECT NEW.latest_effective_rating_evidence_card_identity
            UNION
            SELECT redirect.survivor_study_card_identity
            FROM identity_path path
            JOIN study_card_identity_redirects redirect
              ON redirect.historical_study_card_identity = path.identity
        )
        SELECT EXISTS (
            SELECT 1 FROM identity_path WHERE identity = NEW.projected_study_card_identity
        ) INTO rating_identity_reaches_projection;
        IF NOT rating_identity_reaches_projection THEN
            RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'latest rating evidence card does not resolve to projected card identity';
        END IF;
    END IF;

    IF NEW.latest_presented_evidence_card_identity IS NOT NULL THEN
        WITH RECURSIVE identity_path(identity) AS (
            SELECT NEW.latest_presented_evidence_card_identity
            UNION
            SELECT redirect.survivor_study_card_identity
            FROM identity_path path
            JOIN study_card_identity_redirects redirect
              ON redirect.historical_study_card_identity = path.identity
        )
        SELECT EXISTS (
            SELECT 1 FROM identity_path WHERE identity = NEW.projected_study_card_identity
        ) INTO presented_identity_reaches_projection;
        IF NOT presented_identity_reaches_projection THEN
            RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'latest presented evidence card does not resolve to projected card identity';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER study_scheduler_card_projections_validate_redirects
    BEFORE INSERT OR UPDATE ON study_scheduler_card_projections
    FOR EACH ROW EXECUTE FUNCTION study_validate_projection_identity_redirects();

CREATE TRIGGER study_scheduler_card_projections_set_updated_at
    BEFORE UPDATE ON study_scheduler_card_projections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
