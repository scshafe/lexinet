# lexinet — Product Requirements & Authoritative Spec

> **lexinet** ("lexicon network") is a network-accessible, agent-friendly store of technical terms for the software world. This document is the single, internally-consistent specification reconciled from the five facet designs (core-model, relationships, search, api, cli). A Mission Control skipper builds from this directly, phase by phase.

---

## 1. Vision & core insight

The **atom is the IDEA (a concept), not the word.** Many differently-configured names — a canonical name, acronyms, alternative names, outdated/deprecated names, localized labels — all map to one underlying idea. Definitions, examples, and notes hang off the idea. Ideas connect to other ideas through a typed relationship graph useful for software engineering ("term X is used in the definition of idea Y", "system A is a specialization of system-type B", part-of, depends-on, etc.).

lexinet must:

1. Make the **idea** the root entity and resolve **any** of its names to it.
2. Connect ideas via a **typed, directed relationship graph** with ergonomic reverse traversal.
3. Provide **powerful search** over names AND definitions/examples/notes, defaulting to exact/prefix/full-text, with **fuzzy as an opt-in mode**.
4. Expose a **network endpoint** (HTTP) AND ship a **scriptable, JSON-first CLI** built for agents (non-interactive, structured output, typed exit codes).

It is delivered as a **single Go 1.23.x binary** that is both the HTTP service (`lexinet serve`) and the client (every other command), following the proven **loose_ends** pattern verbatim where possible.

---

## 2. Stack & conventions (locked to the loose_ends pattern)

Grounded by reading the live `loose-ends-trial-2` source. All of these are reused, not reinvented:

- **Language/build**: one Go 1.23.x module, `cmd/lexinet/main.go` + `internal/{cli,config,server,storage,tailnet,model}` + `migrations/` (embedded via `embed.go`). Matches the sandbox toolchain (Go 1.23.4 + Postgres + ripgrep).
- **Router**: `chi/v5`. `/healthz` and `/version` registered **outside** the auth group. Everything else in an `r.Group` guarded by `requireBearer(token)` **only when** `service.auth_token` is non-empty (constant-time `subtle.ConstantTimeCompare` of `"Bearer "+token`).
- **DB**: `pgx/v5` (`pgxpool`), `application_name="lexinet"`. **`golang-migrate`** with embedded `iofs` migrations; `postgres://`→`pgx5://` DSN rewrite. `lexinet migrate` is self-bootstrapping.
- **Listeners**: `http.Server` on `service.listen_loopback` (**default `127.0.0.1:17892`** — distinct from loose_ends' `17890` and MC's `18792` to avoid collision on the shared sandbox host) and, when `service.tailnet_enabled`, **additionally** on a `tsnet.Listener` (`service.tailnet_hostname` default `lexinet`). One handler, two listeners.
- **Config**: TOML at `os.UserConfigDir()/lexinet/config.toml`, overridable with `LEXINET_CONFIG`. `Default()` / `LoadFile()` (auto-creates `0600` on first run) / `Save()` (writes `0600`) / `Set(key,value)` exactly like loose_ends. Resolution precedence **flag → env → config → built-in default**.
- **JSON envelope**: requests/responses `application/json`; decode with `DisallowUnknownFields()` (unknown field → `400 BAD_REQUEST`). Error envelope (unchanged from loose_ends):
  ```json
  { "error": { "code": "IDEA_NOT_FOUND", "message": "no idea matches that id, prefix, or name" } }
  ```
  Helpers `writeJSON(w, status, value)` / `writeError(w, status, code, message)`.
