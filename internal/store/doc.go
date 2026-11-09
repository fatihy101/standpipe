// Package store is the gateway's durable storage: the SQLite schema and
// migrations, the idempotency reservation protocol, the append-only trigger
// log, and the inventory reads.
//
// No other package writes SQL; Store is the contract the rest of the gateway
// calls. docs/architecture.md describes where the store sits, and
// docs/reference/hash-scopes.md lists the trigger log's hash scopes.
//
// Four properties this package exists to hold:
//
//   - A reservation is taken before any dispatch, inside BEGIN IMMEDIATE, and
//     the trigger event is written in that same transaction. A dispatch with
//     no record is worse than no dispatch.
//   - The idempotency key is (caller_name, idempotency_key). Rotating a
//     caller's token does not break idempotency.
//   - The log is append-only. run_link corrections use superseded_by, never an
//     UPDATE of the recorded content.
//   - Single writer, separate read pool. A coverage scan never sits in front
//     of a dispatch.
package store
