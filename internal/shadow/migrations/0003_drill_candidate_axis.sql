-- 0003_drill_candidate_axis — the drill's CANDIDATE axis.
--
-- Three tables are ADDED. Nothing in 0001 or 0002 is touched: exec_invocation
-- has no candidate_id column and never gets one. The link is made by ADDING a
-- table, and the price of that is written down at the bottom of this file.
--
-- WHAT THE CANDIDATE AXIS IS FOR, in one sentence: the drill may not EXCLUDE A
-- RUN BY NOT SELECTING IT, and until this migration nothing anywhere could
-- answer "which runs did the selector look at". A rule that nothing executes
-- is a rule that is not held.
--
-- ---------------------------------------------------------------------------
-- THREE THINGS THE DESIGN DROVE AND THIS FILE MAY NOT QUIETLY RE-DECIDE
--
--   1. drill_day_zone has NO FOREIGN KEY to drill_day, and that is the whole
--      point of the table. Driven 2026-08-23: with the FK, retention cannot
--      delete the day the zone row names and Retain fails permanently; without
--      it the zone row outlives the days it explains. A record that EXPLAINS a
--      key cannot be that key's child.
--   2. disposition is closed at SEVEN values and deliberately has no member
--      meaning "we cannot reproduce this run". That absence IS the enforcement
--      of "excluding by not-selecting is forbidden": the excluded class must
--      become a candidate, be attempted, and land in X. An eighth value is a
--      schema change and is reviewed as one, so the rule cannot be broken by
--      accident.
--   3. The partial UNIQUE index makes selection ONCE-EVER. The promotion gate
--      asks for ">= 20 DISTINCT workflow runs" and the denominator query
--      counts ROWS; the index is why those two coincide.
--
-- ---------------------------------------------------------------------------
-- WHERE THIS FILE DIVERGES FROM THE DESIGN'S PROPOSED DDL, AND WHY
--
--   a. The proposal opens with `PRAGMA foreign_keys = ON;`. It is NOT here.
--      This file runs inside BEGIN IMMEDIATE (migrate.go), where PRAGMA
--      foreign_keys is a SILENT NO-OP — it does not error, it does nothing,
--      which is the worst of the three possible behaviours.
--      foreign_keys(ON) is a CONNECTION pragma and sqlite.go sets it in the
--      DSN. A line that looks like it guarantees the FKs and guarantees
--      nothing is worse than no line: 0001 and 0002 do not carry one either.
--   b. Every CHECK is a NAMED CONSTRAINT. That is 0002's convention and it is
--      what makes a constraint failure readable; the proposal writes them
--      bare.
--   c. drill_attempt gets a SECOND trigger, drill_attempt_write_once, so that
--      invocation_id is the ONLY column an UPDATE may name. The proposal
--      protects invocation_id from a second write and leaves candidate_id,
--      supply_mode and planned_at rewritable. That is
--      exec_invocation_write_once's shape (0002) applied one axis up.
--   d. The proposal's
--      `CREATE INDEX idx_drill_attempt_inv ON drill_attempt(invocation_id)`
--      is NOT here: the column is already UNIQUE, which creates an index. A
--      second index over the same column is a second thing to maintain and
--      buys no plan.
--   e. idx_drill_candidate_open covers `deferred` AS WELL AS `not_concluded`.
--      The proposal indexes only the second, and the sixth candidate audit
--      (QueuedCandidatesBefore), the one that is READ rather than repaired,
--      selects both. An index that covers half the query it exists for leaves
--      the other half a full scan, and the half it leaves out is `deferred`,
--      which is precisely the disposition that turns into a permanent
--      exclusion when nobody reads it.
--
-- Read with candidate.go (the writer), audit.go (the six candidate audits) and
-- retention.go.

-- ---------------------------------------------------------------------------
-- THE INTERPRETATION OF THE DAY KEY
--
-- Each row says, from that day onward, which timezone the day key is drawn in.
-- Before this table, shadow.db could not tell whether two day rows were drawn
-- in the same zone. This table is the answer, and it is a SEPARATE record
-- rather than a column on drill_day because a column would be deleted with
-- the day.
--
-- NO FOREIGN KEY, and it is not an omission: item 1 at the top of this file.
-- shadow.Retain never deletes from this table.
-- ---------------------------------------------------------------------------
CREATE TABLE drill_day_zone (
    -- The first day drawn under this timezone. Same shape as drill_day.day and
    -- the same GLOB, so a caller cannot hand it a timestamp.
    since_day  TEXT PRIMARY KEY
               CONSTRAINT drill_day_zone_since_day_shape
               CHECK (since_day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),

    -- An IANA zone name, NOT an offset. A fixed offset is wrong for half the
    -- year in any zone observing DST, and a day drawn under one silently
    -- shifts by an hour at the transition.
    timezone   TEXT NOT NULL
               CONSTRAINT drill_day_zone_timezone_nonempty
               CHECK (timezone <> '')
);

