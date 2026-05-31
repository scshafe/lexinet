DROP TRIGGER IF EXISTS notes_refresh_search_doc ON notes;
DROP TRIGGER IF EXISTS examples_refresh_search_doc ON examples;
DROP TRIGGER IF EXISTS definitions_refresh_search_doc ON definitions;
DROP TRIGGER IF EXISTS names_refresh_search_doc ON names;
DROP TRIGGER IF EXISTS ideas_refresh_search_doc ON ideas;

DROP FUNCTION IF EXISTS lexinet_refresh_search_doc_for_child();
DROP FUNCTION IF EXISTS lexinet_refresh_search_doc_for_idea();
DROP FUNCTION IF EXISTS lexinet_refresh_search_doc(UUID);
DROP FUNCTION IF EXISTS lexinet_build_search_doc(UUID);

DROP INDEX IF EXISTS notes_body_trgm;
DROP INDEX IF EXISTS examples_body_trgm;
DROP INDEX IF EXISTS definitions_body_trgm;
DROP INDEX IF EXISTS names_text_trgm;
DROP INDEX IF EXISTS ideas_search_doc_gin;

ALTER TABLE ideas DROP COLUMN IF EXISTS search_doc;
