-- 0002_two_phase_and_comparison — the run axis becomes two-phase and records
-- the comparison.
--
-- Four changes, one transaction:
--
--   1. exec_invocation becomes TWO-PHASE. The row is written BEFORE the
--      process, with outcome IS NULL, and a second write closes it. A crashed
--      process cannot emit its own counter, so the trace has to be a row
--      written before the work.
--   2. `comparison` ('matched' | 'mismatched') is added. The validator's
--      vocabulary is closed at those two values (exec.Comparison), and the
--      column transcribes it. M is a RECORD, not a derivation: this ledger
--      holds no GitHub-side conclusion anywhere, and that is exactly the price
--      the separate file was paid with.
--   3. exec_image.digest is renamed source_digest. Under `create_commit` the
--      configured source digest and the locally recomputed one already differ,
--      and a name that cannot be read invites the wrong value silently.
--   4. The append-only trigger is NARROWED, not lifted: exactly five phase-2
--      columns may be written, and a closed row may never take a second
--      verdict.
--
-- exec_step_observation is NOT touched. Its primary-key repair was expected to
-- ride along in this migration, but 0001 already shipped the key as
-- (invocation_id, job_id, leg_key, step_id) with the declared ordinal
-- surviving as a partial unique index. The gap was between the original design
-- and this package, not inside this package; there is nothing left to fold in.
-- Verified against sqlite_master before this file was written.
--
-- ---------------------------------------------------------------------------
-- HOW THIS REBUILD IS ORDERED, AND WHY IT IS NOT THE MANUAL'S ORDER
--
-- SQLite's documented 12-step rebuild opens with `PRAGMA foreign_keys = OFF`
-- OUTSIDE a transaction. Neither half is available here: foreign_keys(ON) is a
-- CONNECTION pragma set in the DSN, and applyMigration runs this whole file
-- inside BEGIN IMMEDIATE — where `PRAGMA foreign_keys` is a SILENT NO-OP. It
-- does not error, it does nothing, which is the worst of the three possible
-- behaviours.
--
-- `PRAGMA defer_foreign_keys = ON` is the one that works inside a transaction,
-- and it resets itself at COMMIT or ROLLBACK so a migration cannot leak a
-- relaxed setting into the daemon's connection. VERIFIED, and it is necessary
-- but NOT SUFFICIENT — this is the finding that shaped the file:
--
--   `defer_foreign_keys` DEFERS A COUNTER, IT DOES NOT RE-CHECK AT COMMIT.
--
-- `DROP TABLE exec_invocation` performs an implicit DELETE FROM with FKs
-- enabled, which increments the deferred counter ONCE PER CHILD ROW in the six
-- referencing tables. `ALTER TABLE ... RENAME TO exec_invocation` then puts an
-- identical parent back — and DOES NOT DECREMENT THE COUNTER. COMMIT fails
-- with SQLITE_CONSTRAINT_FOREIGNKEY (787) on a database that is, at that
-- moment, entirely consistent: run on the rolled-back file, PRAGMA
-- foreign_key_check returns ZERO ROWS. Driven on a populated ledger, both
-- halves. gateway.db's 0003 did not hit this because trigger_events is its OWN
-- parent: dropping it deleted the children in the same statement, and the
-- increments and decrements cancelled. exec_invocation's children are in OTHER
-- tables, and nothing cancels them.
--
-- What DOES decrement the counter is inserting the parent rows back while the
-- children are watching. So the order here is:
--
--   carry the rows into a TEMP table  ->  DROP  ->  RENAME  ->  RE-INSERT
--
-- not the manual's copy-then-drop. The parent INSERT in the last step is what
-- brings the deferred counter to zero, and COMMIT then succeeds. Two other
-- procedures were driven and REJECTED:
--
--   - copy-then-drop-then-rename (the manual's order): COMMIT fails, 787.
--   - `PRAGMA legacy_alter_table = ON` around a rename-the-old-table-away
--     step: it does NOT prevent the reference rewrite, because rewriting
--     REFERENCES clauses on rename is governed by `foreign_keys`, not by
--     legacy_alter_table — the children came out pointing at
--     `REFERENCES "exec_invocation_old"` and COMMIT failed with three genuine
--     foreign_key_check rows. It also LEAKS: legacy_alter_table survives a
--     ROLLBACK, unlike defer_foreign_keys, so a failed migration would have
--     left the writer connection in legacy rename mode.
--
-- The CHECK constraints DO apply to the re-insert, and that is deliberate: a
-- pre-existing row the new schema cannot hold ABORTS the whole file, rolling
-- back to schema 1 with every row intact and user_version unchanged. Driven,
-- including the re-run afterwards — the TEMP table does not leak across the
-- rollback onto the pooled writer connection.
--
-- ONE COST OF THAT ABORT IS NAMED HERE BECAUSE IT IS NOT OBVIOUS: under schema
-- 1 the WHOLE row is append-only (exec_invocation_append_only), so the operator
-- cannot repair a blocking row with an UPDATE. It comes back as
-- SQLITE_CONSTRAINT_TRIGGER. The repair is DELETE plus re-INSERT, and it is
-- outside this ledger's guarantees. Also driven.
-- ---------------------------------------------------------------------------
PRAGMA defer_foreign_keys = ON;

-- ---------------------------------------------------------------------------
-- 3. exec_image.digest -> source_digest
--
-- THE CONFIGURED SOURCE DIGEST — the value identity was asserted AGAINST — and
-- not the digest recomputed locally. Under `create_commit` the two already
-- differ, because `docker commit` recomputes `Id` and `RepoDigests`. Writing
-- the local value here would turn the record from "what we ran" into "what we
-- produced", the opposite of what a provenance record is for.
--
-- RENAME COLUMN, not a rebuild: no constraint changes, and exec_image's
-- append-only trigger names no columns.
-- ---------------------------------------------------------------------------
ALTER TABLE exec_image RENAME COLUMN digest TO source_digest;

-- ---------------------------------------------------------------------------
-- 1., 2. and 4. exec_invocation, rebuilt.
--
-- SQLite cannot drop a NOT NULL in place, and five columns lose theirs:
-- outcome (phase 1 writes the row before there is a verdict) and the four
-- preflight columns (a `refused` row written at preflight cannot carry values
-- preflight never produced — a run refused with `normalisation_failed` has no
-- normalised text, and the sha of a text that does not exist cannot be
-- written).
-- ---------------------------------------------------------------------------
-- The carry table. It is TEMP, so it lives on this connection only and is
-- rolled back with everything else if the file aborts; the IF EXISTS makes a
-- re-run after an abort safe regardless.
DROP TABLE IF EXISTS temp.shadow_0002_carry;
CREATE TEMP TABLE shadow_0002_carry AS
    SELECT rowid AS carried_rowid, * FROM main.exec_invocation;

CREATE TABLE main.exec_invocation_new (
    invocation_id         TEXT    PRIMARY KEY,

    -- WHICH SCHEMA WROTE THIS ROW, and since this migration that is
    -- load-bearing rather than decorative — see the grandfather clause on
    -- exec_invocation_drill_needs_comparison below. The DEFAULT moves to 2 and
    -- exec_invocation_schema_floor refuses any INSERT that claims less, so the
    -- exemption cannot be claimed by a row written from now on.
    schema_version        INTEGER NOT NULL DEFAULT 2
                          CONSTRAINT exec_invocation_schema_version_positive
                          CHECK (schema_version >= 1),

    backend_name          TEXT    NOT NULL,
    executor_kind         TEXT    NOT NULL,          -- 'act'
    executor_version      TEXT    NOT NULL,          -- the version OBSERVED
    executor_pin_ok       INTEGER NOT NULL
                          CONSTRAINT exec_invocation_pin_ok_boolean
                          CHECK (executor_pin_ok IN (0, 1)),

    -- Phase 1 stamps this. It is the only clock the row has until it closes.
    started_at            INTEGER NOT NULL,          -- unix-millis

    -- NULL now means TWO different things and the pair (finished_at, outcome)
    -- separates them:
    --   outcome IS NULL     — the row is OPEN. The harness never came back:
    --                         it crashed, was killed, or is still running.
    --   outcome IS NOT NULL — the process never finished under a verdict that
    --                         still bounded it, i.e. deadline_exceeded.
    finished_at           INTEGER,

    repo                  TEXT    NOT NULL,
    workflow_path         TEXT    NOT NULL,
    commit_sha            TEXT    NOT NULL
                          CONSTRAINT exec_invocation_commit_sha_len
                          CHECK (length(commit_sha) = 40),
    event_name            TEXT    NOT NULL,
    event_payload_sha256  TEXT    NOT NULL,          -- the trace of an input-less replay

    -- 'drill' = the gate's candidate (a GitHub side EXISTS).
    -- 'outage' = work actually carried during an outage (no GitHub side, and
    --            there never will be one).
    -- Since this migration the sentence "written before the run" is STRUCTURAL
    -- and not merely a decision: the row exists before the process, and
    -- `purpose` is not one of the five columns phase 2 may write.
    purpose               TEXT    NOT NULL
                          CONSTRAINT exec_invocation_purpose_enum
                          CHECK (purpose IN ('drill', 'outage')),

    drill_day             TEXT    NOT NULL REFERENCES drill_day(day),

    supply_mode           TEXT    NOT NULL
                          CONSTRAINT exec_invocation_supply_mode_enum
                          CHECK (supply_mode IN ('online', 'offline')),

    -- PREFLIGHT'S OUTPUT — nullable, and bound below by
    -- exec_invocation_preflight_columns_present. If the process was invoked,
    -- all four are written. Only a `refused` row may stand without them.
    argv_sha256           TEXT,
    workflow_norm_sha256  TEXT,                      -- the text HANDED to the engine
    workflow_norm_applied INTEGER
                          CONSTRAINT exec_invocation_norm_applied_boolean
                          CHECK (workflow_norm_applied IS NULL OR
                                 workflow_norm_applied IN (0, 1)),
    workdir_clean         INTEGER
                          CONSTRAINT exec_invocation_workdir_clean_boolean
                          CHECK (workdir_clean IS NULL OR workdir_clean IN (0, 1)),
    workdir_commit_count  INTEGER,                   -- keeps short-history checkouts visible
    workdir_shallow       INTEGER
                          CONSTRAINT exec_invocation_workdir_shallow_boolean
                          CHECK (workdir_shallow IS NULL OR workdir_shallow IN (0, 1)),

    -- PHASE 2. NULL is not a gap, it is a STATE: the harness reached no
    -- verdict. Such a row still enters A and is paid out of X — a run that
    -- crashed halfway is a run that was ATTEMPTED, and a row that is not
    -- written falls out of both budgets in silence.
    outcome               TEXT
                          CONSTRAINT exec_invocation_outcome_enum
                          CHECK (outcome IS NULL OR
                                 outcome IN ('completed', 'refused', 'harness_error',
                                             'unmeasurable', 'deadline_exceeded')),
    refusal_reason        TEXT
                          CONSTRAINT exec_invocation_refusal_reason_enum
                          CHECK (refusal_reason IS NULL OR refusal_reason IN (
                                 'image_unverified', 'platform_unmapped',
                                 'executor_version_mismatch', 'token_absent',
                                 'workdir_dirty', 'normalisation_failed',
                                 'local_repository_unresolved',
                                 'event_payload_incomplete', 'unsupported_platform_line',
                                 'job_without_result', 'step_set_unavailable',
                                 'plan_disagrees_with_github', 'job_identity_unmapped',
                                 'conclusion_unmappable')),
    run_conclusion        TEXT
                          CONSTRAINT exec_invocation_run_conclusion_enum
                          CHECK (run_conclusion IS NULL OR run_conclusion IN (
                                 'success', 'failure', 'cancelled', 'skipped',
                                 'timed_out', 'action_required', 'neutral', 'stale')),

    -- THE GATE'S NUMERATOR (M) IS READ HERE AND CANNOT BE DERIVED. This ledger
    -- holds GitHub's `conclusion` NOWHERE — the separate file was paid for
    -- with exactly that — so 'matched' is a RECORD, not a computation: the
    -- validator's verdict. What enters the ledger is OUR judgement, not
    -- GitHub's data, so the rule that shadow.db holds no GitHub-side data is
    -- not breached.
    --
    -- WHAT THIS COLUMN DOES NOT DO: a mismatch's EXPLANATION still has no shape
    -- here, so the gate's THIRD condition ("zero UNEXPLAINED mismatches in the
    -- last 7 drill days") is still not auditable from this table. That is an
    -- open question and it is deliberately not invented here. The column made
    -- the second condition auditable and not the third.
    comparison            TEXT
                          CONSTRAINT exec_invocation_comparison_enum
                          CHECK (comparison IS NULL OR comparison IN ('matched', 'mismatched')),

    -- A conclusion is derived only if the validator passed.
    CONSTRAINT exec_invocation_completed_needs_conclusion
    CHECK (outcome <> 'completed' OR run_conclusion IS NOT NULL),
    CONSTRAINT exec_invocation_conclusion_only_when_completed
    CHECK (outcome =  'completed' OR run_conclusion IS NULL),

    -- Every non-completed outcome carries a reason. 'deadline_exceeded'
    -- names its own cause and must NOT also take a refusal_reason.
    CONSTRAINT exec_invocation_needs_refusal_reason
    CHECK (outcome IN ('completed', 'deadline_exceeded') OR refusal_reason IS NOT NULL),
    CONSTRAINT exec_invocation_refusal_reason_forbidden
    CHECK (outcome NOT IN ('completed', 'deadline_exceeded') OR refusal_reason IS NULL),

    -- ------------------------------------------------------------------
    -- A ROW THAT REACHED NO VERDICT CARRIES NO VERDICT.
    --
    -- The four CHECKs above do NOT hold this, and the reason is the single
    -- most important SQLite fact in this file: A CHECK THAT EVALUATES TO NULL
    -- PASSES. With outcome IS NULL, `outcome <> 'completed'` is NULL, so
    -- `NULL OR (run_conclusion IS NOT NULL)` is NULL whenever run_conclusion
    -- is NULL — the constraint does not bind. A half-written row could
    -- therefore have slipped past all four carrying part of a verdict.
    --
    -- These three are written with IS NULL / IS NOT NULL, which are the only
    -- comparisons in SQL that are never NULL themselves.
    -- ------------------------------------------------------------------
    CONSTRAINT exec_invocation_open_row_has_no_conclusion
    CHECK (outcome IS NOT NULL OR run_conclusion IS NULL),
    CONSTRAINT exec_invocation_open_row_has_no_reason
    CHECK (outcome IS NOT NULL OR refusal_reason IS NULL),
    CONSTRAINT exec_invocation_open_row_has_no_comparison
    CHECK (outcome IS NOT NULL OR comparison     IS NULL),

    -- No row other than a compared drill run may carry a comparison. An outage
    -- run has NO side to compare against; an excluded run was NOT COMPARED.
    CONSTRAINT exec_invocation_comparison_only_on_completed_drill
    CHECK (comparison IS NULL OR (purpose = 'drill' AND outcome = 'completed')),

    -- ...and a compared drill run may not CLOSE without one, so the gate's
    -- numerator cannot quietly empty through an omission.
    --
    -- THE GRANDFATHER CLAUSE, and why it is not a third comparison value.
    -- Rows written under schema 1 are `completed` drill rows for which no
    -- verdict was ever PRODUCED: the column did not exist, and the validator's
    -- output shape was not settled. There are exactly three things this
    -- migration could do with them, and two of them are forbidden:
    --   - write 'matched'    — fabricates the gate's NUMERATOR. Never.
    --   - write 'mismatched' — fabricates the third condition's subject.
    --   - rewrite outcome to 'unmeasurable' — destroys a recorded verdict and
    --     silently shrinks D.
    -- Deleting them is repair-by-deletion, which this ledger forbids.
    -- Aborting the migration on them would be the 0003 precedent on gateway.db
    -- — but there the failing row was MALFORMED and had a repair; here the row
    -- is perfectly legal under the schema that wrote it and has NO repair, so
    -- an abort makes the ledger unmigratable forever.
    --
    -- So the constraint is SCOPED to the rows it can bind. `comparison IS NULL`
    -- already means "no verdict recorded"; nothing is invented. The exemption
    -- is not an escape hatch because exec_invocation_schema_floor refuses any
    -- INSERT claiming schema_version < 2, and schema_version is in
    -- exec_invocation_write_once's list so it cannot be lowered afterwards.
    -- The grandfathered set is finite, cannot grow, and is listed by
    -- DB.CompletedDrillsWithoutComparison. It counts toward A and D and NOT
    -- toward M, so it makes the gate read WORSE — the safe direction.
    CONSTRAINT exec_invocation_drill_needs_comparison
    CHECK (comparison IS NOT NULL
           OR schema_version < 2
           OR purpose <> 'drill'
           OR outcome IS NULL
           OR outcome <> 'completed'),

    -- If the process was invoked, preflight's outputs WERE written. Only a
    -- `refused` row may stand without them.
    --
    -- The leading `outcome IS NOT NULL AND` is not redundant. Without it,
    -- `outcome = 'refused'` is NULL on an open row, and NULL OR FALSE is NULL,
    -- which PASSES — an open row could then be written with no argv hash at
    -- all. `IS NOT NULL` collapses that to FALSE and the constraint binds.
    CONSTRAINT exec_invocation_preflight_columns_present
    CHECK ((outcome IS NOT NULL AND outcome = 'refused')
           OR (argv_sha256 IS NOT NULL AND workflow_norm_sha256 IS NOT NULL
               AND workflow_norm_applied IS NOT NULL AND workdir_clean IS NOT NULL))
);

-- Drops exec_invocation's indexes and its append-only trigger with it. The
-- deferred FK counter goes up by one per child row here; the re-insert below is
-- what brings it back to zero.
DROP TABLE main.exec_invocation;

-- The six child tables' `REFERENCES exec_invocation(invocation_id)` clauses are
-- untouched by this rename: they name `exec_invocation`, which is free between
-- the DROP above and this statement, and SQLite only rewrites references to the
-- name being renamed FROM. Verified by reading the child DDL back out of
-- sqlite_master afterwards.
ALTER TABLE main.exec_invocation_new RENAME TO exec_invocation;

-- rowid is carried EXPLICITLY. Nothing in this package orders by rowid today,
-- which is exactly why omitting it would have been discovered late, in an
-- export diff, rather than here.
--
-- This is also the statement that clears the deferred foreign-key counter the
-- DROP raised. It runs BEFORE the triggers are created, which is what keeps the
-- grandfather clause honest: the carried rows keep schema_version = 1 because
-- exec_invocation_schema_floor does not exist yet.
INSERT INTO main.exec_invocation (
    rowid,
    invocation_id, schema_version, backend_name, executor_kind, executor_version,
    executor_pin_ok, started_at, finished_at, repo, workflow_path, commit_sha,
    event_name, event_payload_sha256, purpose, drill_day, supply_mode,
    argv_sha256, workflow_norm_sha256, workflow_norm_applied, workdir_clean,
    workdir_commit_count, workdir_shallow,
    outcome, refusal_reason, run_conclusion, comparison
)
SELECT
    carried_rowid,
    invocation_id, schema_version, backend_name, executor_kind, executor_version,
    executor_pin_ok, started_at, finished_at, repo, workflow_path, commit_sha,
    event_name, event_payload_sha256, purpose, drill_day, supply_mode,
    argv_sha256, workflow_norm_sha256, workflow_norm_applied, workdir_clean,
    workdir_commit_count, workdir_shallow,
    outcome, refusal_reason, run_conclusion, NULL
FROM temp.shadow_0002_carry;

DROP TABLE temp.shadow_0002_carry;

CREATE INDEX idx_exec_invocation_day_purpose ON exec_invocation(drill_day, purpose);
CREATE INDEX idx_exec_invocation_started     ON exec_invocation(started_at);

-- The open rows are the fifth audit query's whole subject, and that query is
-- read on every drill report. Without this index it is a full scan of the run
-- axis on a long-scan connection.
CREATE INDEX idx_exec_invocation_open ON exec_invocation(started_at) WHERE outcome IS NULL;

-- ---------------------------------------------------------------------------
-- 4. The append-only trigger, NARROWED.
--
-- The prohibition is no longer "no UPDATE"; it is "no UPDATE OUTSIDE these five
-- columns". Everything carrying IDENTITY or a DECISION is write-once. Note the
-- shape: UPDATE OF fires on a column being NAMED, not on its value changing, so
-- rewriting `purpose` with the value it already has is refused too. The writer
-- must name exactly the five.
--
-- The narrowing is deliberate and its reason is recorded, so it cannot later be
-- read as a relaxation that crept in.
-- ---------------------------------------------------------------------------
CREATE TRIGGER exec_invocation_write_once
BEFORE UPDATE OF invocation_id, schema_version, backend_name, executor_kind,
                 executor_version, executor_pin_ok, started_at, repo, workflow_path,
                 commit_sha, event_name, event_payload_sha256, purpose, drill_day,
                 supply_mode, argv_sha256, workflow_norm_sha256, workflow_norm_applied,
                 workdir_clean, workdir_commit_count, workdir_shallow
ON exec_invocation
BEGIN
    SELECT RAISE(ABORT, 'shadow: only the five phase-2 columns may be written (finished_at, outcome, refusal_reason, run_conclusion, comparison)');
END;

-- A verdict is written ONCE. A closed row taking a second verdict would be
-- post-hoc labelling on the run axis, the same escape hatch `purpose` is
-- forbidden from opening.
CREATE TRIGGER exec_invocation_no_reverdict
BEFORE UPDATE OF finished_at, outcome, refusal_reason, run_conclusion, comparison
ON exec_invocation
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'shadow: a verdict is written once (this invocation is already closed)');
END;

-- The floor that keeps the grandfather clause from being an escape hatch. Rows
-- copied above kept schema_version = 1 because this trigger did not exist yet;
-- every row written from now on must claim 2 or more, and therefore must carry
-- a comparison to close as a completed drill.
CREATE TRIGGER exec_invocation_schema_floor
BEFORE INSERT ON exec_invocation
WHEN NEW.schema_version < 2
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_invocation.schema_version must be >= 2 (schema_version < 2 exempts a row from the comparison requirement and is reserved for rows this build did not write)');
END;