- **IDs & refs**: every PK is `uuid` (`gen_random_uuid()`, pgcrypto). Path params accept a **full UUID or a unique UUID prefix** (`ResolveID` UX: `left(lower(id::text), length($1)) = $1`, LIMIT 2 → 0=not-found, 1=ok, 2=ambiguous). **Idea refs additionally resolve by exact (case-insensitive) name or slug.**
- **Timestamps**: RFC3339 `created_at`/`updated_at` on every row; `*_at` `omitempty` when null.
- **Status codes**: `201` create, `200` read/update, `204` delete, `400/401/404/409/500` errors. `409` for unique-name / duplicate-relationship.
- **Enums in SQL**: `TEXT + CHECK (... IN (...))` — **not** native PG enums — exactly like loose_ends `status`. This is a deliberate reconciliation of the relationships facet (which proposed a native `ENUM`): TEXT+CHECK keeps extension to a one-line migration and matches the established pattern.
- **`updated_at` triggers**: a single generalized `set_updated_at()` plpgsql function + one `BEFORE UPDATE` trigger per table.
- **CLI skeleton**: cobra root with `SilenceUsage`/`SilenceErrors`, persistent `--json`/`--no-color`/`--endpoint`/`--token-file`; `SetFlagErrorFunc`; `doJSON` bearer client; `writeJSON` pretty encoder (`SetIndent("", "  ")`); `bodyFromFlag` reading stdin on `-`.

---

## 3. Data model (idea-centric)

### 3.1 Extensions — `0000_extensions.up.sql`
```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS citext;    -- case-insensitive name matching
CREATE EXTENSION IF NOT EXISTS pg_trgm;   -- fuzzy search (trigram)
```
> **Operator note (load-bearing):** `pgcrypto`/`citext` are *trusted* (a DB owner may create them). **`pg_trgm` is NOT trusted — `CREATE EXTENSION pg_trgm` requires superuser.** `IF NOT EXISTS` makes this a no-op where already present. The trial's host **Stage-0.5 bootstrap** that pre-creates `pgcrypto`/`citext` over the `cole` superuser socket **must also pre-create `pg_trgm`**, or `lexinet migrate` regresses.

### 3.2 `ideas` — the central entity — `0001_init.up.sql`
```sql
CREATE TABLE ideas (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug              CITEXT UNIQUE,         -- optional stable handle; what agents pass to `idea get <slug>`
    summary           TEXT,                  -- one-line gloss for list/search rows
    domain            TEXT,                  -- coarse free-text bucket ("protocol","pattern","tool"); controlled vocab is a later facet
    status            TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('draft','active','merged','archived')),
    canonical_name_id UUID,                  -- denormalized pointer to the canonical name row (FK added after names exists)
    merged_into_id    UUID REFERENCES ideas(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (id <> merged_into_id),
    CHECK (status <> 'merged' OR merged_into_id IS NOT NULL)
);
```

### 3.3 `names` — the many-words-to-one-idea surface (THE CORE INSIGHT)
```sql
CREATE TABLE names (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id          UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    text             CITEXT NOT NULL,        -- surface form ("REST"=="rest" for lookup)
    display          TEXT,                   -- optional preserved casing ("gRPC","OAuth")
    kind             TEXT NOT NULL
                     CHECK (kind IN ('canonical','acronym','alias','deprecated','localized')),
    expansion        TEXT,                   -- acronym only ("Representational State Transfer")
    lang_tag         TEXT,                   -- localized only (BCP-47, e.g. 'es','fr-CA')
    deprecation_note TEXT,                   -- deprecated only ("use X instead")
    sort_key         DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (kind = 'acronym'    OR expansion IS NULL),
    CHECK (kind = 'localized'  OR lang_tag IS NULL),
    CHECK (kind = 'deprecated' OR deprecation_note IS NULL)
);

-- A given surface form maps to at most ONE idea: lookup-by-name is unambiguous (v1).
CREATE UNIQUE INDEX names_text_uniq ON names (text);
-- Exactly one canonical name per idea.
CREATE UNIQUE INDEX names_one_canonical_per_idea ON names (idea_id) WHERE kind = 'canonical';
-- One localized name per (idea, language).
CREATE UNIQUE INDEX names_one_localized_per_lang ON names (idea_id, lang_tag) WHERE kind = 'localized';
CREATE INDEX names_idea_id_kind_idx ON names (idea_id, kind, sort_key);

ALTER TABLE ideas
    ADD CONSTRAINT ideas_canonical_name_fk
    FOREIGN KEY (canonical_name_id) REFERENCES names(id) ON DELETE SET NULL;
```

