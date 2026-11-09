// Package shadow is Phase 1's shadow ledger: the drill day axis, the executor's
// invocation records, and the audit queries that must come back empty.
//
// # It is a SEPARATE DATABASE, and that is the point
//
// This package writes shadow.db (storage.shadow_sqlite_path, default
// /var/lib/standpipe/shadow.db). It never touches gateway.db, for two
// independent reasons that both still bind:
//
//   - gateway.db's event_type enum is a closed set hand-written in five places,
//     four of which fail silently. A shadow run is not a trigger.
//   - gateway.db's schema_version and migration list belong to Phase 0.
//     Phase 1's tables there would make every Phase 1 schema change a change
//     to Phase 0's schema.
//
// The price was paid and is named: there is NO JOIN with trigger_events. A
// shadow run is tied to a trigger by FIELDS — repo, workflow_path, commit_sha —
// never by a foreign key.
//
// # Why this is a separate PACKAGE too, and not internal/store
//
// internal/store's migration runner is deliberately a singleton: one embedded
// migrations directory, one package-level list, one LatestSchemaVersion. That
// is what lets Open refuse to start on a version mismatch without being told
// which list to check. Parameterising it would turn a compile-time fact into a
// runtime argument, and it would put shadow.db's schema evolution inside a
// package whose doc, errors and tests all say "gateway" — re-coupling exactly
// what the separate file decoupled.
//
// So the migration runner, the DSN and writeTx are deliberately DUPLICATED
// here, not shared, and so is the driver-quirk knowledge: this package reads
// modernc.org/sqlite's EXTENDED result codes itself (record.go) rather than
// calling internal/store's IsBusy / IsDiskFull / IsConstraint, which answer on
// the PRIMARY code and cannot tell a CHECK from a foreign key from a trigger's
// RAISE(ABORT) — a distinction this package needs in three call sites. Nothing
// is imported from internal/store. The cost of the duplication is named: a
// repair to the pragma or transaction discipline in one package does not reach
// the other, so each has to be repaired by hand.
//
// # What this package holds
//
//   - A day row exists for EVERY day of the window. The absence of a row is a
//     recording defect, not "the drill ran".
//   - The invocation row is written in TWO PHASES: OpenInvocation before the
//     process, with outcome NULL, and CloseInvocation after. A crashed process
//     cannot emit its own counter, so the trace has to be a row written before
//     the work. An open row is a STATE, not a gap: it enters A and is paid out
//     of X. The residual is named — a crash during PREFLIGHT still leaves no
//     row, because the row's own columns are partly preflight's output.
//   - purpose has no default, is written before the run, and cannot be
//     rewritten. Since the row now exists before the process that sentence is
//     STRUCTURAL: purpose is not among the five columns phase 2 may write.
//   - Append-only is NARROWED, not lifted. exec_invocation_write_once permits
//     exactly finished_at, outcome, refusal_reason, run_conclusion and
//     comparison; exec_invocation_no_reverdict refuses a second verdict. Every
//     other exec_* table is append-only outright.
//   - comparison holds the gate's numerator M, which CANNOT be derived here:
//     the ledger has no GitHub-side conclusion anywhere, by design. A
//     completed drill run may not close without it and nothing else may carry
//     it. A mismatch's EXPLANATION still has no shape, so the gate's third
//     condition remains unauditable from this ledger (an open question). The
//     TYPE is exec.Comparison since 2026-08-23: it is the
//     VALIDATOR's output, not this ledger's vocabulary, and this package
//     transcribes it into a CHECK rather than defining it. The import direction
//     stays one-way — internal/exec depends on nothing of ours.
//   - Since 0003 it also holds the CANDIDATE axis: every
//     run the selector LOOKED AT, the paired attempts planned for the selected
//     ones, and the timezone the day key was drawn in. Three things there are
//     decisions rather than shapes. drill_day_zone has NO foreign key onto
//     drill_day, because a record that EXPLAINS a key cannot be that key's
//     child and must outlive it. `disposition` is closed at seven values with
//     no member naming OUR inability to run something — the absence is what
//     enforces "excluding by not-selecting is forbidden", since the excluded
//     class then has to be recorded, attempted and paid out of X. And a partial
//     UNIQUE index makes selection once-ever, which is what makes the promotion
//     gate's "20 DISTINCT runs" true of a query that counts rows.
//   - THE GATE'S NUMBERS ARE NEVER READ THROUGH THE CANDIDATE AXIS. It was
//     measured, not argued: on one ledger with a single unclaimed mismatched
//     `online` run, exec_invocation says 80.0% and the same query INNER JOINed
//     through drill_attempt says 100.0%. The join drops the unmatched row in
//     silence and the gate passes. A/D/M/X come from exec_invocation; the
//     candidate axis is a CROSS-CHECK, and the two readings agreeing is exactly
//     DrillRunsWithoutACandidate coming back empty.
//   - Single writer, separate read pool. The audit scans never sit in front
//     of a record write.
//   - The ordering rule — the audits run BEFORE retention — is enforced by the
//     API SHAPE, because no CHECK can hold it: a table cannot know when the
//     statement that deletes it runs. The purges are unexported and Retain is
//     the only exported way to delete anything; it runs the audits on the
//     SAME transaction as the DELETEs and refuses with the offending rows. It
//     also refuses a cutoff inside the gate's own window, derived from the
//     ledger's MAX(day) and GateWindowDays rather than chosen. The rule was not
//     hypothetical: PurgeDays broke it by deleting a childless `ran` day, which
//     turned the second audit green by deletion.
//
// The drill rules and the candidate axis are summarised in
// docs/reference/drill-rules.md; the ledger's place in the system is in
// docs/architecture.md.
package shadow
