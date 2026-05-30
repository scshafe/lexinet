CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE ideas (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug              CITEXT UNIQUE,
    summary           TEXT,
    domain            TEXT,
    status            TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('draft','active','merged','archived')),
    canonical_name_id UUID,
    merged_into_id    UUID REFERENCES ideas(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (id <> merged_into_id),
    CHECK (status <> 'merged' OR merged_into_id IS NOT NULL)
);

CREATE TABLE names (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id          UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    text             CITEXT NOT NULL,
    display          TEXT,
    kind             TEXT NOT NULL
                     CHECK (kind IN ('canonical','acronym','alias','deprecated','localized')),
    expansion        TEXT,
    lang_tag         TEXT,
    deprecation_note TEXT,
    sort_key         DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (kind = 'acronym'    OR expansion IS NULL),
    CHECK (kind = 'localized'  OR lang_tag IS NULL),
    CHECK (kind = 'deprecated' OR deprecation_note IS NULL)
);

CREATE UNIQUE INDEX names_text_uniq ON names (text);
CREATE UNIQUE INDEX names_one_canonical_per_idea ON names (idea_id) WHERE kind = 'canonical';
CREATE UNIQUE INDEX names_one_localized_per_lang ON names (idea_id, lang_tag) WHERE kind = 'localized';
CREATE INDEX names_idea_id_kind_idx ON names (idea_id, kind, sort_key);

ALTER TABLE ideas
    ADD CONSTRAINT ideas_canonical_name_fk
    FOREIGN KEY (canonical_name_id) REFERENCES names(id) ON DELETE SET NULL;

CREATE TRIGGER ideas_set_updated_at
    BEFORE UPDATE ON ideas
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER names_set_updated_at
    BEFORE UPDATE ON names
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
