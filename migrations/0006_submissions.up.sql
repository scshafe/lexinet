CREATE TABLE submissions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term TEXT NOT NULL,
    context TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    idea_id UUID REFERENCES ideas(id) ON DELETE SET NULL,
    note TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT submissions_term_not_blank CHECK (btrim(term) <> ''),
    CONSTRAINT submissions_status_valid CHECK (status IN ('pending', 'enriched', 'failed'))
);

CREATE INDEX submissions_queue_idx ON submissions (status, created_at, id);
CREATE INDEX submissions_idea_id_idx ON submissions (idea_id);

CREATE TRIGGER submissions_set_updated_at
    BEFORE UPDATE ON submissions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