**Name-kind taxonomy:**

| kind | meaning | extra column |
|---|---|---|
| `canonical` | the one preferred/official name; exactly one per idea | — |
| `acronym` | abbreviation (REST, gRPC, JWT) | `expansion` |
| `alias` | alternative / synonym / brand / spelling variant | — |
| `deprecated` | outdated/superseded name, still resolvable | `deprecation_note` |
| `localized` | non-English label for the same idea (optional kind) | `lang_tag` (BCP-47) |

> **Reconciliation:** core-model named this table `lexemes`; api/cli/search called it `names`. We standardize on **`names`** (majority + loose_ends-plain). The cli facet's `alternative` kind is renamed **`alias`** (matches core-model/api/search). All five name kinds are retained; `localized` is the only one not in the api/cli facet and is supported but optional.

**Resolution semantics:**
- **By any name:** `SELECT idea_id FROM names WHERE text = $1` (citext, exact). One idea guaranteed by `names_text_uniq`.
- **Display preferred name:** join `ideas.canonical_name_id → names.id`.
- **Merged idea:** if a resolved idea has `status='merged'`, the app follows `merged_into_id` to the survivor (app-layer redirect in `ResolveIdea`/`GetIdea`).
- **Creation order (soft FK cycle):** insert idea → insert canonical name → set `ideas.canonical_name_id`, all in **one transaction** so there is never a window without a canonical name.

### 3.4 Child bodies — `0002_bodies.up.sql`
An idea can carry several definitions/examples/notes (e.g. domain-specific).
```sql
CREATE TABLE definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    register TEXT,          -- "plain","formal","rfc","implementation"
    source TEXT,            -- "RFC 7231", a URL, a book
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
    lang TEXT,              -- code-fence hint ("go","sql","bash")
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
    kind TEXT,             -- "gotcha","history","see-also","todo"
    sort_key DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX notes_idea_id_idx ON notes (idea_id, sort_key);
```

> **Reconciliation:** the search facet assumed flat `definition/examples/notes` columns directly on `ideas`. We keep the richer **child tables** (core-model + cli), so search builds its `tsvector` from an aggregate across child rows (see §5) rather than a single generated column.

### 3.5 `updated_at` triggers
```sql
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;
-- one BEFORE UPDATE trigger per table: ideas, names, definitions, examples, notes, relationships
```

---

## 4. Relationship / graph model — `0003_relationships.up.sql`

One table holds every relationship as a **directed** edge `(from_idea_id) --[kind]--> (to_idea_id)`, read **"from KIND to"**.

```sql
CREATE TABLE relationships (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_idea_id   UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    to_idea_id     UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN (
                       'is_a',                  -- specialization → general (reverse read: generalization_of)
                       'part_of',               -- component → whole       (reverse: has_part)
                       'depends_on',            -- dependent → dependency   (reverse: depended_on_by)
                       'used_in_definition_of', -- term → idea whose def uses it (reverse: defined_using)
                       'implements',            -- implementation → contract/spec
                       'supersedes',            -- newer → older            (reverse: superseded_by)
                       'synonym_of',            -- SYMMETRIC
                       'related_to'             -- SYMMETRIC
                   )),
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT relationships_no_self_edge CHECK (from_idea_id <> to_idea_id),
    -- Symmetric kinds canonicalized to ascending-UUID order so {A,B} is one row:
    CONSTRAINT relationships_symmetric_canonical_order CHECK (
        kind NOT IN ('synonym_of','related_to') OR from_idea_id < to_idea_id
    )
);
CREATE UNIQUE INDEX relationships_from_to_kind_uidx ON relationships (from_idea_id, to_idea_id, kind);
CREATE INDEX relationships_to_kind_idx   ON relationships (to_idea_id,   kind);  -- reverse lookups
CREATE INDEX relationships_from_kind_idx ON relationships (from_idea_id, kind);  -- forward fan-out
```

