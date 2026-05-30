DROP TRIGGER IF EXISTS names_set_updated_at ON names;
DROP TRIGGER IF EXISTS ideas_set_updated_at ON ideas;
ALTER TABLE IF EXISTS ideas DROP CONSTRAINT IF EXISTS ideas_canonical_name_fk;
DROP TABLE IF EXISTS names;
DROP TABLE IF EXISTS ideas;
DROP FUNCTION IF EXISTS set_updated_at();
