-- 0001_shadow_init — Phase 1's shadow ledger.
--
-- THIS IS shadow.db. Phase 0's gateway.db is NOT TOUCHED, for two independent
-- reasons that both still bind:
--
--   1. gateway.db's `event_type` enum is a closed set hand-written in five
--      places, four of which fail SILENTLY. A shadow run is not a trigger;
--      putting it in that family would widen the family's MEANING and inherit
--      the trap.
--   2. gateway.db's schema_version and migration list belong to Phase 0.
--      Phase 1's tables there would make every Phase 1 schema change a change
--      to Phase 0's schema.
--
-- The price was paid and is named: NO JOIN with trigger_events. A shadow run
-- is tied to a trigger by FIELDS (repo, workflow_path, commit_sha), never by a
-- foreign key.
--
-- What the rows are counted for: the promotion gate reads, per supply mode,
-- A (drill runs attempted), D (attempts that reached `completed`, the
-- denominator), M (completed runs whose result matched GitHub's) and
-- X = A - D (exclusions, each paid out of the gate's budget). None of these is
-- stored; all are derived from the rows. internal/shadow/doc.go and audit.go
-- describe the queries.

CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,   -- sha256 of the migration source, hex
    applied_at INTEGER NOT NULL    -- unix-millis
);

-- ---------------------------------------------------------------------------
-- THE DAY AXIS
--
-- There is a row for EVERY day of the window: the ABSENCE of a row cannot be
-- read as "the drill ran", it is a RECORDING DEFECT. "No drill ever ran" and
-- "drills ran and agreed" must never look the same in the ledger.
--
-- No counts are stored. A/D/X are derivable per day by grouping
-- exec_invocation on drill_day; a maintained tally is a second copy. The only
-- thing that is NOT derivable is WHY an absence is empty, and that is all this
-- table holds.
-- ---------------------------------------------------------------------------
CREATE TABLE drill_day (
    -- 'YYYY-MM-DD'. The day is drawn in ONE timezone, and that timezone comes
    -- from configuration, NOT from the host's locale. Which zone a day was
    -- drawn in is recorded separately, in drill_day_zone (0003).
    --
    -- The GLOB is what stops a caller handing this column a timestamp or a
    -- host-formatted string with an offset; without it a wrong-shaped day is a
    -- new day, silently, and the window grows.
    day          TEXT    PRIMARY KEY
                 CHECK (day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    status       TEXT    NOT NULL CHECK (status IN ('ran', 'dropped')),
    -- Closed set. All four drop the day and all four come out of the SAME 7-day
    -- budget; but "GitHub was gone" and "nobody pushed" are counted separately.
    drop_reason  TEXT
                 CHECK (drop_reason IS NULL OR drop_reason IN (
                        'github_unreachable',     -- the event the product exists for
                        'executor_unavailable',   -- executor / host side
                        'no_candidate_run',       -- GitHub up, no candidate run
                        'operator_suspended')),   -- the drill was stopped on purpose
    -- A reasonless dropped day cannot be written; a ran day cannot carry one.
    CHECK (status <> 'dropped' OR drop_reason IS NOT NULL),
    CHECK (status <> 'ran'     OR drop_reason IS NULL)
);

-- The day key itself is never rewritten: renaming a day moves every run that
-- references it, and the FK would not notice.
CREATE TRIGGER drill_day_key_immutable
BEFORE UPDATE OF day ON drill_day
BEGIN
    SELECT RAISE(ABORT, 'shadow: drill_day.day is immutable');
END;

-- 'dropped' -> 'ran' is legal: a day recorded as dropped in the morning can
-- turn out to have run. The reverse is FORBIDDEN. A 'ran' day relabelled
-- 'dropped' INVENTS an absence reason for a day that was not absent — the day
-- axis's copy of "purpose is written before the run and never rewritten".
CREATE TRIGGER drill_day_no_downgrade
BEFORE UPDATE OF status ON drill_day
WHEN OLD.status = 'ran' AND NEW.status = 'dropped'
BEGIN
    SELECT RAISE(ABORT, 'shadow: a ran day cannot be relabelled dropped');
END;

-- ---------------------------------------------------------------------------
-- THE RUN AXIS
-- ---------------------------------------------------------------------------
CREATE TABLE exec_invocation (
    invocation_id         TEXT    PRIMARY KEY,
    schema_version        INTEGER NOT NULL DEFAULT 1,
    backend_name          TEXT    NOT NULL,          -- backends[].name
    executor_kind         TEXT    NOT NULL,          -- 'act'
    executor_version      TEXT    NOT NULL,          -- the version OBSERVED
    executor_pin_ok       INTEGER NOT NULL CHECK (executor_pin_ok IN (0, 1)),
    started_at            INTEGER NOT NULL,          -- unix-millis
    -- NULL = the process never finished (the fifth outcome state). It is reached
    -- with outcome='deadline_exceeded': the wall clock bounded it, so "never
    -- finished" still lands in a bucket instead of becoming a third one.
    finished_at           INTEGER,
    repo                  TEXT    NOT NULL,
    workflow_path         TEXT    NOT NULL,
    commit_sha            TEXT    NOT NULL CHECK (length(commit_sha) = 40),
    event_name            TEXT    NOT NULL,
    event_payload_sha256  TEXT    NOT NULL,          -- the trace of an input-less replay
    -- 'drill'   = the gate's candidate (a GitHub side EXISTS).
    -- 'outage'  = work actually carried during an outage (no GitHub side, and
    --             there never will be one).
    -- It has NO DEFAULT and is written BEFORE the run. If it could be labelled
    -- afterwards, a drill that went badly could be declared an outage and
    -- escape the gate's budget. exec_invocation_append_only below is what makes
    -- that a mechanism instead of a sentence.
    purpose               TEXT    NOT NULL
                          CHECK (purpose IN ('drill', 'outage')),
    -- Which day a run is written to is a DECISION (the timezone), not a
    -- derivation. The FK forces every run to land on a RECORDED day.
    drill_day             TEXT    NOT NULL REFERENCES drill_day(day),
    -- This is the gate's SCORE AXIS, not merely a record field:
    -- A/D/M/X are kept per mode and D >= 20 is satisfied per mode.
    supply_mode           TEXT    NOT NULL
                          CHECK (supply_mode IN ('online', 'offline')),
    -- The executor's argv AND the process's working directory: the hash is
    -- taken over `cd <WorkDir> && env -i ... act ...`. A hash that leaves the
    -- directory out describes two runs in two different repositories
    -- identically.
    argv_sha256           TEXT    NOT NULL,
    workflow_norm_sha256  TEXT    NOT NULL,          -- the text HANDED to the engine
    workflow_norm_applied INTEGER NOT NULL CHECK (workflow_norm_applied IN (0, 1)),
    workdir_clean         INTEGER NOT NULL CHECK (workdir_clean IN (0, 1)),
    workdir_commit_count  INTEGER,                   -- keeps short-history checkouts visible
    workdir_shallow       INTEGER CHECK (workdir_shallow IS NULL OR workdir_shallow IN (0,1)),
    outcome               TEXT    NOT NULL
                          CHECK (outcome IN ('completed', 'refused', 'harness_error',
                                             'unmeasurable', 'deadline_exceeded')),
    refusal_reason        TEXT
                          CHECK (refusal_reason IS NULL OR refusal_reason IN (
                                 'image_unverified', 'platform_unmapped',
                                 'executor_version_mismatch', 'token_absent',
                                 'workdir_dirty', 'normalisation_failed',
                                 'local_repository_unresolved',
                                 'event_payload_incomplete', 'unsupported_platform_line',
                                 'job_without_result', 'step_set_unavailable',
                                 'plan_disagrees_with_github', 'job_identity_unmapped',
                                 'conclusion_unmappable')),
    -- Run-level conclusion. NEVER 'in_progress': that is a value of GitHub's
    -- `status` field, not `conclusion`, and it is not admitted here.
    -- 'unmappable' is not admitted either: a run whose conclusion could not be
    -- mapped is unmeasurable, not concluded.
    run_conclusion        TEXT
                          CHECK (run_conclusion IS NULL OR run_conclusion IN (
                                 'success', 'failure', 'cancelled', 'skipped',
                                 'timed_out', 'action_required', 'neutral', 'stale')),
    -- A conclusion is derived only if the validator passed.
    CHECK (outcome <> 'completed' OR run_conclusion IS NOT NULL),
    CHECK (outcome =  'completed' OR run_conclusion IS NULL),
    -- Every non-completed outcome carries a reason. 'deadline_exceeded'
    -- names its own cause and must NOT also take a refusal_reason: a cause held
    -- in two places is a cause one of which goes stale.
    CHECK (outcome IN ('completed', 'deadline_exceeded') OR refusal_reason IS NOT NULL),
    CHECK (outcome NOT IN ('completed', 'deadline_exceeded') OR refusal_reason IS NULL)
);

CREATE INDEX idx_exec_invocation_day_purpose ON exec_invocation(drill_day, purpose);
CREATE INDEX idx_exec_invocation_started     ON exec_invocation(started_at);

-- `purpose` is written before the run and never rewritten. The whole ROW is
-- append-only, which is the same discipline gateway.db holds for its trigger
-- log: an observation is written once. There is deliberately no
-- superseded_by here, because an invocation has no correction path — a second
-- run is a second invocation_id.
CREATE TRIGGER exec_invocation_append_only
BEFORE UPDATE ON exec_invocation
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_invocation is append-only (purpose is written before the run and never rewritten)');
END;

CREATE TABLE exec_image (
    invocation_id     TEXT    NOT NULL REFERENCES exec_invocation(invocation_id),
    runs_on_label     TEXT    NOT NULL,
    local_tag         TEXT    NOT NULL,
    -- THE CONFIGURED SOURCE DIGEST — the value identity was asserted AGAINST —
    -- and NOT the digest recomputed locally. Under `create_commit` the two
    -- already differ, because `docker commit` recomputes `Id` and
    -- `RepoDigests`. Writing the local value here would turn the record from
    -- "what we ran" into "what we produced", the opposite of what a provenance
    -- record is for.
    digest            TEXT    NOT NULL,
    architecture      TEXT    NOT NULL CHECK (architecture <> ''),  -- may not be empty
    config_nonempty   INTEGER NOT NULL CHECK (config_nonempty IN (0, 1)),
    path_present      INTEGER NOT NULL CHECK (path_present IN (0, 1)),
    PRIMARY KEY (invocation_id, runs_on_label)
);

CREATE TRIGGER exec_image_append_only
BEFORE UPDATE ON exec_image
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_image is append-only');
END;

-- For every --local-repository style mapping, the HEAD read BEFORE the run.
-- Why: a MISSING mirror fails loudly, but a STALE one produces a clean
-- `success` — a mirror rewound ~25 commits ran a four-year-old action with no
-- warning at all (measured). `act` writes the matched PATH at debug level and
-- the RESOLVED REVISION nowhere, so if it is not written here it exists nowhere.
-- This table makes staleness AUDITABLE, not NOISY.
CREATE TABLE exec_local_repository (
    invocation_id  TEXT NOT NULL REFERENCES exec_invocation(invocation_id),
    url            TEXT NOT NULL,     -- left side of the mapping
    local_path     TEXT NOT NULL,     -- right side
    -- If it cannot be read the row is not written: the run is refused at
    -- preflight with local_repository_unresolved. There is no NULL SHA.
    resolved_sha   TEXT NOT NULL CHECK (length(resolved_sha) = 40),
    PRIMARY KEY (invocation_id, url)
);

CREATE TRIGGER exec_local_repository_append_only
BEFORE UPDATE ON exec_local_repository
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_local_repository is append-only');
END;

CREATE TABLE exec_job_result (
    invocation_id  TEXT    NOT NULL REFERENCES exec_invocation(invocation_id),
    job_id         TEXT    NOT NULL,
    leg_key        TEXT    NOT NULL DEFAULT '',   -- '' = no matrix, OR the leg was not distinguished
    -- Whether the matrix leg could be told apart. For act 1 is EXPECTED: every
    -- record carries a typed `matrix` object (measured). 0 is no longer a NORM
    -- in act, it is an EVENT.
    leg_resolved   INTEGER NOT NULL CHECK (leg_resolved IN (0, 1)),
    source         TEXT    NOT NULL
                   CHECK (source IN ('executor', 'model')),  -- 'model' = act left no trace
    conclusion     TEXT    NOT NULL
                   CHECK (conclusion IN ('success', 'failure', 'cancelled', 'skipped',
                                         'timed_out', 'action_required', 'neutral',
                                         'stale', 'unmappable')),
    PRIMARY KEY (invocation_id, job_id, leg_key)
);

CREATE TRIGGER exec_job_result_append_only
BEFORE UPDATE ON exec_job_result
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_job_result is append-only');
END;

-- ---------------------------------------------------------------------------
-- THE STEP KEY IS step_id, NOT THE DECLARED ORDINAL.
--
-- The first design keyed this table on (invocation_id, job_id, leg_key,
-- ordinal) while `ordinal` is the DECLARED step order — and a composite
-- action's INNER steps live under one declared step. Driven: a workflow with
-- one composite step yields three records — `g`, `g.mk`, `g.1` — with NO
-- declared order between them. Three rows collapse onto one key: the write
-- either fails or two observations overwrite each other.
--
-- REPAIR: the key is `step_id`, not `ordinal`. The step join key is (jobID,
-- matrix, stage=="Main", stepID) and `Ordinal` is explicitly NOT part of it.
-- The old key was not wrong, it was OVER-APPLIED: its true half survives as
-- the partial unique index below.
--
-- WHAT THAT DOES TO `ordinal`: it stops being an identity and becomes an
-- ANNOTATION. It is no longer unique, and 0 repeats freely — 0 means "the
-- engine's channel carried no declared index for this record", which is the
-- case for every `id:`-bearing step and every composite inner step. 0 is
-- recorded rather than reconstructed from emission order on purpose:
-- reconstructing it shifts every later ordinal by one, silently, the moment a
-- declared step emits nothing — and whether such a step can exist is still an
-- open question. An `ORDER BY ordinal` over this table is therefore NOT a step
-- order; it puts every unindexed record first.
-- ---------------------------------------------------------------------------
CREATE TABLE exec_step_observation (
    invocation_id  TEXT    NOT NULL REFERENCES exec_invocation(invocation_id),
    job_id         TEXT    NOT NULL,
    leg_key        TEXT    NOT NULL DEFAULT '',
    -- DECLARED step order, 1-based. 0 = no declared index was carried.
    -- NOT a key, NOT unique, and not a position — see the block above.
    ordinal        INTEGER NOT NULL CHECK (ordinal >= 0),
    -- The stepID act emits, canonicalised: the `id:` when one was declared,
    -- otherwise the declared index (measured, and read in act's
    -- job_executor.go). A composite inner step is the multi-element key
    -- joined with dots.
    -- Recorded so that the claim "the model computed the same key" can be
    -- AUDITED afterwards.
    --
    -- It may not be empty, and it deliberately has no `DEFAULT ''`: an
    -- empty identity on a key column is the same collision one level down, and
    -- a step with no identity cannot be joined at all — that run is
    -- harness_error / step_set_unavailable, not a row here.
    step_id        TEXT    NOT NULL CHECK (step_id <> ''),
    step_name      TEXT    NOT NULL DEFAULT '',
    ran            INTEGER NOT NULL CHECK (ran IN (0, 1)),
    conclusion     TEXT
                   CHECK (conclusion IS NULL OR conclusion IN ('success', 'failure',
                          'cancelled', 'skipped', 'timed_out', 'neutral', 'unmappable')),
    PRIMARY KEY (invocation_id, job_id, leg_key, step_id)
);

-- The true half of the old primary key. Two DECLARED steps may not claim the
-- same declared index — that is an off-by-one join: an implementation that
-- treats the two numberings as one pairs steps one position off with no
-- error. Restricted to ordinal > 0 because 0 means "no index carried" and
-- repeats by design.
CREATE UNIQUE INDEX idx_step_declared_ordinal
    ON exec_step_observation (invocation_id, job_id, leg_key, ordinal)
    WHERE ordinal > 0;

CREATE TRIGGER exec_step_observation_append_only
BEFORE UPDATE ON exec_step_observation
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_step_observation is append-only');
END;

-- act leaks containers and volumes for FAILED jobs: after a single pass, 16
-- containers and 31 volumes; only SUCCESSFUL jobs write a cleanup log
-- (measured). A second, independent measurement gave a RATE: one container
-- and two volumes per failed job; ~15 runs accumulated 5 containers and 11
-- volumes. What this table counts is a MEASURED quantity, not a possibility.
-- This table only COUNTS. Cleanup policy belongs to the executor pool's design
-- and is NOT here.
CREATE TABLE exec_residue (
    invocation_id  TEXT NOT NULL REFERENCES exec_invocation(invocation_id),
    kind           TEXT NOT NULL CHECK (kind IN ('container', 'volume')),
    ref            TEXT NOT NULL,
    observed_at    INTEGER NOT NULL,
    PRIMARY KEY (invocation_id, kind, ref)
);

CREATE TRIGGER exec_residue_append_only
BEFORE UPDATE ON exec_residue
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_residue is append-only');
END;

-- ---------------------------------------------------------------------------
-- THE ENGINE'S FREE NOTES, given a home.
--
-- Provenance carries the engine's FREE NOTES, and the `.actrc` finding is
-- produced there and REPORTED — `act` can inject flags from its own config
-- file, so "the caller can neither add nor remove a flag" is true of the
-- CALLER and false of the HOST. The first schema design had no column for it,
-- so a field the executor produces was recorded NOWHERE, and "visible" meant
-- "visible in the run's own stdout" — gone the moment the process exits.
--
-- A TABLE rather than a column, for one reason: instead of a fifteenth
-- refusal reason, the decision was that anything appearing here REPEATEDLY
-- gets reported. Repeatedly is a COUNT, and a newline-joined TEXT column
-- cannot be counted. `GROUP BY note` can.
--
-- The notes are free text and are stored VERBATIM. Nothing here redacts them;
-- that obligation is the executor's, and Provenance.ArgvRedacted is the only
-- field which carries a stated redaction guarantee.
-- ---------------------------------------------------------------------------
CREATE TABLE exec_note (
    invocation_id  TEXT    NOT NULL REFERENCES exec_invocation(invocation_id),
    seq            INTEGER NOT NULL CHECK (seq >= 0),   -- emission order, 0-based
    note           TEXT    NOT NULL CHECK (note <> ''),
    PRIMARY KEY (invocation_id, seq)
);

CREATE TRIGGER exec_note_append_only
BEFORE UPDATE ON exec_note
BEGIN
    SELECT RAISE(ABORT, 'shadow: exec_note is append-only');
END;
