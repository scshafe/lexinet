CREATE TABLE topics (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug CITEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT topics_slug_not_blank CHECK (btrim(slug::text) <> ''),
    CONSTRAINT topics_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT topics_slug_format CHECK (slug::text ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);

CREATE UNIQUE INDEX topics_slug_uidx ON topics (slug);
CREATE INDEX topics_name_idx ON topics (name);

CREATE TABLE idea_topics (
    idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    topic_id UUID NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (idea_id, topic_id)
);

CREATE INDEX idea_topics_topic_id_idx ON idea_topics (topic_id, idea_id);

CREATE TABLE acronym_links (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    acronym_idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    expansion_idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    note TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT acronym_links_no_self_link CHECK (acronym_idea_id <> expansion_idea_id)
);

CREATE UNIQUE INDEX acronym_links_pair_uidx ON acronym_links (acronym_idea_id, expansion_idea_id);
CREATE INDEX acronym_links_acronym_idea_id_idx ON acronym_links (acronym_idea_id);
CREATE INDEX acronym_links_expansion_idea_id_idx ON acronym_links (expansion_idea_id);

CREATE TRIGGER topics_set_updated_at
    BEFORE UPDATE ON topics
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER idea_topics_set_updated_at
    BEFORE UPDATE ON idea_topics
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER acronym_links_set_updated_at
    BEFORE UPDATE ON acronym_links
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
