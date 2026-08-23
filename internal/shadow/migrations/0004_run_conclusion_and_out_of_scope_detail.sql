-- 0004_run_conclusion_and_out_of_scope_detail — two repairs, one transaction.
--
--   1. `startup_failure` is ADDED to the two RUN-LEVEL conclusion CHECKs:
--      exec_invocation.run_conclusion (0002) and drill_candidate.github_conclusion
--      (0003). Both say eight; internal/exec's RunConclusions() says nine.
--      THIS IS A REPAIR, NOT A WIDENING. GitHub really writes the value — it
--      was measured in 1 of 10,897 concluded runs in the activity-weighted
--      frame and 7 of 2,604 in the uniform frame — and the drill may not
--      silently drop a candidate GitHub created. So the missing word did not
--      cost one row: it aborted the day's scan.
--
--      There is a LIVE Go/SQL window today, in BOTH directions, which is why
--      this is not cosmetic. shadow.Candidate.Validate checks GitHub's verdict
--      with exec.Conclusion.IsGitHub() and shadow.Verdict.Validate does the
--      same for the run column — and IsGitHub() has admitted `startup_failure`
--      since 2026-08-23. Go accepts the row and the schema aborts the INSERT.
--
--      exec_job_result.conclusion and exec_step_observation.conclusion are NOT
--      touched and MUST NOT BE. The job column holds an ENGINE's result, never
--      GitHub's; no engine produces `startup_failure`. And the step column's
--      set is defined by subtraction from exec.Conclusions(), so widening the
--      job set would silently widen the step set too.
--
--   2. `out_of_scope` gets its MECHANISM. The value carries THREE meanings —
--      `shadow: never`, not configured, and GitHub's own machinery — and until
--      now they were separated only by disposition_detail, which was FREE TEXT
--      with no CHECK. That is the exact inverse of the disposition set's own
--      argument (0003), which holds the anti-exclusion rule by what the set
--      cannot SAY.
--
-- ---------------------------------------------------------------------------
-- THE GITHUB-MACHINERY RULE, AND THE CAVEAT THAT TRAVELS WITH IT
--
-- Measured 2026-08-23 over 1,909 runs in three independently drawn frames:
-- every run of GitHub's own machinery carries BOTH
--
--     path = 'dynamic/<producer>/<name>'        event = 'dynamic'
--
-- and the two agree perfectly in both directions — no `dynamic/` path under
-- another event, no `dynamic` event under another path, zero exceptions.
-- Producers seen: pages, dependabot, github-code-scanning, copilot-swe-agent,
-- agents, copilot-pull-request-reviewer, dependency-graph.
--
--   * THE MATCH IS ON THE PREFIX, NEVER ON A NAME LIST. The second segment is
--     not a closed vocabulary: Copilot review appeared under TWO different
--     producers, and `agents/copilot-pull-request-reviewer` is a newer spelling
--     of the same thing. A name list falls silently to the wrong side the
--     moment GitHub adds a producer.
--
--   * `substr(workflow_path, 1, 8) = 'dynamic/'`, NOT `LIKE 'dynamic/%'`.
--     SQLite's LIKE is case-INSENSITIVE for ASCII, so LIKE would also catch a
--     row spelling `DYNAMIC/`. This predicate means the exact bytes GitHub
--     writes and nothing else.
--
--   * THE RULE IS A POSITIVE MATCH ON `dynamic/`. It is NOT a negative match on
--     `.github/workflows/`, and this line is here so nobody writes the negative
--     form later. A workflow file we wrote that GitHub could not COMPILE
--     comes back as
--
--         path = "BuildFailed"   conclusion = "startup_failure"   name = ""
--
--     with a workflow object in state "deleted". THAT RUN IS OURS. Any rule
--     shaped "not under .github/workflows/ implies not ours" misfiles it as
--     GitHub's machinery, which is the one direction this axis may not err in.
--     (`BuildFailed` implies `startup_failure`; the converse does not hold —
--     `startup_failure` also occurs with a perfectly ordinary path.)
--
--   * AND THE UNDOCUMENTEDNESS IS PART OF THE RULE, NOT A FOOTNOTE. `dynamic`
--     appears in NO published `event` enum and the `dynamic/` path shape is in
--     no REST reference. It was universal and exception-free in the
--     measurement, and it is still OBSERVED behaviour, not PROMISED behaviour.
--     If GitHub renames the prefix, this CHECK does not degrade quietly — it
--     flips from holding the rule to ABORTING EVERY INSERT for a Pages-hosted
--     repo, because such a run would then be a `dynamic/`-less row that the
--     selector still calls github_internal_workflow, or an ordinary-looking row
--     the selector calls something else while the biconditional demands
--     agreement. Loud is the right failure direction here. It is written down
--     because a schema-level dependency on unpromised behaviour must say so
--     where the next person reads it, and this file is that place.
--
-- WHAT THIS FILE DELIBERATELY DOES NOT DO: the ORG-LEVEL REQUIRED WORKFLOW
-- class. Those runs carry a `workflow_url` of .../actions/required_workflows/{id},
-- an ordinary-looking `run.path` that exists at NO ref in the repo, and they
-- were 0.48 % of the activity-weighted frame — concentrated in exactly the
-- org-managed shape Standpipe targets. `workflow_path` CANNOT SEE THEM: only
-- the workflow_url path segment can, and this ledger does not store it.
-- Inventing a rule for a class we cannot read would be the same guess the
-- rule above was written to avoid. It is tracked as a separate open question.
--
-- ---------------------------------------------------------------------------
-- WHERE THIS FILE DIVERGES FROM THE PROPOSED DDL, AND WHY
--
--   a. THE BICONDITIONAL WAS MADE TWO-VALUED. As proposed it read
--
--          CHECK ((substr(workflow_path, 1, 8) = 'dynamic/')
--                 = (disposition = 'out_of_scope'
--                    AND disposition_detail = 'github_internal_workflow'))
--
--      and with disposition = 'out_of_scope' and disposition_detail NULL the
--      right-hand side is `TRUE AND NULL` = NULL, so the whole CHECK is NULL —
--      AND SQLITE PASSES A CHECK THAT EVALUATES TO NULL. The only thing
--      stopping that row today is a DIFFERENT constraint written before it,
--      drill_candidate_exclusion_has_a_reason. Review named that dependency
--      and asked for it to be carried knowingly. Naming it and then REMOVING
--      it is better: `disposition_detail IS NOT NULL AND` collapses the NULL
--      to FALSE, both sides are then two-valued (workflow_path is NOT NULL, so
--      the left side never is), and the discriminator stands on its own feet.
--      The same repair is applied to the vocabulary CHECK for the same reason.
--
--      The identical NULL rule has already silently disarmed FOUR CHECKs on
--      exec_invocation once (0002's "A ROW THAT REACHED NO VERDICT CARRIES NO
--      VERDICT" block). A discriminator that leans on a neighbour is one
--      relaxation away from passing everything without erroring.
--
--   b. The CHECKs are NAMED CONSTRAINTS, as 0002's and 0003's are. An unnamed
--      one fails with the rebuild table's name, which the operator has never
--      seen, in a message arriving while a migration aborts.
--
--   c. exec_invocation.schema_version keeps DEFAULT 2 and its floor trigger
--      keeps `< 2`. Widening an enum imposes no new obligation on a row, so
--      there is nothing for a schema floor to enforce and nothing for the 0002
--      grandfather clause to be re-scoped against. Bumping the default would
--      have changed what `schema_version` MEANS as a side effect of an enum
--      repair.
--
-- ---------------------------------------------------------------------------
-- HOW THE TWO REBUILDS ARE ORDERED
--
-- SQLite has no ALTER TABLE ... ADD/ALTER CONSTRAINT, so widening a CHECK is a
-- TABLE REBUILD. The order below is 0002's, not the SQLite manual's, and the
-- reason was measured there:
--
--   `PRAGMA defer_foreign_keys` DEFERS A COUNTER; IT DOES NOT RE-CHECK AT
--   COMMIT. DROP TABLE runs an implicit DELETE FROM with FKs enabled and
--   increments the counter ONCE PER CHILD ROW. ALTER TABLE ... RENAME TO puts
--   an identical parent back and DOES NOT DECREMENT IT. COMMIT then fails with
--   SQLITE_CONSTRAINT_FOREIGNKEY (787) on a database that is entirely
--   consistent. What decrements the counter is INSERTING THE PARENT ROWS BACK.
--
-- So each rebuild is:  carry to TEMP -> CREATE new -> DROP -> RENAME ->
-- RE-INSERT -> indexes -> triggers.
--
-- `PRAGMA foreign_keys` is NOT used: it is a CONNECTION pragma set in the DSN
-- and a SILENT NO-OP inside a transaction. `defer_foreign_keys` is the one that
-- works there, and it resets itself at COMMIT and at ROLLBACK, so this file
-- cannot leak a relaxed setting onto the daemon's writer connection.
--
-- exec_invocation now has SEVEN child tables, not six: 0003's drill_attempt
-- joined exec_image, exec_local_repository, exec_job_result,
-- exec_step_observation, exec_residue and exec_note. drill_candidate has one,
-- also drill_attempt. Both rebuilds therefore need the re-insert, and the two
-- are independent: each brings the counter it raised back to zero before the
-- next begins.
--
-- ---------------------------------------------------------------------------
-- THE FAILING COPY IS THE FEATURE, AND HERE IS WHAT IT COSTS
--
-- The new CHECKs apply to the re-insert. A pre-existing drill_candidate row the
-- new schema cannot hold ABORTS THE WHOLE FILE, rolling back to schema 3 with
-- every row intact and user_version unchanged. Two classes can do that:
--
--   * an `out_of_scope` row whose disposition_detail is not one of the three
--     words — i.e. one of the rows whose free text this migration exists to
--     close; and
--   * a `dynamic/` row the selector did not file as GitHub's machinery — i.e. a
--     run of GitHub's own machinery that a selector unaware of this rule
--     treated as a candidate.
--
-- Both are exactly the defect the rule names, so neither may be rewritten here.
-- Writing 'github_internal_workflow' over an operator's own sentence would be
-- repair-by-fabrication, and 0002's grandfather clause is NOT a precedent for
-- it: there the blocking row was LEGAL under the schema that wrote it and had
-- NO repair, so an abort would have made the ledger unmigratable forever. Here
-- the row has a repair and the operator has to make the call, because the call
-- is which of three meanings the row actually carried.
--
-- The operator finds them with:
--
--   SELECT candidate_id, drill_day, workflow_path, disposition, disposition_detail
--     FROM drill_candidate
--    WHERE (disposition = 'out_of_scope'
--           AND (disposition_detail IS NULL
--                OR disposition_detail NOT IN ('shadow_never', 'not_configured',
--                                              'github_internal_workflow')))
--       OR ((substr(workflow_path, 1, 8) = 'dynamic/')
--            <> (disposition = 'out_of_scope'
--                AND disposition_detail IS NOT NULL
--                AND disposition_detail = 'github_internal_workflow'));
--
-- drill_candidate is APPEND-ONLY, so the repair is DELETE plus re-INSERT and it
-- is outside this ledger's guarantees — the same cost 0002 wrote down. A
-- `selected` row with attempts cannot be deleted while drill_attempt points at
-- it, and a `selected` row is the only violating shape that can have children.
--
-- AT THE TIME THIS FILE WAS WRITTEN THE CLASS IS EMPTY BY CONSTRUCTION: 0003
-- shipped on 2026-08-23, RecordExamination is the only writer of
-- drill_candidate anywhere in this module, and nothing outside this package's
-- tests calls it — the drill's single entry point does not exist yet. That is
-- a fact about today, not a guarantee, which is why the abort path is written
-- out above rather than assumed away.
-- ---------------------------------------------------------------------------
PRAGMA defer_foreign_keys = ON;

-- ===========================================================================
-- 1. exec_invocation — run_conclusion gains `startup_failure`.
--
-- Every other column, CHECK, index and trigger is carried across UNCHANGED and
-- character-for-character. This file is a CHECK repair; a rebuild is the only
-- mechanism SQLite offers for it, and a rebuild that quietly improves something
-- else is a rebuild nobody can review.
-- ===========================================================================
DROP TABLE IF EXISTS temp.shadow_0004_inv;
CREATE TEMP TABLE shadow_0004_inv AS
    SELECT rowid AS carried_rowid, * FROM main.exec_invocation;

CREATE TABLE main.exec_invocation_new (
    invocation_id         TEXT    PRIMARY KEY,

    -- The 0002 grandfather clause reads this column and its floor trigger
    -- guards it. Untouched here: divergence c at the top of this file.
    schema_version        INTEGER NOT NULL DEFAULT 2
                          CONSTRAINT exec_invocation_schema_version_positive
                          CHECK (schema_version >= 1),

    backend_name          TEXT    NOT NULL,
    executor_kind         TEXT    NOT NULL,          -- 'act'
    executor_version      TEXT    NOT NULL,          -- the version OBSERVED
    executor_pin_ok       INTEGER NOT NULL
                          CONSTRAINT exec_invocation_pin_ok_boolean
                          CHECK (executor_pin_ok IN (0, 1)),

    started_at            INTEGER NOT NULL,          -- unix-millis

    -- NULL means TWO things and (finished_at, outcome) separates them:
    --   outcome IS NULL     — the row is OPEN; the harness never came back.
    --   outcome IS NOT NULL — deadline_exceeded.
    finished_at           INTEGER,

    repo                  TEXT    NOT NULL,
    workflow_path         TEXT    NOT NULL,
    commit_sha            TEXT    NOT NULL
                          CONSTRAINT exec_invocation_commit_sha_len
                          CHECK (length(commit_sha) = 40),
    event_name            TEXT    NOT NULL,
    event_payload_sha256  TEXT    NOT NULL,          -- the trace of an input-less replay

    purpose               TEXT    NOT NULL
                          CONSTRAINT exec_invocation_purpose_enum
                          CHECK (purpose IN ('drill', 'outage')),

    drill_day             TEXT    NOT NULL REFERENCES drill_day(day),

    supply_mode           TEXT    NOT NULL
                          CONSTRAINT exec_invocation_supply_mode_enum
                          CHECK (supply_mode IN ('online', 'offline')),

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

    -- THE ONE CHANGE IN THIS TABLE. `startup_failure` was added 2026-08-23; the
    -- other eight are byte-identical to 0002's list.
    --
    -- WHY IT BELONGS IN *OUR* COLUMN, when exec.DeriveRunConclusion can only
    -- ever return cancelled|failure|skipped|success: this column and
    -- drill_candidate.github_conclusion are the TWO SIDES OF THE COMPARISON
    -- that produces M. A value GitHub can write and our side
    -- cannot HOLD makes the comparison ill-typed for that run — and the
    -- narrowing would have to be a hand-written subtraction from
    -- exec.RunConclusions(), which is the fifth-hand-copy shape shadow.db's
    -- CHECK-literal test exists to prevent. shadow.Verdict.Validate already
    -- admits it (it guards with IsGitHub()), so the schema was the only
    -- disagreeing party.
    --
    -- 'in_progress' can never join this set: that is a value of GitHub's
    -- `status` field, not `conclusion`.
    run_conclusion        TEXT
                          CONSTRAINT exec_invocation_run_conclusion_enum
                          CHECK (run_conclusion IS NULL OR run_conclusion IN (
                                 'success', 'failure', 'cancelled', 'skipped',
                                 'timed_out', 'action_required', 'neutral', 'stale',
                                 'startup_failure')),

    -- THE GATE'S NUMERATOR (M) IS READ HERE AND CANNOT BE DERIVED.
    comparison            TEXT
                          CONSTRAINT exec_invocation_comparison_enum
                          CHECK (comparison IS NULL OR comparison IN ('matched', 'mismatched')),

    CONSTRAINT exec_invocation_completed_needs_conclusion
    CHECK (outcome <> 'completed' OR run_conclusion IS NOT NULL),
    CONSTRAINT exec_invocation_conclusion_only_when_completed
    CHECK (outcome =  'completed' OR run_conclusion IS NULL),

    CONSTRAINT exec_invocation_needs_refusal_reason
    CHECK (outcome IN ('completed', 'deadline_exceeded') OR refusal_reason IS NOT NULL),
    CONSTRAINT exec_invocation_refusal_reason_forbidden
    CHECK (outcome NOT IN ('completed', 'deadline_exceeded') OR refusal_reason IS NULL),

    -- A ROW THAT REACHED NO VERDICT CARRIES NO VERDICT. Written with IS NULL /
    -- IS NOT NULL because those are the only comparisons in SQL that are never
    -- NULL themselves — the four CHECKs above do NOT bind on an open row.
    CONSTRAINT exec_invocation_open_row_has_no_conclusion
    CHECK (outcome IS NOT NULL OR run_conclusion IS NULL),
    CONSTRAINT exec_invocation_open_row_has_no_reason
    CHECK (outcome IS NOT NULL OR refusal_reason IS NULL),
    CONSTRAINT exec_invocation_open_row_has_no_comparison
    CHECK (outcome IS NOT NULL OR comparison     IS NULL),

    CONSTRAINT exec_invocation_comparison_only_on_completed_drill
    CHECK (comparison IS NULL OR (purpose = 'drill' AND outcome = 'completed')),

    -- THE 0002 GRANDFATHER CLAUSE, carried across unchanged. Rows written under
    -- schema 1 are `completed` drill rows for which no verdict was ever
    -- PRODUCED; the exemption is not an escape hatch because
    -- exec_invocation_schema_floor refuses any INSERT claiming < 2 and
    -- schema_version is in exec_invocation_write_once's list.
    CONSTRAINT exec_invocation_drill_needs_comparison
    CHECK (comparison IS NOT NULL
           OR schema_version < 2
           OR purpose <> 'drill'
           OR outcome IS NULL
           OR outcome <> 'completed'),

    -- The leading `outcome IS NOT NULL AND` is not redundant: without it
    -- `outcome = 'refused'` is NULL on an open row and NULL OR FALSE passes.
    CONSTRAINT exec_invocation_preflight_columns_present
    CHECK ((outcome IS NOT NULL AND outcome = 'refused')
           OR (argv_sha256 IS NOT NULL AND workflow_norm_sha256 IS NOT NULL
               AND workflow_norm_applied IS NOT NULL AND workdir_clean IS NOT NULL))
);

-- Drops exec_invocation's two indexes, its partial open index and its three
-- triggers with it, and raises the deferred FK counter by one per child row
-- across the SEVEN referencing tables. The re-insert below is what clears it.
DROP TABLE main.exec_invocation;

-- The children's `REFERENCES exec_invocation(invocation_id)` clauses are
-- untouched: they name `exec_invocation`, which is free between the DROP above
-- and this statement, and SQLite only rewrites references to the name being
-- renamed FROM.
ALTER TABLE main.exec_invocation_new RENAME TO exec_invocation;

-- rowid is carried EXPLICITLY, and `comparison` is carried for real this time —
-- 0002 wrote NULL there because the column was new in that migration.
--
-- This is also the statement that clears the deferred foreign-key counter, and
-- it runs BEFORE the triggers are recreated, so grandfathered rows keep
-- schema_version = 1 rather than being refused by exec_invocation_schema_floor.
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
    outcome, refusal_reason, run_conclusion, comparison
FROM temp.shadow_0004_inv;

DROP TABLE temp.shadow_0004_inv;

CREATE INDEX idx_exec_invocation_day_purpose ON exec_invocation(drill_day, purpose);
CREATE INDEX idx_exec_invocation_started     ON exec_invocation(started_at);
CREATE INDEX idx_exec_invocation_open ON exec_invocation(started_at) WHERE outcome IS NULL;

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

CREATE TRIGGER exec_invocation_no_reverdict
BEFORE UPDATE OF finished_at, outcome, refusal_reason, run_conclusion, comparison
ON exec_invocation
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'shadow: a verdict is written once (this invocation is already closed)');
END;

