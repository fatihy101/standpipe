-- 0001_init — Phase 0 baseline schema.
--
-- The idempotency reservations, the append-only trigger log (trigger_events),
-- the workflow definition cache and the GitHub run cache. The only requirement
-- behind hmac_keys is that the inputs_hash HMAC key live in the database and
-- carry a version prefix; the table's shape is this package's choice.

CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,   -- sha256 of the migration source, hex
    applied_at INTEGER NOT NULL    -- unix-millis
);

-- One row per (caller, key): the reservation taken before any dispatch.
CREATE TABLE idempotency (
    caller_name       TEXT    NOT NULL,    -- namespace key
    idempotency_key   TEXT    NOT NULL,
    caller_token_id   TEXT    NOT NULL,    -- audit trail only, never part of the PK
    request_path      TEXT    NOT NULL,    -- METHOD + path: "POST /t/acme/core/release.yml"
    body_sha256       BLOB    NOT NULL,    -- SHA-256 of the canonical body, 32 bytes
    state             TEXT    NOT NULL
                      CHECK (state IN ('in_flight', 'complete', 'unknown')),
    response_status   INTEGER,
    response_body     TEXT,
    event_id          TEXT    NOT NULL,    -- owning trigger event
    attempt_started_at  INTEGER NOT NULL,  -- unix-millis, moment of queue entry
    dispatch_started_at INTEGER,           -- unix-millis, moment the request left for GitHub
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL,
    PRIMARY KEY (caller_name, idempotency_key)
);

CREATE INDEX idx_idempotency_expires ON idempotency(expires_at);

-- Supports the crash-recovery sweep without scanning the whole table.
CREATE INDEX idx_idempotency_in_flight ON idempotency(state, attempt_started_at)
    WHERE state = 'in_flight';

-- The append-only trigger log. Every event type shares this table; a column
-- a type does not use is NULL on that type's rows.
CREATE TABLE trigger_events (
    event_id              TEXT    PRIMARY KEY,
    schema_version        INTEGER NOT NULL DEFAULT 1,
    event_type            TEXT    NOT NULL DEFAULT 'trigger'
                          CHECK (event_type IN ('trigger', 'trigger_outcome',
                                                'trigger_rejected',
                                                'run_link', 'run_observed')),
    ts                    INTEGER NOT NULL,  -- unix-millis
    origin_forge          TEXT    NOT NULL DEFAULT 'github',
    epoch                 INTEGER NOT NULL DEFAULT 0,
    content_sha256        TEXT    NOT NULL,
    -- trigger fields (NULL on the other types)
    caller_token_id       TEXT,
    caller_name           TEXT,
    caller_source_ip      TEXT,       -- redactable
    caller_user_agent     TEXT,       -- redactable
    target_project        TEXT,
    target_workflow       TEXT,
    target_ref            TEXT,
    inputs_hash           TEXT,       -- 'hmac-sha256:<key_id>:...'
    inputs_validation     TEXT
                          CHECK (inputs_validation IS NULL OR
                                 inputs_validation IN ('default_branch', 'exact_ref',
                                                       'skipped_cold_cache',
                                                       'skipped_non_default_ref')),
    correlation           TEXT        -- required on 'trigger', NULL on every other type
                          CHECK (correlation IS NULL OR
                                 correlation IN ('injected', 'dispatch_response',
                                                 'window')),
    idempotency_key       TEXT,
    idempotency_outcome   TEXT,       -- 'new', 'replay', 'conflict'
    require_raw           TEXT,       -- JSON or NULL
    decision_backend      TEXT,
    decision_reason       TEXT,
    outcome_status        TEXT        -- always NULL on 'trigger';
                                      -- carried by 'trigger_outcome'
                          CHECK (outcome_status IS NULL OR
                                 outcome_status IN ('forwarded', 'rejected', 'error')),
    outcome_reason        TEXT,
    outcome_http_status   INTEGER,
    -- trigger_rejected fields (NULL on every other type)
    reject_reason         TEXT        -- required on 'trigger_rejected'
                          CHECK (reject_reason IS NULL OR
                                 reject_reason IN ('rate_limited', 'caller_policy',
                                                   'not_in_allowlist',
                                                   'default_branch_unknown',
                                                   'bad_request', 'unsupported_inputs',
                                                   'secret_detected')),
    suppressed_count      INTEGER,    -- only when reject_reason = 'rate_limited'
    suppressed_since      INTEGER,    -- unix-millis, only when 'rate_limited'
    outcome_upstream_duration_ms INTEGER,
    reason_text           TEXT,       -- redactable, max 1024 characters
    request_id            TEXT,
    -- run_link fields
    trigger_event_id      TEXT        -- parent event for run_link, run_observed, trigger_outcome
                          REFERENCES trigger_events(event_id),
    run_id                INTEGER,    -- run_link: matched run; NULL when resolution = 'none'
    resolution            TEXT
                          CHECK (resolution IS NULL OR
                                 resolution IN ('exact', 'inferred', 'ambiguous', 'none')),
    resolution_source     TEXT        -- required on 'run_link', NULL elsewhere
                          CHECK (resolution_source IS NULL OR
                                 resolution_source IN ('dispatch_response', 'run_name',
                                                       'window')),
    superseded_by         TEXT,       -- run_link: a stronger resolution invalidated this row
    -- run_observed fields
    conclusion            TEXT,
    -- correlation is only meaningful on a trigger row, and there it is required
    CHECK (event_type <> 'trigger' OR correlation IS NOT NULL),
    -- resolution_source is only meaningful on a run_link row, and there it is required
    CHECK (event_type <> 'run_link' OR resolution_source IS NOT NULL),
    -- trigger_rejected: reason and HTTP status are required, and it has no parent
    CHECK (event_type <> 'trigger_rejected' OR
           (reject_reason IS NOT NULL AND outcome_http_status IS NOT NULL
            AND trigger_event_id IS NULL)),
    -- suppressed_* may only be populated on a coalesced 429 row
    CHECK ((suppressed_count IS NULL AND suppressed_since IS NULL) OR
           (reject_reason = 'rate_limited' AND suppressed_count >= 1
            AND suppressed_since IS NOT NULL))
);