### Relation-kind taxonomy (reconciled)
The three facets proposed overlapping enums. The **stored** set is the eight TEXT values above. Design rules:

- **Inverses are NOT stored** as separate rows/kinds. `is_a` is read backward for generalization; `supersedes` backward for superseded-by; etc. Reverse queries hit the `(to_idea_id, kind)` index.
- **Virtual reverse names** are accepted as **input sugar** and used as **output labels**: `generalization_of` (↔`is_a`), `has_part` (↔`part_of`), `depended_on_by` (↔`depends_on`), `defined_using` (↔`used_in_definition_of`), `superseded_by` (↔`supersedes`). On input, a virtual reverse swaps from/to and stores the base kind. **One shared mapping module** (`internal/model/relation.go`) owns this table and is used by BOTH the HTTP handler and the CLI so labels can never diverge.
- **Symmetric kinds** (`synonym_of`, `related_to`): the write path swaps `from`/`to` into ascending-UUID order before insert; the CHECK enforces it loudly if a raw insert skips canonicalization. `synonym_of` is reserved for two **intentionally-distinct** ideas marked equivalent — most acronym/alt/deprecated cases are *names on one idea*, not edges.
- **Cycles** are allowed at the DB level (legitimate for `depends_on`/`part_of`). Recursive traversal uses a **visited-set + depth cap** and surfaces a `truncated` flag when the cap is hit.
- **Idempotent upsert**: re-asserting `(from,to,kind)` updates the `note`, never duplicates (unique index).

### Traversal
`GET /ideas/{ref}/graph?depth=1..5&kind=is_a,depends_on&direction=out|in|both` — bounded BFS, `depth` clamped `[1,5]` (default 1), cycle-safe visited set, symmetric kinds always traversed both ways and de-duplicated. CLI `traverse` caps `--depth` at 6; the **service enforces its own cap** so the CLI cap is not the only guard.

---

## 5. Search — `0004_search.up.sql`

Two complementary Postgres mechanisms (tsvector for text, pg_trgm for fuzzy), satisfying "full-text over names AND definitions/examples/notes, with fuzzy OPTIONAL."

### Index strategy (reconciled to child tables)
Because definitions/examples/notes are child tables, a single cross-table `GENERATED` column is impossible. Instead maintain a **per-idea weighted `tsvector`** column populated by triggers:
```sql
ALTER TABLE ideas ADD COLUMN search_doc tsvector;
CREATE INDEX ideas_search_doc_gin ON ideas USING GIN (search_doc);

-- Recompute one idea's search_doc from its names + child bodies.
CREATE OR REPLACE FUNCTION lexinet_refresh_search_doc(target UUID) RETURNS void AS $$
  UPDATE ideas i SET search_doc =
      setweight(to_tsvector('simple',  coalesce((SELECT string_agg(text,' ') FROM names WHERE idea_id=i.id AND kind='canonical'),'')), 'A') ||
      setweight(to_tsvector('simple',  coalesce((SELECT string_agg(text,' ') FROM names WHERE idea_id=i.id AND kind<>'canonical'),'')), 'B') ||
      setweight(to_tsvector('english', coalesce((SELECT string_agg(body,' ') FROM definitions WHERE idea_id=i.id),'')), 'C') ||
      setweight(to_tsvector('english', coalesce((SELECT string_agg(body,' ') FROM examples   WHERE idea_id=i.id),'')
                                     ||' '||coalesce((SELECT string_agg(body,' ') FROM notes WHERE idea_id=i.id),'')), 'D')
  WHERE i.id = target;
$$ LANGUAGE sql;
```
Triggers on `INSERT/UPDATE/DELETE` of `names`, `definitions`, `examples`, `notes` (and `ideas`) call `lexinet_refresh_search_doc(idea_id)` for the affected idea, so the vector stays correct with **no application write-path changes**. Weights: **A**=canonical name, **B**=other names, **C**=definitions, **D**=examples+notes. Names use the **`simple`** config (so acronyms like `JWT`/`API` are not stemmed away); bodies use **`english`** (stemming).