CREATE TRIGGER exec_invocation_schema_floor
BEFORE INSERT ON exec_invocation
WHEN NEW.schema_version < 2
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_invocation.schema_version must be >= 2 (schema_version < 2 exempts a row from the comparison requirement and is reserved for rows this build did not write)');
END;

-- ===========================================================================
-- 2. drill_candidate — github_conclusion gains `startup_failure`, and
--    disposition_detail stops being free text where it carried a decision.
-- ===========================================================================

-- THIS LINE IS LOAD-BEARING AND IT WAS VERIFIED BY REMOVING IT.
--
-- drill_attempt_only_for_selected lives on drill_attempt and reads
-- drill_candidate in its BODY. `ALTER TABLE ... RENAME TO` RE-PARSES EVERY
-- TRIGGER IN THE SCHEMA, and between the DROP and the RENAME below there is no
-- drill_candidate for that body to name. Without this line the migration fails
-- with
--
--   SQL logic error: error in trigger drill_attempt_only_for_selected:
--   no such table: main.drill_candidate (1)
--
-- 0002's rebuild never met this: exec_invocation's seven children have foreign
-- KEYS into it and no trigger BODY that reads it, and a foreign key clause
-- survives the window (SQLite only rewrites references to the name being
-- renamed FROM) while a trigger body does not. So the rule is not "rebuilds
-- need this"; it is A TABLE NAMED IN ANOTHER OBJECT'S TRIGGER BODY CANNOT BE
-- ABSENT ACROSS A RENAME. The trigger is recreated verbatim at the bottom.
DROP TRIGGER drill_attempt_only_for_selected;

