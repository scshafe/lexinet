CREATE TABLE relationships (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    to_idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN (
        'is_a',
        'part_of',
        'depends_on',
        'used_in_definition_of',
        'implements',
        'supersedes',
        'synonym_of',
        'related_to'
    )),
    note TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT relationships_no_self_edge CHECK (from_idea_id <> to_idea_id),
    CONSTRAINT relationships_symmetric_canonical_order CHECK (
        kind NOT IN ('synonym_of','related_to') OR from_idea_id < to_idea_id
    )
);

CREATE UNIQUE INDEX relationships_from_to_kind_uidx ON relationships (from_idea_id, to_idea_id, kind);
CREATE INDEX relationships_to_kind_idx ON relationships (to_idea_id, kind);
CREATE INDEX relationships_from_kind_idx ON relationships (from_idea_id, kind);

CREATE TRIGGER relationships_set_updated_at
    BEFORE UPDATE ON relationships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
