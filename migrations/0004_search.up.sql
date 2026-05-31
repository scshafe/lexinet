ALTER TABLE ideas ADD COLUMN search_doc tsvector NOT NULL DEFAULT ''::tsvector;

CREATE INDEX ideas_search_doc_gin ON ideas USING GIN (search_doc);
CREATE INDEX names_text_trgm ON names USING GIN ((text::text) gin_trgm_ops);
CREATE INDEX definitions_body_trgm ON definitions USING GIN (body gin_trgm_ops);
CREATE INDEX examples_body_trgm ON examples USING GIN (body gin_trgm_ops);
CREATE INDEX notes_body_trgm ON notes USING GIN (body gin_trgm_ops);

CREATE OR REPLACE FUNCTION lexinet_build_search_doc(target UUID) RETURNS tsvector AS $$
    SELECT
        setweight(to_tsvector('simple', coalesce((
            SELECT string_agg(coalesce(display, text::text) || ' ' || text::text, ' ')
            FROM names
            WHERE idea_id = target AND kind = 'canonical'
        ), '')), 'A') ||
        setweight(to_tsvector('simple', coalesce((
            SELECT string_agg(coalesce(display, text::text) || ' ' || text::text, ' ')
            FROM names
            WHERE idea_id = target AND kind <> 'canonical'
        ), '')), 'B') ||
        setweight(to_tsvector('english', coalesce((
            SELECT string_agg(body, ' ')
            FROM definitions
            WHERE idea_id = target
        ), '')), 'C') ||
        setweight(to_tsvector('english', coalesce((
            SELECT string_agg(body, ' ')
            FROM examples
            WHERE idea_id = target
        ), '') || ' ' || coalesce((
            SELECT string_agg(body, ' ')
            FROM notes
            WHERE idea_id = target
        ), '')), 'D');
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION lexinet_refresh_search_doc(target UUID) RETURNS void AS $$
    UPDATE ideas
    SET search_doc = lexinet_build_search_doc(target)
    WHERE id = target;
$$ LANGUAGE sql;

CREATE OR REPLACE FUNCTION lexinet_refresh_search_doc_for_idea() RETURNS trigger AS $$
BEGIN
    NEW.search_doc = lexinet_build_search_doc(NEW.id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION lexinet_refresh_search_doc_for_child() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM lexinet_refresh_search_doc(OLD.idea_id);
        RETURN OLD;
    END IF;

    PERFORM lexinet_refresh_search_doc(NEW.idea_id);
    IF TG_OP = 'UPDATE' AND OLD.idea_id <> NEW.idea_id THEN
        PERFORM lexinet_refresh_search_doc(OLD.idea_id);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ideas_refresh_search_doc
    BEFORE INSERT OR UPDATE ON ideas
    FOR EACH ROW EXECUTE FUNCTION lexinet_refresh_search_doc_for_idea();

CREATE TRIGGER names_refresh_search_doc
    AFTER INSERT OR UPDATE OR DELETE ON names
    FOR EACH ROW EXECUTE FUNCTION lexinet_refresh_search_doc_for_child();

CREATE TRIGGER definitions_refresh_search_doc
    AFTER INSERT OR UPDATE OR DELETE ON definitions
    FOR EACH ROW EXECUTE FUNCTION lexinet_refresh_search_doc_for_child();

CREATE TRIGGER examples_refresh_search_doc
    AFTER INSERT OR UPDATE OR DELETE ON examples
    FOR EACH ROW EXECUTE FUNCTION lexinet_refresh_search_doc_for_child();

CREATE TRIGGER notes_refresh_search_doc
    AFTER INSERT OR UPDATE OR DELETE ON notes
    FOR EACH ROW EXECUTE FUNCTION lexinet_refresh_search_doc_for_child();

SELECT lexinet_refresh_search_doc(id) FROM ideas;
