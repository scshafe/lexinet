-- pgcrypto and citext are trusted extensions; pg_trgm requires superuser on
-- PostgreSQL versions where it is not trusted. The host bootstrap pre-creates
-- pg_trgm for the lexinet trial, so this is a no-op there.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