Trigram (fuzzy + prefix/substring):
```sql
CREATE INDEX names_text_trgm ON names USING GIN (text gin_trgm_ops);
-- optional, larger; only used when scope=all + fuzzy=true:
CREATE INDEX definitions_body_trgm ON definitions USING GIN (body gin_trgm_ops);
```

### Modes & ranking
| Mode | When | Mechanism | Score band |
|---|---|---|---|
| **exact** (default) | always | `names.text = lower(q)` | 1.00 (canonical +0.0, alias −0.05, deprecated −0.15) |
| **prefix** (default) | always | `names.text LIKE lower(q)||'%'` | 0.80 × len(q)/len(name) |
| **fts** (default when `scope=all`) | text search | `search_doc @@ websearch_to_tsquery('english', q)` | 0.60 × normalized `ts_rank_cd` |
| **fuzzy** (opt-in `fuzzy=true`) | typo/approx | `names.text % q` (+`definitions.body % q` when `scope=all`) | 0.50 × `similarity(name)` / 0.30 × `similarity(body)` |

Default = exact + prefix (+ FTS when `scope=all`). **Fuzzy is strictly opt-in.** Results dedupe to **one row per resolved idea** (best matching field wins) and carry *why* they matched. Tie-break: canonical match → `is_primary` → `updated_at desc`. **Deprecated-name hits resolve the LIVE idea** (the whole point), de-prioritized, with `matched_name_type='deprecated'`. `ts_headline` produces snippets for text matches.

### HTTP `GET /search` / CLI `lexinet search`
| Param / flag | Default | Meaning |
|---|---|---|
| `q` / `<query>` (required) | — | search text (empty → `400 BAD_REQUEST`) |
| `scope` / `--scope` | `names` | `names` \| `all` (include defs/examples/notes via FTS + optional fuzzy) |
| `fuzzy` / `--fuzzy` | `false` | enable trigram fuzzy |
| `threshold` / `--threshold` (`--min-score`) | `0.3` | min trigram similarity (fuzzy only) |
| `kind` / `--kind` (`--name-kind`, repeatable) | — | restrict matched name buckets |
| `limit` / `--limit` | `20` (max `100`) | page size |
| `offset` / `--offset` | `0` | pagination |

Result shape (one row per idea):
```json
{
  "query": "in-memory cache", "scope": "all", "fuzzy": true,
  "total": 2, "limit": 20, "offset": 0,
  "results": [
    { "idea": { "id": "9b2c..", "slug": "redis", "canonical_name": "Redis", "summary": "An in-memory key-value store..." },
      "score": 0.71, "matched_via": "name", "matched_field": "canonical_name",
      "matched_name": "Redis", "matched_name_type": "canonical", "match_type": "exact",
      "similarity": null, "headline": null }
  ]
}
```

---

## 6. HTTP API

### Health / version (open, no auth)
```
GET /healthz   -> 200 {"ok": true}
GET /version   -> 200 {"version": "1.0.0"}
```

