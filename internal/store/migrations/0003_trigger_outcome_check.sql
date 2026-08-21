-- 0003_trigger_outcome_check — a `trigger` row structurally has no outcome.
--
-- A trigger row's hash scope takes `outcome` as a constant null, because the
-- result of a trigger lives in its own `trigger_outcome` event
-- (docs/reference/hash-scopes.md). This migration adds the CHECK that keeps
-- that constant true, which in SQLite means a table rebuild.
--
-- Why a CHECK here and a scope widening for trigger_outcome's origin_forge /
-- epoch, when both holes had the same shape ("the scope assumes a constant the
-- schema does not enforce"): a scope constant is defended by a CHECK only when
-- the constancy is itself a DESIGN DECISION. That a trigger row has no outcome
-- is one, so the constant null stands and the column is what must be forced.
-- origin_forge and epoch, by contrast, are on every event precisely so a later
-- phase can carry other values there, so freezing them with a CHECK would
-- cancel the reason they exist — there the column is today's fact and the
-- SCOPE is what widens.
--
-- Why a whole table rebuild: SQLite has no ALTER TABLE ... ADD CONSTRAINT. A
-- CHECK can only be added by creating a new table, copying, dropping and
-- renaming. The copy failing is the CORRECT outcome: a row that does not
-- satisfy the new CHECK is exactly the row whose content_sha256 cannot be
-- recomputed, and stopping the migration is how the operator learns of it
-- instead of inheriting it. The CHECK is named, so that
-- failure reads `CHECK constraint failed: trigger_row_has_no_outcome` rather
-- than naming a scratch table the operator has never heard of.
--
-- Why forward-only rather than editing 0001_init: the baseline's checksum is
-- recorded in every database that has already applied it
-- (verifyAppliedChecksums), so editing it would make every existing local
-- database report a tampered migration on the next start. Same reason as
-- 0002_prior_dispatch.
--
-- All four outcome_* columns, not just outcome_status: the scope has ONE
-- constant null standing for the whole `outcome` object, so a row carrying
-- only outcome_http_status is just as undescribable by that scope as one
-- carrying a status.
--
-- PRAGMA defer_foreign_keys is used rather than PRAGMA foreign_keys=OFF: every
-- connection is opened with foreign_keys ON (sqlite.go), trigger_events has a
-- SELF-referential foreign key (trigger_event_id -> event_id), and
-- `foreign_keys` is a no-op inside a transaction, which is where a migration
-- runs. defer_foreign_keys is the one that works there: it postpones
-- enforcement to COMMIT and resets itself at COMMIT or ROLLBACK. It keeps the
-- copy from having to be ordered parents-first and keeps the DROP's implicit
-- DELETE from tripping the self-reference. The constraints are still checked —
-- at COMMIT, against the finished table.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE trigger_events_new (
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
                          REFERENCES trigger_events_new(event_id),
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
    -- A trigger row's outcome does not exist structurally; it is carried by
    -- the trigger_outcome event. The hash scope takes `outcome` as a constant
    -- null, and this is what keeps that constant honest. All four, because
    -- the one constant in the scope stands for all four columns.
    CONSTRAINT trigger_row_has_no_outcome
    CHECK (event_type <> 'trigger' OR
           (outcome_status IS NULL AND outcome_reason IS NULL
            AND outcome_http_status IS NULL
            AND outcome_upstream_duration_ms IS NULL)),
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

-- rowid is carried across explicitly. Nothing in this package orders by it,
-- but a rebuild that silently renumbers rows is the kind of difference that
-- only shows up in an export diff months later.
INSERT INTO trigger_events_new (
    rowid,
    event_id, schema_version, event_type, ts, origin_forge, epoch, content_sha256,
    caller_token_id, caller_name, caller_source_ip, caller_user_agent,
    target_project, target_workflow, target_ref,
    inputs_hash, inputs_validation, correlation,
    idempotency_key, idempotency_outcome, require_raw,
    decision_backend, decision_reason,
    outcome_status, outcome_reason, outcome_http_status,
    reject_reason, suppressed_count, suppressed_since,
    outcome_upstream_duration_ms, reason_text, request_id,
    trigger_event_id, run_id, resolution, resolution_source, superseded_by,
    conclusion
)
SELECT
    rowid,
    event_id, schema_version, event_type, ts, origin_forge, epoch, content_sha256,
    caller_token_id, caller_name, caller_source_ip, caller_user_agent,
    target_project, target_workflow, target_ref,
    inputs_hash, inputs_validation, correlation,
    idempotency_key, idempotency_outcome, require_raw,
    decision_backend, decision_reason,
    outcome_status, outcome_reason, outcome_http_status,
    reject_reason, suppressed_count, suppressed_since,
    outcome_upstream_duration_ms, reason_text, request_id,
    trigger_event_id, run_id, resolution, resolution_source, superseded_by,
    conclusion
FROM trigger_events;

DROP TABLE trigger_events;

-- The rename rewrites the self-reference above (REFERENCES trigger_events_new)
-- back to REFERENCES trigger_events, which is why legacy_alter_table must stay
-- off. Verified by reading sqlite_master back after this migration.
ALTER TABLE trigger_events_new RENAME TO trigger_events;

-- DROP TABLE took the 0001 indexes with it; they are recreated here with the
-- same names and definitions.
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
-- upsert.
CREATE INDEX idx_trigger_events_run_link_run
    ON trigger_events(run_id)
    WHERE event_type = 'run_link' AND superseded_by IS NULL;