CREATE INDEX idx_trigger_events_ts ON trigger_events(ts);
CREATE INDEX idx_trigger_events_caller ON trigger_events(caller_token_id);
CREATE INDEX idx_trigger_events_target ON trigger_events(target_project, target_workflow);
CREATE INDEX idx_trigger_events_outcome ON trigger_events(outcome_status);
CREATE INDEX idx_trigger_events_type ON trigger_events(event_type);
CREATE INDEX idx_trigger_events_trigger_ref ON trigger_events(trigger_event_id)
    WHERE trigger_event_id IS NOT NULL;
CREATE INDEX idx_trigger_events_content ON trigger_events(content_sha256);

-- The reject log is read by caller + time (`standpipe status`, `standpipe trace`)
CREATE INDEX idx_trigger_rejected_caller ON trigger_events(caller_name, ts)
    WHERE event_type = 'trigger_rejected';

-- At most one active (non-superseded) run_link per trigger_event_id
CREATE UNIQUE INDEX idx_run_link_unique
    ON trigger_events(trigger_event_id)
    WHERE event_type = 'run_link' AND superseded_by IS NULL;

-- The coverage query joins run_link rows on run_id, and the run_observed
-- producer looks up the active run_link for a run_id after every github_runs
-- upsert. Without this the join degrades
-- to a scan of the whole event log.
CREATE INDEX idx_trigger_events_run_link_run
    ON trigger_events(run_id)
    WHERE event_type = 'run_link' AND superseded_by IS NULL;

-- Workflow definitions read from GitHub: triggers, dispatch inputs and the
-- default branch, per ref.
CREATE TABLE workflow_cache (
    repo            TEXT    NOT NULL,
    workflow        TEXT    NOT NULL,
    ref             TEXT    NOT NULL DEFAULT 'default',
    workflow_id     INTEGER NOT NULL,
    triggers        TEXT    NOT NULL,   -- JSON array: ["workflow_dispatch", "push"]
    inputs_def      TEXT,               -- JSON: workflow_dispatch.inputs definition
    default_branch  TEXT,
    fetched_at      INTEGER NOT NULL,   -- unix-millis
    PRIMARY KEY (repo, workflow, ref)
);

-- The workflow runs GitHub reports, as last fetched.
CREATE TABLE github_runs (
    run_id           INTEGER PRIMARY KEY,
    repo             TEXT    NOT NULL,
    workflow_id      INTEGER NOT NULL,
    workflow         TEXT    NOT NULL,
    event            TEXT    NOT NULL,
    head_branch      TEXT,
    head_branch_normalized TEXT,  -- with the refs/heads/ prefix restored
    actor            TEXT,
    triggering_actor TEXT,
    conclusion       TEXT,
    created_at       INTEGER NOT NULL,  -- unix-millis
    fetched_at       INTEGER NOT NULL   -- unix-millis
);

CREATE INDEX idx_github_runs_repo_workflow ON github_runs(repo, workflow);
CREATE INDEX idx_github_runs_event ON github_runs(event);
CREATE INDEX idx_github_runs_created ON github_runs(created_at);

-- The inputs_hash HMAC key is persistent and identified by a RANDOM key id,
-- not a counter. Exactly one row is active; the id is the prefix in
-- 'hmac-sha256:<key_id>:<hex>' so older values stay verifiable after a
-- rotation.
--
-- The id must not be a counter: the key lives inside the backup snapshot.
-- Restoring a snapshot taken before a key was minted makes the gateway mint a
-- new one, and a counter would label it 'v1' again — old values from the
-- previous v1 key would then be reported as "hash mismatch" (data corruption)
-- instead of "the key that produced this value is not in this database".
CREATE TABLE hmac_keys (
    key_id     TEXT    PRIMARY KEY,   -- 16 hex characters from a CSPRNG
    key        BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    active     INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1))
);

CREATE UNIQUE INDEX idx_hmac_keys_active ON hmac_keys(active) WHERE active = 1;

-- The idempotency purge persists its own wall-clock time so the next run can
-- notice a forward clock jump. Key in use: 'idempotency_purge_last_run_ms'.
CREATE TABLE gateway_meta (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL       -- unix-millis
);
