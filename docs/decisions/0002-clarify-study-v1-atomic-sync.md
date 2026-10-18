<!-- @canonical study-v1-atomic-sync-clarification -->

# Decision 0002: Clarify `study.v1` Atomic Sync Boundaries

**Status:** accepted

**Date:** 2026-08-16

## Context

The accepted Step 14 review defined sync as one atomic batch, and Decision
0001 froze `study.v1` with the same promise: the sync operation "atomically
adjudicates a contiguous client-stream batch." The original golden response
showed accepted, semantic-duplicate, and terminally rejected receipts together,
but did not identify which receipts were immutable replay evidence and which
receipt belonged to the newly adjudicated suffix. Read in isolation, that
example could be mistaken for partial commitment of a new batch.

The idempotency wording was also broader than the intended guarantee. Stable
study evidence and issued content must replay identically, but a session is a
live authoritative resource whose lifecycle may advance after the original
sync. Whole-response byte stability would force stale lifecycle reporting or
contradict that authority.

## Decision

Clarify the already-approved atomic rule as follows:

1. A sync request is reconciled as an immutable, terminally adjudicated replay
   prefix followed by at most one new contiguous suffix. Either part may be
   empty.
2. Replayed sequences must match their original event identities and normalized
   payload digests and return their original receipts.
3. A valid new suffix commits atomically and may contain both `accepted` and
   `semantic_duplicate` dispositions.
4. If any new event has a terminal domain failure, every event in the new
   suffix receives `terminal_rejected` with the first failure code. The server
   commits those terminal receipts and consumes their stream positions, but
   commits no new-suffix canonical event and allocates no new-suffix
   per-profile ingestion ordinal. The replay prefix is unaffected.
5. A retryable request failure adjudicates no new sequence.
6. Replaying an identical `sync_id` preserves the original event receipts,
   flush receipt, and immutable issued page bytes. The live session envelope
   may report current authoritative lifecycle or state, so the complete sync
   response has no byte-stability guarantee.

The contract manifest now labels replay boundaries on sync examples, and the
validator executes the successful mixed-suffix and grouped terminal-suffix
rules. OpenAPI vendor metadata makes the same partition and replay guarantees
machine-readable.

## Compatibility

This is a clarification inside `study.v1`, not a new semantic or protocol
version. It resolves the ambiguity in the previously accepted word "atomic"
and rejects an interpretation—partial commitment of a failing new suffix—that
Step 14 never approved. No request or response member, required field, enum,
required-semantic identifier, schema version, or protocol version changes.
Existing conforming clients already retain terminal receipts, treat issued
pages as immutable, and consume current server session state.

The existing mixed golden response remains conforming specifically because
sequences 1–4 are its immutable replay prefix and sequence 5 is its sole new
suffix event. Manifest metadata and executable validation now make that
otherwise invisible boundary explicit. Its established exported path remains
stable for consumers whose provenance tooling treats paths as identities.

## Consequences

- Implementations have one transaction boundary for all newly adjudicated
  sequences and cannot expose partially accepted new suffixes.
- Previously terminal replay evidence remains stable even when a later suffix
  fails.
- Clients may trust receipt, flush, and issued-page replay without treating a
  later authoritative lifecycle transition as an idempotency violation.
- No coordinated `study.v2` cutover is required for this clarification.