DROP TABLE IF EXISTS temp.shadow_0004_cand;
CREATE TEMP TABLE shadow_0004_cand AS
    SELECT rowid AS carried_rowid, * FROM main.drill_candidate;

CREATE TABLE main.drill_candidate_new (
    candidate_id       TEXT    PRIMARY KEY,

    drill_day          TEXT    NOT NULL REFERENCES drill_day(day),

    examined_at        INTEGER NOT NULL,             -- unix-millis

    selector_rev       TEXT    NOT NULL
                       CONSTRAINT drill_candidate_selector_rev_nonempty
                       CHECK (selector_rev <> ''),

    source             TEXT    NOT NULL
                       CONSTRAINT drill_candidate_source_enum
                       CHECK (source IN ('github_history', 'synthetic')),

    repo               TEXT    NOT NULL,

    -- SINCE THIS MIGRATION THIS COLUMN IS READ, not merely stored. It is the
    -- discriminator for the third meaning of `out_of_scope`; see
    -- drill_candidate_github_internal_is_dynamic_path below and the block at
    -- the top of this file.
    workflow_path      TEXT    NOT NULL,

    commit_sha         TEXT    NOT NULL
                       CONSTRAINT drill_candidate_commit_sha_len
                       CHECK (length(commit_sha) = 40),
    event_name         TEXT    NOT NULL,

    github_run_id      INTEGER,
    github_run_attempt INTEGER
                       CONSTRAINT drill_candidate_run_attempt_positive
                       CHECK (github_run_attempt IS NULL OR github_run_attempt >= 1),

    -- unix-millis. THE DEFERRAL ORDER: oldest first, GitHub's own
    -- sequence, and the sort key reads nothing from our side.
    github_created_at  INTEGER,

    -- `startup_failure` ADDED 2026-08-23. The set stays WIDER than the data in
    -- the other direction on purpose: `timed_out`, `neutral` and `stale` were
    -- never observed in ~13,500 runs and they STAY. Not having observed a value
    -- is not evidence that it does not exist, width costs three words, and
    -- absence costs an ABORT — which is precisely what this line repairs.
    github_conclusion  TEXT
                       CONSTRAINT drill_candidate_github_conclusion_enum
                       CHECK (github_conclusion IS NULL OR github_conclusion IN
                              ('success', 'failure', 'cancelled', 'skipped',
                               'timed_out', 'action_required', 'neutral', 'stale',
                               'startup_failure')),

    -- THE CLOSED SET, AND WHAT CARRIES THE RULE IS WHAT IT DOES NOT CONTAIN.
    --
    -- None of the seven can say "we cannot repeat this", "the inputs are
    -- missing", "act does not support it" or "it mismatched last time". What
    -- cannot be written cannot be done. An eighth member is a schema change,
    -- reviewed as one.
    disposition        TEXT    NOT NULL
                       CONSTRAINT drill_candidate_disposition_enum
                       CHECK (disposition IN (
                              'selected',          -- selected; attempted in BOTH modes
                              'deferred',          -- the day cap was full; requeued, NOT dropped
                              'not_concluded',     -- GitHub has written no conclusion yet
                              'out_of_scope',      -- outside the drill universe
                              'already_selected',  -- this run was selected on an earlier day
                              'not_created',       -- prefilter: GitHub would not have created it
                              'undecidable')),     -- prefilter could not decide

    -- FREE TEXT FOR SIX DISPOSITIONS AND A CLOSED VOCABULARY FOR THE SEVENTH.
    -- The asymmetry is the point: `deferred` says which cap, and no two caps
    -- are the same sentence. `out_of_scope` names a CLASS, and three different
    -- classes were landing on it.
    disposition_detail TEXT,

    CONSTRAINT drill_candidate_history_has_run_id
    CHECK ((source = 'github_history') = (github_run_id IS NOT NULL)),
    CONSTRAINT drill_candidate_history_has_created_at
    CHECK ((source = 'github_history') = (github_created_at IS NOT NULL)),
    CONSTRAINT drill_candidate_history_has_run_attempt
    CHECK ((source = 'github_history') = (github_run_attempt IS NOT NULL)),

    -- THE PREFILTER MAY NOT ELIMINATE A RUN ON THE HISTORY PATH.
    CONSTRAINT drill_candidate_history_dispositions
    CHECK (source <> 'github_history'
           OR disposition IN ('selected', 'deferred', 'not_concluded',
                              'out_of_scope', 'already_selected')),
    CONSTRAINT drill_candidate_prefilter_dispositions_are_synthetic
    CHECK (disposition NOT IN ('not_created', 'undecidable') OR source = 'synthetic'),

    -- GITHUB CONCLUSION IS ALWAYS GITHUB. THE SCORE IS NEVER SCORED AGAINST A
    -- NUMBER WE INVENTED. `deferred` is barred alongside `selected` because a
    -- deferral that can never be selected is a promise that can never be kept.
    CONSTRAINT drill_candidate_synthetic_never_enters_the_score
    CHECK (source <> 'synthetic' OR disposition NOT IN ('selected', 'deferred')),

    CONSTRAINT drill_candidate_selected_needs_conclusion
    CHECK (disposition <> 'selected'      OR github_conclusion IS NOT NULL),
    CONSTRAINT drill_candidate_not_concluded_has_no_conclusion
    CHECK (disposition <> 'not_concluded' OR github_conclusion IS NULL),

    -- NOT BEING SELECTED ALWAYS CARRIES A SENTENCE. A reasonless exclusion
    -- cannot be written — the day axis's reasonless `dropped` day, one level
    -- down. `deferred` is included on purpose: a cap that never says which cap
    -- is how a cap becomes a hiding place.
    --
    -- READ THIS BEFORE TOUCHING IT. As the GitHub-machinery discriminator was
    -- PROPOSED, this constraint was the only thing holding it up, and nothing
    -- said so.
    -- The proposed biconditional evaluates to NULL when disposition_detail is
    -- NULL (`TRUE AND NULL` is NULL), SQLite PASSES a CHECK that evaluates to
    -- NULL, and a `dynamic/` row with a NULL detail therefore sailed straight
    -- through the constraint written to catch it. The only thing refusing that
    -- row was THIS line, in another paragraph, written for another reason.
    --
    -- The two constraints below now hold their own NULLs, so the lean is GONE
    -- rather than merely documented. This line stays because it is still the
    -- general rule for the other five non-selected dispositions — and the
    -- history stays because a reader tightening or relaxing it deserves to know
    -- what once depended on it.
    CONSTRAINT drill_candidate_exclusion_has_a_reason
    CHECK (disposition = 'selected' OR disposition_detail IS NOT NULL),

    -- ------------------------------------------------------------------
    -- `out_of_scope`'s THREE MEANINGS, SEPARATED BY GITHUB'S OWN WORD.
    --
    -- The vocabulary closes the value; the biconditional binds the third
    -- member to a fact about GITHUB's side rather than to a word of ours. That
    -- is what keeps the drill universe's boundary intact: the criterion for
    -- "not in the drill's universe" is decided by data GitHub wrote, with no
    -- reference to what act, the executor or the validator can do.
    --
    -- AND IT ESTABLISHES A PRECEDENCE THE SELECTOR DOES NOT GET TO CHOOSE:
    -- `out_of_scope` BEATS `not_concluded`. A `dynamic/` run is outside the
    -- universe whether or not GitHub has judged it yet, because the
    -- biconditional forces EVERY `dynamic/` row to out_of_scope. The selector
    -- asks about the universe first and about conclusion second.
    --
    -- Both are spelled so they can never evaluate to NULL: SQLite PASSES a
    -- CHECK that evaluates to NULL, and `disposition_detail` is nullable.
    -- Divergence (a) at the top of this file.
    -- ------------------------------------------------------------------
    CONSTRAINT drill_candidate_out_of_scope_detail_enum
    CHECK (disposition <> 'out_of_scope'
           OR (disposition_detail IS NOT NULL AND disposition_detail IN
               ('shadow_never', 'not_configured', 'github_internal_workflow'))),

    CONSTRAINT drill_candidate_github_internal_is_dynamic_path
    CHECK ((substr(workflow_path, 1, 8) = 'dynamic/')
           = (disposition = 'out_of_scope'
              AND disposition_detail IS NOT NULL
              AND disposition_detail = 'github_internal_workflow'))
);

