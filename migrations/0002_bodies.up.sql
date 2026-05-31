CREATE TABLE definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    register TEXT,
    source TEXT,
    is_primary BOOLEAN NOT NULL DEFAULT false,
    sort_key DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX definitions_one_primary_per_idea ON definitions (idea_id) WHERE is_primary;
CREATE INDEX definitions_idea_id_idx ON definitions (idea_id, sort_key);

CREATE TABLE examples (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    lang TEXT,
    caption TEXT,
    sort_key DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX examples_idea_id_idx ON examples (idea_id, sort_key);

CREATE TABLE notes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    kind TEXT,
    sort_key DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX notes_idea_id_idx ON notes (idea_id, sort_key);

CREATE TRIGGER definitions_set_updated_at
    BEFORE UPDATE ON definitions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER examples_set_updated_at
    BEFORE UPDATE ON examples
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER notes_set_updated_at
    BEFORE UPDATE ON notes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
