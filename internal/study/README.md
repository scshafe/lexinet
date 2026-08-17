# Study scheduling core

This package owns pure, versioned finite-cycle selection. It does not read a
database, clock, random source, environment variable, registry, or network.
The composition layer must provide a complete `StudySchedulingSnapshot`,
including the server planning time and deterministic seed.

## Canonical seams

- `StudyScheduler` is the `@extension-point study_scheduler`. A scheduler gets
  one immutable snapshot and returns one complete plan or a structured error.
- `variety_first_v1` / version `1` is the active policy. It orders exact rating
  ranks `again`, `hard`, unseen/unrated, `good`, `easy`; then uses ascending
  latest-presented server ingestion ordinals with nulls first. Only null/null
  comparisons consult the seeded tie digest. A digest collision falls back to
  the lowercase card UUID.
- `shuffle_bag_v1` / version `1` is a rating-independent deterministic
  comparison fixture. It is not a production fallback.
- `internal/study/digest` is the `@canonical study_digest` leaf. Every digest
  starts with `lexinet-study-digest-v1\x00`, then frames its purpose and every
  ordered part with a four-byte big-endian length. Configuration, tie,
  card-content, card-source, and corpus hashes have distinct purposes.

Policy constructors bind the exact name, version, and ordered configuration
parts before planning. A zero-value scheduler fails closed. Plan errors contain
only allowlisted operation, field, and reason metadata; card identities,
profile identities, source content, and seeds are never copied into error
context. `StudyMaximumSchedulingCards` is the exported current per-plan work
bound; composition must reject larger snapshots rather than attempting an
unbounded map allocation or sort.

A scheduler result is a pure proposal, not persistence authority. Composition
must invoke schedulers through the materializer's authority-owning deep-copy
and output-validation boundary. Direct extension calls may be useful in tests
and shadow evaluation, but their output must never be written as an issued
cycle without that materializer validation.

`CycleMaterializer` is the `@canonical study_cycle_materializer`. It owns the
storage-neutral composition of one locked eligible corpus, accepted-prefix
projection rebuild, merge resolution, predecessor boundary, canonical plan,
frozen item bytes, and root/successor persistence proposal. It validates the
configured `variety_first_v1` plan independently before persistence. Durable
creation receipts are resolved before mutable corpus, projection, redirect,
or scheduler reads; replay returns the issued plan without inventing a
historical scheduling snapshot. Empty locked corpora skip prefix rebuild and
scheduling and persist or converge a `waiting_for_corpus` sentinel. Card,
accepted-prefix, digest, and shadow payload bounds fail with structured errors
before unbounded work.

The storage package's `Repository.MaterializeStudyCycle` is the production
transaction wrapper. It exposes no staged result until commit succeeds.
`StudyShadowRunner` is a post-commit-only, non-authoritative evaluation seam:
it receives private snapshot/plan copies and has no store, callback, or plan
write capability. Callers may invoke it only for a newly created result whose
exact scheduling snapshot is available after an independent committed read.

## Boundary behavior

When the merge-canonicalized predecessor tail would be the new head,
`variety_first_v1` left-rotates the maximal leading same-rank block exactly
once. Only the new head records `boundary_repeat_rotated`. If the leading rank
has no alternative, strict rank priority wins and the unchanged head records
`boundary_repeat_due_to_priority`. No boundary adjustment crosses a rank.

## Extension checklist

A future scheduler must:

1. use a new immutable name/version and explicit ordered configuration parts;
2. implement only `Plan(StudySchedulingSnapshot)` and remain deterministic for
   the same snapshot;
3. return every eligible identity exactly once or return a structured error;
4. preserve per-item reviewable selection provenance; and
5. be injected explicitly by the composition root—there is no ambient policy
   registry or silent fallback.

Canonical event projection/rebuild, transactional materialization composition,
and the authority-isolated shadow runner are implemented in adjacent files.
They do not add database, clock, UUID, seed, or ambient configuration authority
to the scheduler. API/service routing remains outside this package.