-- Raises the deferred FK counter by one per drill_attempt row; the re-insert
-- below clears it.
DROP TABLE main.drill_candidate;

ALTER TABLE main.drill_candidate_new RENAME TO drill_candidate;

INSERT INTO main.drill_candidate (
    rowid,
    candidate_id, drill_day, examined_at, selector_rev, source,
    repo, workflow_path, commit_sha, event_name,
    github_run_id, github_run_attempt, github_created_at, github_conclusion,
    disposition, disposition_detail
)
SELECT
    carried_rowid,
    candidate_id, drill_day, examined_at, selector_rev, source,
    repo, workflow_path, commit_sha, event_name,
    github_run_id, github_run_attempt, github_created_at, github_conclusion,
    disposition, disposition_detail
FROM temp.shadow_0004_cand;

DROP TABLE temp.shadow_0004_cand;

-- A GITHUB RUN IS SELECTED ONCE IN ITS LIFETIME. Synthetic rows carry a NULL
-- github_run_id and SQLite treats NULLs in a UNIQUE index as distinct, so this
-- index does not bind them at all; what holds that corner is
-- drill_candidate_synthetic_never_enters_the_score, not this index.
CREATE UNIQUE INDEX idx_drill_candidate_selected_once
    ON drill_candidate(github_run_id, github_run_attempt)
    WHERE disposition = 'selected';

CREATE INDEX idx_drill_candidate_day  ON drill_candidate(drill_day, disposition);

-- Covers `deferred` as well as `not_concluded`: the sixth candidate audit
-- reads both, and the half an index leaves out is the half that becomes a
-- hiding place.
CREATE INDEX idx_drill_candidate_open ON drill_candidate(github_created_at)
    WHERE disposition IN ('deferred', 'not_concluded');

CREATE TRIGGER drill_candidate_append_only
BEFORE UPDATE ON drill_candidate
BEGIN
    SELECT RAISE(ABORT, 'shadow: drill_candidate is append-only');
END;

-- Recreated verbatim from 0003 (dropped above for the rename).
CREATE TRIGGER drill_attempt_only_for_selected
BEFORE INSERT ON drill_attempt
WHEN (SELECT disposition FROM drill_candidate WHERE candidate_id = NEW.candidate_id) <> 'selected'
BEGIN
    SELECT RAISE(ABORT, 'shadow: an attempt may only be planned for a selected candidate');
END;