-- Append-only outright. Rewriting a zone row would retroactively change which
-- calendar day every key since since_day names, and nothing would notice.
CREATE TRIGGER drill_day_zone_append_only
BEFORE UPDATE ON drill_day_zone
BEGIN
    SELECT RAISE(ABORT, 'shadow: drill_day_zone is append-only');
END;

-- ---------------------------------------------------------------------------
-- EVERY RUN THAT WAS LOOKED AT — not every run that was selected.
--
-- The narrowing happens INSIDE this table, never in front of it, and the name
-- is the reminder. A run the selector rejected is a ROW carrying a REASON; a
-- run the selector never wrote down is the defect the no-exclusion-by-not-
-- selecting rule exists to forbid.
-- ---------------------------------------------------------------------------
CREATE TABLE drill_candidate (
    candidate_id       TEXT    PRIMARY KEY,

    -- The day this examination belongs to. The FK is what stops a candidate
    -- landing on a day the ledger never recorded, exactly as it does for
    -- exec_invocation.
    drill_day          TEXT    NOT NULL REFERENCES drill_day(day),

    examined_at        INTEGER NOT NULL,             -- unix-millis

    -- The identity of the code that MADE the decision. Inputs alone (the
    -- GitHub columns below) do not let a past selection be questioned; which
    -- selector looked is the other half. Same reason executor_version is on
    -- the run axis.
    selector_rev       TEXT    NOT NULL
                       CONSTRAINT drill_candidate_selector_rev_nonempty
                       CHECK (selector_rev <> ''),

    -- Where the run came from. github_history = GitHub's own run history, the
    -- drill's real path. synthetic = a run the ref prober proposed that GitHub
    -- has no counterpart for yet (that path is not designed yet).
    source             TEXT    NOT NULL
                       CONSTRAINT drill_candidate_source_enum
                       CHECK (source IN ('github_history', 'synthetic')),

    repo               TEXT    NOT NULL,
    workflow_path      TEXT    NOT NULL,
    commit_sha         TEXT    NOT NULL
                       CONSTRAINT drill_candidate_commit_sha_len
                       CHECK (length(commit_sha) = 40),
    event_name         TEXT    NOT NULL,

    github_run_id      INTEGER,
    github_run_attempt INTEGER
                       CONSTRAINT drill_candidate_run_attempt_positive
                       CHECK (github_run_attempt IS NULL OR github_run_attempt >= 1),

    -- unix-millis. THIS IS THE DEFERRAL ORDER: oldest first, GitHub's
    -- own sequence, and the sort key reads nothing from our side. A ranking
    -- that looked at us would let the hard runs be put off indefinitely.
    github_created_at  INTEGER,

    github_conclusion  TEXT
                       CONSTRAINT drill_candidate_github_conclusion_enum
                       CHECK (github_conclusion IS NULL OR github_conclusion IN
                              ('success', 'failure', 'cancelled', 'skipped',
                               'timed_out', 'action_required', 'neutral', 'stale')),

    -- THE CLOSED SET, AND WHAT CARRIES THE RULE IS WHAT IT DOES NOT CONTAIN.
    --
    -- None of the seven can say "we cannot repeat this", "the inputs are
    -- missing", "act does not support it" or "it mismatched last time". What
    -- cannot be written cannot be done. Item 2 at the top of this file.
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

    disposition_detail TEXT,

    -- The source columns and the GitHub columns are one fact stated twice, so
    -- they are bound together rather than left to agree by habit.
    CONSTRAINT drill_candidate_history_has_run_id
    CHECK ((source = 'github_history') = (github_run_id IS NOT NULL)),
    CONSTRAINT drill_candidate_history_has_created_at
    CHECK ((source = 'github_history') = (github_created_at IS NOT NULL)),
    CONSTRAINT drill_candidate_history_has_run_attempt
    CHECK ((source = 'github_history') = (github_run_attempt IS NOT NULL)),

    -- THE PREFILTER MAY NOT ELIMINATE A RUN ON THE HISTORY PATH. GitHub
    -- CREATED that run; our filter believing otherwise is a FINDING about our
    -- filter, not a reason to drop the run. This CHECK closes the laundering
    -- channel, and it is the reason the prefilter can be wrong safely.
    CONSTRAINT drill_candidate_history_dispositions
    CHECK (source <> 'github_history'
           OR disposition IN ('selected', 'deferred', 'not_concluded',
                              'out_of_scope', 'already_selected')),
    CONSTRAINT drill_candidate_prefilter_dispositions_are_synthetic
    CHECK (disposition NOT IN ('not_created', 'undecidable') OR source = 'synthetic'),

    -- GITHUB CONCLUSION IS ALWAYS GITHUB. THE SCORE IS NEVER SCORED AGAINST A
    -- NUMBER WE INVENTED.
    --
    -- Read the two constraints above together and the hole appears: `synthetic`
    -- FORCES github_run_id NULL, and `selected` FORCES github_conclusion NOT
    -- NULL. So the ONLY way to select a synthetic candidate is to write
    -- GitHub's verdict on a run GitHub has no id for -- and that verdict is the
    -- value the comparison scores against, so a fabricated one flows straight
    -- into the gate's numerator M. A match against a number we made up is worth
    -- MORE than a mismatch to whoever finds it later.
    --
    -- This is drill_candidate_history_dispositions pointing the other way. That
    -- one stops us from laundering a run OUT of the corpus; this one stops us
    -- from laundering a verdict IN.
    --
    -- `deferred` is barred alongside `selected`, and that is a decision rather
    -- than symmetry. Deferral is a PROMISE TO LOOK AGAIN, and with `selected`
    -- barred it is a promise that can never be kept: the row would sit in the
    -- sixth audit query's list forever with nothing able to clear it -- the same
    -- hiding place idx_drill_candidate_open exists to keep readable. The two
    -- must also be LIFTED together, so they are held by one constraint.
    --
    -- The remaining five stay writable for synthetic rows: `not_created` and
    -- `undecidable` are the prefilter's own, and `out_of_scope`,
    -- `already_selected` and `not_concluded` say nothing about a score.
    --
    -- WHAT A FUTURE SYNTHETIC PATH IS EXPECTED TO DO WITH THIS. Replace it,
    -- not work around it. A synthetic candidate that can be scored needs its
    -- own identity for the run it proposes and its own answer to where a
    -- comparable conclusion comes from; when it has both, this constraint is
    -- what has to be rewritten, and rewriting it is the moment those two
    -- questions get asked. Nothing writes
    -- synthetic rows today, so barring the path costs nothing now and hands
    -- that design an honest starting point instead of a hole it has to notice.
    CONSTRAINT drill_candidate_synthetic_never_enters_the_score
    CHECK (source <> 'synthetic' OR disposition NOT IN ('selected', 'deferred')),

    -- A run cannot be selected before GitHub has judged it, and a run GitHub
    -- has judged is not `not_concluded`.
    CONSTRAINT drill_candidate_selected_needs_conclusion
    CHECK (disposition <> 'selected'      OR github_conclusion IS NOT NULL),
    CONSTRAINT drill_candidate_not_concluded_has_no_conclusion
    CHECK (disposition <> 'not_concluded' OR github_conclusion IS NULL),

    -- NOT BEING SELECTED ALWAYS CARRIES A SENTENCE. A reasonless exclusion
    -- cannot be written — the day axis's reasonless `dropped` day, one level
    -- down. `deferred` is included on purpose: a cap that never says which cap
    -- is how a cap becomes a hiding place.
    CONSTRAINT drill_candidate_exclusion_has_a_reason
    CHECK (disposition = 'selected' OR disposition_detail IS NOT NULL)
);