### Ideas + children (auth group)
```
POST   /ideas                         create idea (+ optional inline names/definitions; all-or-nothing tx)
GET    /ideas?limit&offset&q&slug&topic list/browse (paginated: {total,limit,offset,ideas[]})
GET    /ideas/{ref}?expand=...         fetch one (names always; ?expand=definitions,examples,notes,relationships)
PATCH  /ideas/{ref}                    update summary/slug/domain/status
DELETE /ideas/{ref}                    delete (cascades names/defs/examples/notes/relationships)
POST   /ideas/{ref}/merge              fold a duplicate idea into a survivor (atomic) — body {into}

POST   /ideas/{ref}/names              add a name
GET    /ideas/{ref}/names              list names
PATCH  /names/{id}                     update kind/display/note/sort
DELETE /names/{id}                     remove a name

POST   /ideas/{ref}/definitions  GET  /ideas/{ref}/definitions  PATCH/DELETE /definitions/{id}
POST   /ideas/{ref}/examples     GET  /ideas/{ref}/examples     PATCH/DELETE /examples/{id}
POST   /ideas/{ref}/notes        GET  /ideas/{ref}/notes        PATCH/DELETE /notes/{id}
```
`{ref}` = UUID | UUID-prefix | exact citext name | slug. Ambiguous → `400 IDEA_REF_AMBIGUOUS` (response lists candidate ideas so the CLI can stay scriptable). `POST /ideas` requires ≥1 name; exactly one becomes primary/canonical (if none flagged, the first `canonical` else the first name is promoted). Setting `is_primary`/promoting a canonical name atomically demotes the prior one in one transaction.

### Topics
```
POST   /topics                         create topic {name, slug?, description?}
GET    /topics                         list topics
PATCH  /topics/{ref}                   rename/update slug/description
DELETE /topics/{ref}                   delete topic and assignments
POST   /ideas/{ref}/topics             assign topic to idea {topic}
GET    /ideas/{ref}/topics             list idea topics
DELETE /ideas/{ref}/topics/{topic}     remove assignment
GET    /topics/{ref}/ideas?limit&offset&q&slug list ideas assigned to topic
```
Topic refs accept UUID, slug, or name. Slugs normalize to lowercase dash-separated ASCII. Idea detail, list/search summaries, and topic-filtered lists include topic summaries where available.

### Acronym entry links
```
POST   /acronym-links                  upsert acronym-entry link {acronym, expansion, note?}
GET    /ideas/{ref}/acronym-links      list links touching an idea
DELETE /acronym-links/{id}             remove one link
```
An acronym name (`names.kind = acronym`) is still a surface form on one idea. An acronym entry link is for two distinct ideas: a standalone acronym idea and a standalone expansion idea. The link rejects self-pairs, is listed from either side with direction `acronym` or `expansion`, and cascades on idea delete so dangling halves are not stranded.

### Relationships
```
POST   /relationships                          {from, to, kind, note?} → idempotent upsert (canonicalized)
GET    /relationships?idea={ref}&kind&direction=out|in|both
DELETE /relationships/{id}
GET    /ideas/{ref}/graph?depth=1..5&kind&direction=out|in|both    bounded, cycle-safe BFS
```
`from`/`to` are idea refs. Errors: `from==to` → `400 RELATIONSHIP_SELF_EDGE`; unknown kind → `400 INVALID_RELATIONSHIP_KIND`; duplicate `(from,to,kind)` upserts the note; unresolved ref → `404 IDEA_NOT_FOUND`. Virtual reverse kinds accepted on input; responses relabel stored kinds to virtual names per the shared mapping module.

### Search
```
GET /search?q=...&scope&fuzzy&threshold&kind&limit&offset
```

### Error codes
| code | status | when |
|---|---|---|
| `BAD_REQUEST` | 400 | malformed body, unknown field, missing required field, empty `q` |
| `IDEA_NOT_FOUND` / `NAME_NOT_FOUND` / `RELATIONSHIP_NOT_FOUND` / `TOPIC_NOT_FOUND` / `ACRONYM_LINK_NOT_FOUND` | 404 | no match for ref/id |
| `IDEA_REF_AMBIGUOUS` | 400 | UUID-prefix or name resolves to >1 idea (candidates returned) |
| `INVALID_RELATIONSHIP_KIND` / `INVALID_NAME_KIND` | 400 | enum violation |
| `RELATIONSHIP_SELF_EDGE` | 400 | from == to |
| `NAME_EXISTS` / `SLUG_EXISTS` | 409 | unique violation |
| `UNAUTHORIZED` | 401 | missing/invalid bearer |
| `INTERNAL` | 500 | unexpected |

---

## 7. CLI (`lexinet`) — agent-first

