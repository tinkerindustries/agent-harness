package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id                TEXT PRIMARY KEY,
	parent_id         TEXT,
	job_type          TEXT NOT NULL DEFAULT 'implementation',
	task              TEXT NOT NULL DEFAULT '',
	title             TEXT NOT NULL DEFAULT '',
	description       TEXT NOT NULL DEFAULT '',
	phase             INTEGER NOT NULL DEFAULT 0,
	total_phases      INTEGER NOT NULL DEFAULT 0,
	parent_agent_type TEXT NOT NULL DEFAULT '',
	parent_agent_id   TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL,
	prompt_variant    TEXT NOT NULL DEFAULT '',
	effort            TEXT NOT NULL,
	thinking          INTEGER NOT NULL,
	workspace         TEXT NOT NULL,
	permission_mode   TEXT NOT NULL,
	deny_patterns     TEXT NOT NULL DEFAULT '[]',
	system_prompt     TEXT NOT NULL,
	tool_schema       TEXT NOT NULL,
	result_schema     TEXT,
	status            TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	finished_at       TEXT,
	version           INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS events (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	payload    TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS work_requests (
	request_id     TEXT PRIMARY KEY,
	session_id     TEXT,
	status         TEXT NOT NULL,
	result         TEXT,
	received_at    TEXT NOT NULL,
	finished_at    TEXT,
	delivery_count INTEGER NOT NULL DEFAULT 0,
	version        INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS workspace_leases (
	workspace    TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL,
	version      INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- An eval run and the sessions it compares (docs/EVALS.md). suite_json is the
-- suite as it was loaded: the file under evals/ changes, and a run has to keep
-- saying what it actually ran.
CREATE TABLE IF NOT EXISTS eval_runs (
	id          TEXT PRIMARY KEY,
	suite       TEXT NOT NULL,
	suite_json  TEXT NOT NULL,
	variants    TEXT NOT NULL,
	replicates  INTEGER NOT NULL,
	judge_model TEXT NOT NULL DEFAULT '',
	note        TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	started_at  TEXT NOT NULL,
	finished_at TEXT,
	version     INTEGER NOT NULL DEFAULT 1
);

-- One image a work request carries (docs/DATA-API.md). The bytes live in the
-- database, never inline in the work request: the store is the authority the
-- disk mirror derives from, so harness export stays complete, and a request
-- never carries bytes that could blow its size. A producer writes one row
-- per attachment and the request carries the ids; the worker reads the rows
-- back and internal/workspace materialises them into scratch/attachments/
-- before the session starts.
CREATE TABLE IF NOT EXISTS attachments (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	mime_type  TEXT NOT NULL,
	data       BLOB NOT NULL,
	created_at TEXT NOT NULL
);

-- One member per (task, variant, replicate). scores and verdict are stored
-- rather than recomputed: deleting a session removes the event log a rescore
-- would read, and a judge verdict is a model call that cannot be repeated for
-- free. session_id carries no foreign key, matching work_requests — a deleted
-- session leaves the numbers and loses only the link.
CREATE TABLE IF NOT EXISTS eval_members (
	eval_run_id TEXT NOT NULL,
	request_id  TEXT NOT NULL,
	task_id     TEXT NOT NULL,
	variant     TEXT NOT NULL,
	replicate   INTEGER NOT NULL,
	session_id  TEXT,
	status      TEXT NOT NULL,
	scores      TEXT,
	verdict     TEXT,
	cost_usd    REAL NOT NULL DEFAULT 0,
	sub_turns   INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (eval_run_id, request_id)
);

-- The work queue: the WORK stream's successor (docs/QUEUE-MIGRATION-PLAN.md
-- §1.2). One row per enqueued request, claimed by the worker pool, acked by
-- deleting the row.
--
-- The primary key is a surrogate id, NOT request_id, on purpose: the WORK
-- stream never deduplicated requests, so two publishes of one request_id are
-- two independent messages, each with its own delivery counter starting at
-- 1. store.shouldClaim depends on exactly that — a second, independently
-- published message for the same request_id starts back at 1, which is what
-- makes a genuine race between two live attempts fall through to "still
-- owned elsewhere" instead of running twice. A request_id primary key with
-- INSERT OR IGNORE would collapse the second enqueue into the first and
-- destroy the RefusalOwnedElsewhere and RefusalSpent paths. One enqueue =
-- one row = one independent delivery counter. Do not "fix" this.
--
-- visible_at_ms and lease_expires_ms are integer Unix milliseconds, not the
-- RFC3339Nano TEXT every other table uses, on purpose: Go's RFC3339Nano
-- trims trailing zeros from the fraction, so "…:00Z" and "…:00.5Z" compare
-- as '.'(0x2E) < 'Z'(0x5A) — the half-second timestamp sorts BEFORE the
-- whole-second one. Elsewhere in this schema that is a cosmetic ordering
-- wart; here it is the claim predicate, so it would be a correctness bug.
-- enqueued_at stays RFC3339Nano TEXT because it is only ever displayed.
-- lease_expires_ms = 0 means unleased, so a single
-- "lease_expires_ms <= now_ms" covers both "never leased" and "lease
-- expired".
CREATE TABLE IF NOT EXISTS work_queue (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id       TEXT    NOT NULL,
	payload          TEXT    NOT NULL,
	enqueued_at      TEXT    NOT NULL,
	visible_at_ms    INTEGER NOT NULL,
	lease_expires_ms INTEGER NOT NULL DEFAULT 0,
	delivery_count   INTEGER NOT NULL DEFAULT 0
);

-- The operator's global MCP server registry (docs/MCP.md, "The table"). One
-- row per server, no per-request scoping, which is what makes enable/disable
-- a single toggle with an obvious meaning. tools_json is the tool list as it
-- was read the last time a probe against that server succeeded, not a live
-- read: the request head's tool array is built from this stored snapshot, so
-- a server that is enabled but unreachable when a session starts contributes
-- the tools it contributed last time rather than silently shrinking the
-- array — and the array is the frozen request head, so a resumed session
-- whose array shrank under it would invalidate its own prompt-cache prefix
-- (docs/MCP.md, "The tool array is built from a stored snapshot, never from
-- a live connection"). That asymmetry is why a failed probe
-- (internal/store/mcp.go, SaveMCPProbe) writes only probe_error and leaves
-- tools_json and probed_at untouched: the array a session builds must not
-- depend on whether a subprocess happened to start this minute.
CREATE TABLE IF NOT EXISTS mcp_servers (
	name           TEXT PRIMARY KEY,
	transport      TEXT NOT NULL,               -- 'stdio' or 'http'
	command        TEXT NOT NULL DEFAULT '',    -- stdio: the executable
	args           TEXT NOT NULL DEFAULT '[]',  -- stdio: JSON array of strings
	env            TEXT NOT NULL DEFAULT '{}',  -- stdio: JSON object of strings
	url            TEXT NOT NULL DEFAULT '',    -- http: the endpoint
	headers        TEXT NOT NULL DEFAULT '{}',  -- http: JSON object of strings
	enabled        INTEGER NOT NULL DEFAULT 1,
	allow_readonly INTEGER NOT NULL DEFAULT 0,
	allow_sampling INTEGER NOT NULL DEFAULT 0,  -- may this server spend model tokens?
	tools_json     TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	resources_json TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	prompts_json   TEXT NOT NULL DEFAULT '[]',  -- last successful probe
	instructions   TEXT NOT NULL DEFAULT '',    -- last successful probe's initialize instructions
	stale          INTEGER NOT NULL DEFAULT 0,  -- the server said its lists moved on since that probe
	probed_at      TEXT NOT NULL DEFAULT '',
	probe_error    TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1
);

-- Read paths: the session list's usage summary filters events down to two
-- kinds before scanning, and looks a session up by the request that
-- created it.
CREATE INDEX IF NOT EXISTS idx_events_session_kind ON events (session_id, kind);
CREATE INDEX IF NOT EXISTS idx_work_requests_session_id ON work_requests (session_id);

-- The claim scans visible_at_ms in (visible_at_ms, id) order and the
-- operator-facing queries look rows up by request_id.
CREATE INDEX IF NOT EXISTS idx_work_queue_claimable ON work_queue (visible_at_ms, id);
CREATE INDEX IF NOT EXISTS idx_work_queue_request_id ON work_queue (request_id);

-- The session page asks "which eval does this session belong to?" on every
-- load, which is the reverse of the membership table's own key.
CREATE INDEX IF NOT EXISTS idx_eval_members_session ON eval_members (session_id);
`

// Open opens (creating if needed) the SQLite database at path, applies the
// schema, and starts the writer goroutine.
func Open(path string) (*Store, error) {
	writeDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", path)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	readDB.SetMaxOpenConns(4)

	// busy_timeout goes first: it registers SQLite's busy handler before any
	// statement that might contend for the file, including journal_mode
	// itself. Two processes opening the same database for the first time
	// both race to switch it into WAL mode, and without a busy handler
	// already active that race can return SQLITE_BUSY immediately instead
	// of waiting.
	for _, db := range []*sql.DB{writeDB, readDB} {
		if err := setPragmasWithRetry(db); err != nil {
			writeDB.Close()
			readDB.Close()
			return nil, fmt.Errorf("store: set pragmas: %w", err)
		}
	}
	if _, err := writeDB.Exec(schema); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := migrateTableColumns(writeDB, "sessions", sessionMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate sessions table: %w", err)
	}
	if err := migrateTableColumns(writeDB, "work_requests", workRequestMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate work_requests table: %w", err)
	}
	if err := migrateTableColumns(writeDB, "workspace_leases", workspaceLeaseMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate workspace_leases table: %w", err)
	}
	if err := migrateTableColumns(writeDB, "mcp_servers", mcpServerMigrationColumns); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("store: migrate mcp_servers table: %w", err)
	}

	s := &Store{
		writeDB: writeDB,
		readDB:  readDB,
		jobs:    make(chan job),
		stopped: make(chan struct{}),
	}
	go s.writerLoop()
	return s, nil
}

// setPragmasWithRetry sets the pragmas Open needs, retrying on SQLITE_BUSY.
// The busy handler from the first successful PRAGMA busy_timeout is not yet
// active while that very statement runs, so two processes opening a fresh
// database at the same instant can still each see one immediate SQLITE_BUSY
// before the handler takes hold; a few short retries absorb that without
// requiring the caller to serialise process startup.
func setPragmasWithRetry(db *sql.DB) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") && !strings.Contains(err.Error(), "database is locked") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return err
}

// migrateTableColumns adds columns to a table created by an older binary. It
// reads the existing columns and adds only the missing ones, so it is a
// no-op on a database that already has them. ALTER TABLE ADD COLUMN with a
// constant NOT NULL default backfills existing rows in the same statement.
func migrateTableColumns(db *sql.DB, table string, columns []migrationColumn) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, col := range columns {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return err
		}
	}
	return nil
}

// migrationColumn is one column migrateTableColumns adds to a table created
// by an older binary.
type migrationColumn struct {
	name string
	def  string
}

// sessionMigrationColumns are the columns migrateTableColumns adds to a
// sessions table created by an older binary.
var sessionMigrationColumns = []migrationColumn{
	{"job_type", "TEXT NOT NULL DEFAULT 'implementation'"},
	// task: the job's description, the launching instruction of the run, so
	// the session list can say what a session is about without reading the
	// event log. Older rows default to the empty string, which the browser
	// renders as no description rather than guessing.
	{"task", "TEXT NOT NULL DEFAULT ''"},
	// title: the run's name, shown bold on the main page in place of the raw
	// prompt. Older rows default to the empty string, which the browser
	// renders as no bold title (the task line then carries the description)
	// rather than inventing one.
	{"title", "TEXT NOT NULL DEFAULT ''"},
	// description: what change the agent is making, shown under the title on
	// the main page. Older rows default to the empty string, which the
	// browser renders as the task fallback rather than an empty line.
	{"description", "TEXT NOT NULL DEFAULT ''"},
	// phase: this run's 1-based position in a multi-phase chain. Older rows
	// default to 0, which — with total_phases also 0 — means "not part of a
	// chain", so the browser shows no phase chip.
	{"phase", "INTEGER NOT NULL DEFAULT 0"},
	// total_phases: how many phases the chain has. Older rows default to 0,
	// which — with phase also 0 — means "not part of a chain".
	{"total_phases", "INTEGER NOT NULL DEFAULT 0"},
	{"parent_agent_type", "TEXT NOT NULL DEFAULT ''"},
	{"parent_agent_id", "TEXT NOT NULL DEFAULT ''"},
	// complete_status: the status argument to Complete, kept alongside the
	// session's own status so the session list can tell DONE from GAVE UP.
	// Older rows default to the empty string,
	// which the browser renders as the plain terminal status rather than
	// guessing.
	{"complete_status", "TEXT NOT NULL DEFAULT ''"},
	// plan: the JSON todos array of the latest TodoWrite call, so the
	// session list carries the live plan without re-walking the event log
	// and keeps it for finished sessions.
	// Older rows default to the empty string, which the browser renders as
	// "no plan section" rather than an empty list.
	{"plan", "TEXT NOT NULL DEFAULT ''"},
	// recent_tool_calls: the rolling roll of the last few tool calls. The
	// in-flight card no longer renders it; the column is kept because it is
	// written in the same store write as the plan.
	{"recent_tool_calls", "TEXT NOT NULL DEFAULT ''"},
	// summary: the summary argument the model gave Complete, its own
	// one-line account of the run, shown under the finished table's session
	// id. Older rows default to the empty
	// string, which the browser renders as no subtitle.
	{"summary", "TEXT NOT NULL DEFAULT ''"},
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
	// parent_is_user: producer-stamped provenance; older rows default to 0,
	// which is correct for essentially every historical row (the few "user"
	// rows predate the operator-name era and read as an unnamed person).
	{"parent_is_user", "INTEGER NOT NULL DEFAULT 0"},
	// prompt_variant: the name of the system prompt variant the session runs
	// under, so a resume keeps the variant's tool array and head. Older rows
	// default to the empty string, which is the shipped prompt.
	{"prompt_variant", "TEXT NOT NULL DEFAULT ''"},
}

// workRequestMigrationColumns are the columns migrateTableColumns adds to a
// work_requests table created by an older binary.
var workRequestMigrationColumns = []migrationColumn{
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
}

// workspaceLeaseMigrationColumns are the columns migrateTableColumns adds to
// a workspace_leases table created by an older binary.
var workspaceLeaseMigrationColumns = []migrationColumn{
	// version: the optimistic-concurrency counter every mutating write
	// checks and bumps (docs/DATA-API.md). Older rows default to 1, which is
	// also the version a freshly created row starts at.
	{"version", "INTEGER NOT NULL DEFAULT 1"},
}

// mcpServerMigrationColumns are the columns migrateTableColumns adds to an
// mcp_servers table created by an older binary.
var mcpServerMigrationColumns = []migrationColumn{
	// instructions: the server's own initialize instructions, as the last
	// successful probe read them (docs/MCP.md, "Probing"). A row written
	// before this column existed backfills to '', which is exactly what a
	// server that sends no instructions stores anyway — so the only cost of
	// an unmigrated row is that it contributes nothing to the opening
	// message until the operator's next Refresh.
	{"instructions", "TEXT NOT NULL DEFAULT ''"},
	// stale: set when a connected server notifies that its tool, prompt or
	// resource list has changed since the stored snapshot was taken, and
	// cleared by the next successful probe (docs/MCP.md, "What a server
	// sends back unasked"). A row from an older binary backfills to 0,
	// which is what a server that has never said otherwise means anyway.
	{"stale", "INTEGER NOT NULL DEFAULT 0"},
	// allow_sampling: whether this server may ask the harness to run a
	// model turn on its behalf (docs/MCP.md, "Sampling"). Off for an
	// existing row, which is the same answer every server got before the
	// column existed — a capability an operator has never been asked about
	// must not switch itself on during an upgrade.
	{"allow_sampling", "INTEGER NOT NULL DEFAULT 0"},
	// resources_json, prompts_json: the other two lists a server
	// advertises, snapshotted by the same probe that fills tools_json
	// (docs/MCP.md, "Resources" and "Prompts"). Empty for a row from an
	// older binary until its next Refresh, which is the same state a
	// server that advertises neither is in.
	{"resources_json", "TEXT NOT NULL DEFAULT '[]'"},
	{"prompts_json", "TEXT NOT NULL DEFAULT '[]'"},
}