-- A GITHUB RUN IS SELECTED ONCE IN ITS LIFETIME.
--
-- The promotion gate asks for ">= 20 DISTINCT workflow runs" and the
-- denominator query counts ROWS. Repeating one run on twenty days is
-- INDISTINGUISHABLE from twenty runs in that query. The constraint lives here
-- because here it can be held: it constrains WHAT A ROW CONTAINS.
--
-- Synthetic rows carry a NULL github_run_id and SQLite treats NULLs in a UNIQUE
-- index as distinct, so this index does not bind them AT ALL. That is harmless
-- only because drill_candidate_synthetic_never_enters_the_score bars a
-- synthetic row from being `selected` in the first place -- the index has no
-- synthetic rows to fail on. IT IS NOT THE INDEX THAT HOLDS THE LINE THERE, and
-- a future synthetic path must not read it as if it were: the once-ever
-- guarantee is scoped to the history path, and a synthetic path that can be
-- selected needs its own identity and its own index.
CREATE UNIQUE INDEX idx_drill_candidate_selected_once
    ON drill_candidate(github_run_id, github_run_attempt)
    WHERE disposition = 'selected';

CREATE INDEX idx_drill_candidate_day  ON drill_candidate(drill_day, disposition);

-- The sixth audit query reads deferred and not-yet-concluded runs in GitHub's
-- own created order. Without this index it is a full scan of the axis.
CREATE INDEX idx_drill_candidate_open ON drill_candidate(github_created_at)
    WHERE disposition IN ('deferred', 'not_concluded');