Form: `lexinet [global-flags] <group> <verb> [args] [flags]`. Every read & mutation supports `--json`. `serve` is the service; everything else is the client.

### Command groups
- **`idea`** — `add "<canonical name>"` (`--definition <text|->`, `--example`, `--note`, `--alias` repeatable, `--domain`, `--slug`), `get <ref>` (`--expand`, `--no-relations`), `list` (`--limit`, `--offset`, `--q`, `--slug`), `edit <ref>`, `rm <ref> --yes`, `merge <ref> --into <ref> --yes`.
- **`name`** — `add <idea-ref> "<text>" --kind canonical|acronym|alias|deprecated|localized [--lang --expansion --note]`, `rm`, `promote <idea-ref> "<text>"` (make canonical, demote prior), `list <idea-ref> [--kind]`.
- **`topic`** — `add <name> [--slug --description]`, `list [--jsonl]`, `edit <topic-ref> [--slug --name --description]`, `assign <idea-ref> <topic-ref>`, `unassign <idea-ref> <topic-ref>`, `ideas <topic-ref> [--q --slug --limit --offset --jsonl]`.
- **`acronym`** — `link <acronym-idea-ref> <expansion-idea-ref> [--note]`, `links <idea-ref> [--jsonl]`, `unlink <id> --yes`.
- **`def`** — `add <idea-ref> <text|-> --kind definition|example|note [--source]`, `rm <id>`, `list <idea-ref> [--kind]`. `<text|->` reads stdin on `-` (`bodyFromFlag`).
- **`link`** — `add <from-ref> <relation> <to-ref> [--note]` (idempotent), `rm`, `list <ref> [--relation --direction out|in|both]`. `<relation>` accepts base + virtual reverse names; unknown → exit 2 with the valid list.
- **`traverse <ref>`** — `--relation` (repeatable), `--direction out|in|both`, `--depth` (max 6), `--format tree|edges|ids`, `--json`.
- **`search "<q>"`** — `--fuzzy`, `--scope names|all`, `--name-kind`, `--threshold/--min-score`, `--limit`, `--json`/`--jsonl`.
- **Plumbing** — `config show|set <key> <value>|path`, `serve [--bind --port --db --tailnet on|off --skip-migrate]`, `migrate [--db]`, `ping`, `version --json`.

`<ref>` resolution priority: slug/UUID → exact name (any kind) → error `ambiguous_ref` listing candidates. `--by id|name` forces the mode.

### Config (TOML, files-over-env for tokens)
```toml
[server]
url        = "http://127.0.0.1:17892"
token      = ""                              # discouraged inline
token_file = "~/.config/lexinet/token"       # PREFERRED 0600 file, read at call time
[search]
fuzzy = false
default_limit = 20
[display]
color = "auto"
id_length = 12
[service]
listen_loopback   = "127.0.0.1:17892"
auth_token        = ""
tailnet_enabled   = true
tailnet_hostname  = "lexinet"
tailnet_state_dir = "~/.config/lexinet/tsnet-state"
[service.database]
dsn = "postgres://lexinet@127.0.0.1:5432/lexinet?sslmode=disable"
```
**Token precedence:** `--token-file` → `LEXINET_TOKEN_FILE` → `server.token_file` → `LEXINET_TOKEN` → `server.token`. Token read at call time, never logged, `***redacted***` in `config show` (which warns if `LEXINET_TOKEN` is set). Sent as `Authorization: Bearer <token>`. **Endpoint precedence:** `--endpoint` → `LEXINET_URL` → `server.url` → default.

### Output contract (agent-friendly)
- `--json` first-class on every command (single pretty object via `SetIndent("", "  ")`); `--compact` one-line; `--jsonl` streams one object per line for `search`/`list`/`traverse`.
- **stdout = data, stderr = diagnostics.** Human mode = `<short-id> <field>` lines; `--quiet` prints only IDs for piping.
- Errors decoded from the service envelope; with `--json`, emitted as JSON on stderr with optional `candidates`.
- **Exit codes:** `0` success, `2` usage/bad-flags/invalid-enum, `3` validation rejected (HTTP 400/409), `4` ambiguous ref, `5` endpoint unreachable, `6` auth (401/403), `7` not found (404), `1` unexpected.
- **Non-interactive by default:** no command needs a TTY; destructive `rm`/`merge` **require** `--yes`/`-y` when stdin is not a TTY (fail fast exit 2, never hang).
- **Determinism:** list/search stably ordered (rank desc → created_at → id); opaque cursor with `next_cursor` (`null` when exhausted) in JSON.

### Representative non-interactive agent session
```bash
export LEXINET_URL=http://lexinet:17892
export LEXINET_TOKEN_FILE=~/.config/lexinet/token
id=$(lexinet idea add "Cyclic Redundancy Check" --alias CRC --definition - --quiet <<<'A checksum used to detect accidental data changes.')
lexinet name add "$id" CRC --kind acronym --expansion "Cyclic Redundancy Check"
lexinet name add "$id" "Cyclical Redundancy Code" --kind deprecated
lexinet link add "$id" is_a checksum
lexinet search redundancy --fuzzy --json --limit 5 | jq '.results[].idea.canonical_name'
lexinet traverse "$id" --relation is_a --depth 3 --json
```

---

## 8. Deployment

Mirrors loose_ends: `Dockerfile` (Go 1.23.4 build), `deploy/docker/{docker-compose.yml,config.toml,README.md}`, a launchd plist and/or systemd unit. Single binary, embedded migrations, self-bootstrapping `migrate`. Optional `tsnet` exposure at `lexinet.<tailnet>.ts.net:17892`; bearer `auth_token` is defense-in-depth on top of the tailnet boundary.

---

## 9. Non-goals (v1)

- **Cross-idea homonyms.** `names.text` is globally unique at v1 (one surface form → one idea), so legitimate homonyms (e.g. "GC" = garbage collection vs Google Cloud) are not supported. Relaxing this to one-to-many resolution is a deliberate future facet (it changes the resolve API/CLI contract). Confirmed v1 scope.
- **Edge weights/confidence** — relationships carry an optional free-text `note` only.
- **Controlled vocabularies** for `domain`, `notes.kind`, `definitions.register` — free-text at v1; normalization is a later facet.
- **Accent/whitespace normalization** beyond citext case-folding — the app trims input; deeper normalization is deferred. Agents passing dirty input may miss exact resolution.
- **Multi-user auth / RBAC** — single shared bearer token only.

---

## 10. Known risks carried into the build (for the skipper)

1. **`pg_trgm` superuser bootstrap** — the host Stage-0.5 step must pre-create `pg_trgm` alongside `pgcrypto`/`citext`, or `migrate` fails. Highest-priority operational risk.
2. **Soft FK cycle (idea ↔ canonical name)** — idea+canonical-name creation MUST be one transaction.
3. **Cross-table search_doc** — must be trigger-maintained (no single generated column spans tables); verify triggers fire on every child mutation.
4. **Reverse-label consistency** — the virtual-reverse mapping lives in ONE shared module used by both HTTP and CLI; do not duplicate it.
5. **Symmetric-edge canonicalization** — the write path must pre-sort `from`/`to` for `synonym_of`/`related_to`; the CHECK fails loudly otherwise (acceptable, loud-over-silent).
6. **`is_primary`/canonical demotion** must be atomic in one transaction to avoid partial-unique-index rejection.
7. **Merge atomicity** — re-point names/bodies/edges + de-dup + status flip must be all-or-nothing.
8. **Score normalization** — mixing `ts_rank_cd` (unbounded) with `similarity` (0..1) needs a defined `ts_rank_cd` normalization flag so the composite `score` is comparable across modes; tune weights against a real corpus during build.
9. **Port 17892** chosen to avoid loose_ends (17890) and MC (18792); confirm free on the sandbox host before serve.