-- Append-only outright. Re-examining a run writes a NEW row on a NEW day
-- (`already_selected`); rewriting the old one would erase the fact that the
-- selector once saw it and said something else.
CREATE TRIGGER drill_candidate_append_only
BEFORE UPDATE ON drill_candidate
BEGIN
    SELECT RAISE(ABORT, 'shadow: drill_candidate is append-only');
END;

-- ---------------------------------------------------------------------------
-- THE PLANNED ATTEMPT.
--
-- The row is written BEFORE the process is invoked, with invocation_id NULL —
-- exec_invocation two-phase shape, one axis up, and for the same reason: a run
-- we SELECTED and then failed to invoke must not look the same in the ledger
-- as a run we never selected. exec_invocation cannot catch that gap because
-- there the row is never born at all.
-- ---------------------------------------------------------------------------
CREATE TABLE drill_attempt (
    candidate_id  TEXT    NOT NULL REFERENCES drill_candidate(candidate_id),

    -- The gate is scored PER MODE and the schedule is PAIRED: a selected
    -- candidate is attempted in both.
    supply_mode   TEXT    NOT NULL
                  CONSTRAINT drill_attempt_supply_mode_enum
                  CHECK (supply_mode IN ('online', 'offline')),

    planned_at    INTEGER NOT NULL,                  -- unix-millis

    -- NULL until the executor row is opened. UNIQUE because one invocation
    -- belongs to at most one attempt; that UNIQUE is also the index the fifth
    -- audit query and retention read through.
    invocation_id TEXT    UNIQUE REFERENCES exec_invocation(invocation_id),

    PRIMARY KEY (candidate_id, supply_mode)
);

-- AN ATTEMPT MAY ONLY BE PLANNED FOR A SELECTED CANDIDATE. Without this, the
-- disposition could say `out_of_scope` while the executor ran the run anyway,
-- and both halves of the ledger would look sound on their own.
--
-- A candidate_id with no row at all leaves the subselect NULL, the WHEN false,
-- and the foreign key refuses the insert instead. Both directions are loud.
CREATE TRIGGER drill_attempt_only_for_selected
BEFORE INSERT ON drill_attempt
WHEN (SELECT disposition FROM drill_candidate WHERE candidate_id = NEW.candidate_id) <> 'selected'
BEGIN
    SELECT RAISE(ABORT, 'shadow: an attempt may only be planned for a selected candidate');
END;

-- Only invocation_id may ever be UPDATEd. UPDATE OF fires on a column being
-- NAMED, not on its value changing, so a writer that copies a struct back
-- cannot drift into re-pointing an attempt at another candidate or another
-- mode. This is exec_invocation_write_once one axis up (divergence c).
CREATE TRIGGER drill_attempt_write_once
BEFORE UPDATE OF candidate_id, supply_mode, planned_at ON drill_attempt
BEGIN
    SELECT RAISE(ABORT, 'shadow: only drill_attempt.invocation_id may be written after planning');
END;

-- ...and invocation_id itself is written ONCE. Re-pointing a planned attempt
-- at a second invocation would let a bad run be replaced by a better one after
-- the fact, which is `purpose` post-hoc labelling with a different spelling.
CREATE TRIGGER drill_attempt_invocation_write_once
BEFORE UPDATE ON drill_attempt
WHEN OLD.invocation_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'shadow: drill_attempt.invocation_id is written once');
END;

-- ---------------------------------------------------------------------------
-- AND THE PRICE OF LINKING BY ADDITION, WRITTEN DOWN WHERE THE TABLES ARE.
--
-- exec_invocation has no candidate_id, so the two axes meet only through a
-- JOIN — and what that invites was MEASURED. On one ledger with a single
-- unclaimed mismatched `online` run, reading the gate from
-- exec_invocation gives 80.0% and reading it through an INNER JOIN on
-- drill_attempt gives 100.0%: the join SILENTLY DROPS the unmatched row and the
-- gate passes.
--
-- So: the gate reads A/D/M/X from exec_invocation and from nowhere else. This
-- axis is a CROSS-CHECK, and the two readings agreeing is exactly the fifth
-- audit query coming back empty.
-- ---------------------------------------------------------------------------
