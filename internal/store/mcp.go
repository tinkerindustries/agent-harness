package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MCP transports (docs/MCP.md, "Probing").
const (
	MCPTransportStdio = "stdio"
	MCPTransportHTTP  = "http"
)

// MCPToolSnapshot is one tool as the last successful probe read it.
type MCPToolSnapshot struct {
	Name string `json:"name"`
	// QualifiedName is the mcp__<server>__<tool> name the model is offered
	// (docs/MCP.md, "Naming"), computed once by internal/mcpclient at probe
	// time and frozen here rather than derived again whenever the tool array
	// is built. Computing it once, with the full tool list in hand, is what
	// makes the array a pure function of stored rows — including the
	// collision handling within one server's tools, which must run exactly
	// once rather than being re-derived, possibly differently, on every read.
	QualifiedName string          `json:"qualified_name"`
	Description   string          `json:"description"`
	InputSchema   json.RawMessage `json:"input_schema"`
}

// MCPProbe is everything one successful probe read from a server: the three
// lists it advertises and the instructions it sent at initialize. It is one
// value rather than four parameters because SaveMCPProbe writes it in one
// statement, and because a caller that could supply three of the four would
// be a caller able to mix two probes together.
type MCPProbe struct {
	Tools        []MCPToolSnapshot
	Resources    []MCPResourceSnapshot
	Prompts      []MCPPromptSnapshot
	Instructions string
}

// MCPResourceSnapshot is one resource, or one resource template, as the
// last successful probe read it. Template says which: a template's URI is a
// pattern with {placeholders} in it rather than something readable as it
// stands, so the two cannot be told apart from the URI alone and a reader
// that tried would eventually try to read a pattern.
type MCPResourceSnapshot struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mime_type"`
	Template    bool   `json:"template"`
}

// MCPPromptSnapshot is one prompt a server advertises, as the last
// successful probe read it.
type MCPPromptSnapshot struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Arguments   []MCPPromptArgSnapshot `json:"arguments"`
}

// MCPPromptArgSnapshot is one argument a prompt takes.
type MCPPromptArgSnapshot struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// MCPServer is one row of the mcp_servers table: the operator's
// configuration for one MCP server, plus the last successful probe's tool
// list (docs/MCP.md, "The table"). Args, Env, Headers, and Tools are never
// nil on a value this package hands back — CreateMCPServer, UpdateMCPServer,
// and every read normalise a nil slice or map to empty before it is encoded
// or after it is decoded, so a round trip through the database never turns a
// caller's nil into a stored nil or back.
type MCPServer struct {
	Name          string
	Transport     string // MCPTransportStdio or MCPTransportHTTP
	Command       string
	Args          []string
	Env           map[string]string
	URL           string
	Headers       map[string]string
	Enabled       bool
	AllowReadOnly bool
	// AllowSampling lets this server ask the harness to run a model turn on
	// its behalf (docs/MCP.md, "Sampling"). Off by default and per server,
	// because sampling spends the operator's tokens on a prompt the server
	// wrote: it is the one MCP capability where connecting a server and
	// letting it run are different decisions.
	AllowSampling bool
	Tools         []MCPToolSnapshot
	// Resources and Prompts are the other two lists a server advertises,
	// from the same probe as Tools and kept under the same rule: a failed
	// probe leaves them as they were. Unlike Tools they contribute no
	// entries to the model's tool array — they are reached through the
	// fixed mcp_* tools instead, so that a server gaining a hundred
	// resources cannot move the array a session froze (docs/MCP.md,
	// "Resources").
	Resources []MCPResourceSnapshot
	Prompts   []MCPPromptSnapshot
	// Instructions is the server's own initialize instructions — the
	// `instructions` field of its InitializeResult — as the last successful
	// probe read them, and "" for a server that sends none or has never been
	// probed. It travels with Tools because it comes from the same probe and
	// means the same kind of thing: what this server told us about itself,
	// frozen, so what a session tells the model does not depend on whether a
	// subprocess happened to start this minute (docs/MCP.md, "What the model
	// is told").
	Instructions string
	// Stale is set when a connected server notified that its advertised
	// lists changed after the snapshot in this row was taken, and cleared
	// by the next successful probe. Nothing re-probes on the notification:
	// a run's tool array is frozen for the life of the run, so the honest
	// response is to tell the operator a Refresh is worth pressing, not to
	// move the array under a session already using it.
	Stale      bool
	ProbedAt   string // RFC3339Nano; "" when never probed successfully
	ProbeError string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// mcpServerNameRE is the server name grammar (docs/MCP.md, "Naming"): a
// lowercase letter or digit, followed by up to 31 lowercase letters, digits,
// underscores, or hyphens. It is what keeps the mcp__<server>__<tool>
// prefix free of any character the model API would reject in a function
// name.
var mcpServerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// mcpEnvKeyRE is the grammar for an env var name.
var mcpEnvKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateMCPServer returns a plain-text error an HTTP 400 can show
// verbatim, or nil when s is well-formed. It is a package-level function
// rather than a method so every write path — the CLI and the HTTP API —
// shares one copy, the same reason internal/settings keeps its validation in
// the registry rather than in the store. It checks shape only: it never
// touches the database, so it cannot tell CreateMCPServer's "name already
// exists" from UpdateMCPServer's "name does not exist yet" — those are the
// store methods' own errors.
func ValidateMCPServer(s MCPServer) error {
	if !mcpServerNameRE.MatchString(s.Name) {
		return fmt.Errorf("mcp server name %q must match %s", s.Name, mcpServerNameRE.String())
	}
	switch s.Transport {
	case MCPTransportStdio:
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("mcp server %q: stdio transport requires a command", s.Name)
		}
		if s.URL != "" {
			return fmt.Errorf("mcp server %q: stdio transport must not set a url", s.Name)
		}
		if len(s.Headers) != 0 {
			return fmt.Errorf("mcp server %q: stdio transport must not set headers", s.Name)
		}
	case MCPTransportHTTP:
		if s.URL == "" {
			return fmt.Errorf("mcp server %q: http transport requires a url", s.Name)
		}
		u, err := url.Parse(s.URL)
		if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("mcp server %q: url %q must be an absolute http or https URL", s.Name, s.URL)
		}
		if s.Command != "" {
			return fmt.Errorf("mcp server %q: http transport must not set a command", s.Name)
		}
		if len(s.Args) != 0 {
			return fmt.Errorf("mcp server %q: http transport must not set args", s.Name)
		}
		if len(s.Env) != 0 {
			return fmt.Errorf("mcp server %q: http transport must not set env", s.Name)
		}
	default:
		return fmt.Errorf("mcp server %q: unknown transport %q", s.Name, s.Transport)
	}
	for k := range s.Env {
		if !mcpEnvKeyRE.MatchString(k) {
			return fmt.Errorf("mcp server %q: invalid env key %q", s.Name, k)
		}
	}
	for k := range s.Headers {
		if !validHTTPFieldName(k) {
			return fmt.Errorf("mcp server %q: invalid header key %q", s.Name, k)
		}
	}
	return nil
}

// validHTTPFieldName reports whether k is a non-empty string with no space,
// colon, or control character — a header key loose enough to accept any
// real field name while rejecting the characters that would break the wire
// format it rides on.
func validHTTPFieldName(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if r == ':' || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// mcpColumns is the column list every mcp_servers read uses, so a column
// added for one query cannot silently miss another.
const mcpColumns = `name, transport, command, args, env, url, headers, enabled, allow_readonly,
	allow_sampling, tools_json, resources_json, prompts_json, instructions, stale, probed_at,
	probe_error, created_at, updated_at`

func scanMCPServer(row interface {
	Scan(dest ...any) error
}) (MCPServer, error) {
	var srv MCPServer
	var argsJSON, envJSON, headersJSON, toolsJSON, resourcesJSON, promptsJSON string
	var enabled, allowReadOnly, allowSampling, stale int
	var createdAt, updatedAt string
	err := row.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsJSON, &envJSON, &srv.URL, &headersJSON,
		&enabled, &allowReadOnly, &allowSampling, &toolsJSON, &resourcesJSON, &promptsJSON,
		&srv.Instructions, &stale, &srv.ProbedAt, &srv.ProbeError, &createdAt, &updatedAt)
	if err != nil {
		return MCPServer{}, err
	}
	srv.Enabled = enabled != 0
	srv.AllowReadOnly = allowReadOnly != 0
	srv.AllowSampling = allowSampling != 0
	srv.Stale = stale != 0
	if err := json.Unmarshal([]byte(argsJSON), &srv.Args); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q args: %w", srv.Name, err)
	}
	if err := json.Unmarshal([]byte(envJSON), &srv.Env); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q env: %w", srv.Name, err)
	}
	if err := json.Unmarshal([]byte(headersJSON), &srv.Headers); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q headers: %w", srv.Name, err)
	}
	if err := json.Unmarshal([]byte(toolsJSON), &srv.Tools); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q tools_json: %w", srv.Name, err)
	}
	if err := json.Unmarshal([]byte(resourcesJSON), &srv.Resources); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q resources_json: %w", srv.Name, err)
	}
	if err := json.Unmarshal([]byte(promptsJSON), &srv.Prompts); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q prompts_json: %w", srv.Name, err)
	}
	if srv.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q created_at: %w", srv.Name, err)
	}
	if srv.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return MCPServer{}, fmt.Errorf("store: decode mcp server %q updated_at: %w", srv.Name, err)
	}
	return srv, nil
}

// ListMCPServers returns every mcp_servers row ordered by name ascending.
// The order is deterministic on purpose: it is what makes the tool array a
// session builds from these rows deterministic too (docs/MCP.md, "Naming").
func (s *Store) ListMCPServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+mcpColumns+` FROM mcp_servers ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		srv, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// ListEnabledMCPServers is ListMCPServers filtered to enabled = 1, in the
// same name-ascending order.
func (s *Store) ListEnabledMCPServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+mcpColumns+` FROM mcp_servers WHERE enabled = 1 ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		srv, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// GetMCPServer reads one server by name. ErrMCPServerNotFound when absent.
func (s *Store) GetMCPServer(ctx context.Context, name string) (MCPServer, error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT `+mcpColumns+` FROM mcp_servers WHERE name = ?`, name)
	srv, err := scanMCPServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPServer{}, ErrMCPServerNotFound
	}
	if err != nil {
		return MCPServer{}, err
	}
	return srv, nil
}

// CreateMCPServer inserts srv. ErrMCPServerExists when a row with srv.Name
// already exists. It stamps CreatedAt and UpdatedAt with time.Now().UTC(),
// ignoring any caller-set CreatedAt, UpdatedAt, Tools, Instructions,
// ProbedAt, or ProbeError — a server that has never been probed has no
// snapshot to carry, and its own timestamps are a fact this write
// establishes, not one a caller gets to assert.
func (s *Store) CreateMCPServer(ctx context.Context, srv MCPServer) error {
	argsJSON, err := marshalMCPStrings(srv.Args)
	if err != nil {
		return err
	}
	envJSON, err := marshalMCPStringMap(srv.Env)
	if err != nil {
		return err
	}
	headersJSON, err := marshalMCPStringMap(srv.Headers)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.submit(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM mcp_servers WHERE name = ?`, srv.Name).Scan(&exists)
		switch {
		case err == nil:
			return ErrMCPServerExists
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.Exec(`
			INSERT INTO mcp_servers (name, transport, command, args, env, url, headers, enabled, allow_readonly,
				allow_sampling, tools_json, resources_json, prompts_json, instructions, stale, probed_at,
				probe_error, created_at, updated_at, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', '[]', '', 0, '', '', ?, ?, 1)`,
			srv.Name, srv.Transport, srv.Command, argsJSON, envJSON, srv.URL, headersJSON,
			boolToInt(srv.Enabled), boolToInt(srv.AllowReadOnly), boolToInt(srv.AllowSampling), now, now)
		return err
	})
}

// UpdateMCPServer overwrites srv.Name's configuration columns — transport,
// command, args, env, url, headers, enabled, allow_readonly — and
// updated_at. It leaves tools_json, instructions, probed_at, probe_error,
// and created_at alone: a configuration edit is not a probe, and the
// snapshot from the last one that succeeded must survive it (docs/MCP.md, "The tool array is built
// from a stored snapshot"). ErrMCPServerNotFound when absent.
func (s *Store) UpdateMCPServer(ctx context.Context, srv MCPServer) error {
	argsJSON, err := marshalMCPStrings(srv.Args)
	if err != nil {
		return err
	}
	envJSON, err := marshalMCPStringMap(srv.Env)
	if err != nil {
		return err
	}
	headersJSON, err := marshalMCPStringMap(srv.Headers)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE mcp_servers SET transport = ?, command = ?, args = ?, env = ?, url = ?, headers = ?,
				enabled = ?, allow_readonly = ?, allow_sampling = ?, updated_at = ?, version = version + 1
			WHERE name = ?`,
			srv.Transport, srv.Command, argsJSON, envJSON, srv.URL, headersJSON,
			boolToInt(srv.Enabled), boolToInt(srv.AllowReadOnly), boolToInt(srv.AllowSampling), now, srv.Name)
		if err != nil {
			return err
		}
		return mustAffectOne(res, ErrMCPServerNotFound)
	})
}

// SetMCPServerEnabled is the toggle path: it touches only enabled and
// updated_at, so flipping a server on or off never disturbs its
// configuration or its probe snapshot. ErrMCPServerNotFound when absent.
func (s *Store) SetMCPServerEnabled(ctx context.Context, name string, enabled bool) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE mcp_servers SET enabled = ?, updated_at = ?, version = version + 1 WHERE name = ?`,
			boolToInt(enabled), now, name)
		if err != nil {
			return err
		}
		return mustAffectOne(res, ErrMCPServerNotFound)
	})
}

// DeleteMCPServer removes name's row. ErrMCPServerNotFound when absent.
func (s *Store) DeleteMCPServer(ctx context.Context, name string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM mcp_servers WHERE name = ?`, name)
		if err != nil {
			return err
		}
		return mustAffectOne(res, ErrMCPServerNotFound)
	})
}

// SaveMCPProbe records the outcome of one probe against name
// (docs/MCP.md, "Probing"). The two outcomes are asymmetric on purpose, and
// the asymmetry is the point of the whole table: a successful probe
// (probeErr == "") writes tools_json from tools and instructions from
// instructions, sets probed_at to at, and clears probe_error — the new
// snapshot the request head's tool array and the opening message's MCP
// section will both be built from. A failed probe (probeErr != "") writes
// probe_error only, leaving tools_json, instructions and probed_at exactly
// as they were: the array a session builds must not shrink because a
// subprocess happened not to start this minute (docs/MCP.md, "The tool
// array is built from a stored snapshot, never from a live connection").
// ErrMCPServerNotFound when name has no row.
//
// Everything a probe read travels together in one MCPProbe and lands in one
// statement, because it all came from one handshake: a server that revises
// its instructions alongside its tool list must not leave a session reading
// one probe's tools under an earlier probe's instructions.
func (s *Store) SaveMCPProbe(ctx context.Context, name string, snap MCPProbe, probeErr string, at time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		if probeErr == "" {
			toolsJSON, err := marshalMCPJSON(snap.Tools, []MCPToolSnapshot{})
			if err != nil {
				return err
			}
			resourcesJSON, err := marshalMCPJSON(snap.Resources, []MCPResourceSnapshot{})
			if err != nil {
				return err
			}
			promptsJSON, err := marshalMCPJSON(snap.Prompts, []MCPPromptSnapshot{})
			if err != nil {
				return err
			}
			res, err = tx.Exec(`
				UPDATE mcp_servers SET tools_json = ?, resources_json = ?, prompts_json = ?,
					instructions = ?, stale = 0, probed_at = ?, probe_error = '', version = version + 1
				WHERE name = ?`,
				toolsJSON, resourcesJSON, promptsJSON, snap.Instructions,
				at.UTC().Format(time.RFC3339Nano), name)
			if err != nil {
				return err
			}
		} else {
			var err error
			res, err = tx.Exec(`UPDATE mcp_servers SET probe_error = ?, version = version + 1 WHERE name = ?`,
				probeErr, name)
			if err != nil {
				return err
			}
		}
		return mustAffectOne(res, ErrMCPServerNotFound)
	})
}

// SetMCPServerStale marks name's stored snapshot as out of date, or marks
// it current again. It is the one write a *notification* triggers rather
// than an operator action: a connected server saying its tool, prompt or
// resource list has changed since the probe that filled this row
// (docs/MCP.md, "What a server sends back unasked").
//
// It deliberately does not re-probe. A run's tool array is frozen for the
// life of the run, and replacing it underneath a session in flight would
// invalidate the prompt-cache prefix every request of that run shares — so
// the flag is a note for the operator, and the next Refresh is what acts on
// it. Unlike every other write here it leaves updated_at and version alone:
// nothing the operator configured has changed, and bumping the version
// would make a server chattering about its own lists collide with an
// operator's edit. ErrMCPServerNotFound when absent.
func (s *Store) SetMCPServerStale(ctx context.Context, name string, stale bool) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE mcp_servers SET stale = ? WHERE name = ?`, boolToInt(stale), name)
		if err != nil {
			return err
		}
		return mustAffectOne(res, ErrMCPServerNotFound)
	})
}

// mustAffectOne returns notFound when res reports zero rows affected, the
// shape every mcp_servers write-by-name uses to turn "no such row" into the
// package's own not-found error instead of a silent no-op.
func mustAffectOne(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// marshalMCPStrings encodes args as a JSON array, normalising nil to empty
// so a caller's nil slice and empty slice are indistinguishable once stored
// — a round trip through the database always returns []string{}, never nil.
func marshalMCPStrings(args []string) (string, error) {
	if args == nil {
		args = []string{}
	}
	b, err := json.Marshal(args)
	return string(b), err
}

// marshalMCPStringMap encodes m as a JSON object, normalising nil to empty
// the same way marshalMCPStrings does for a slice.
func marshalMCPStringMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// marshalMCPJSON encodes a probe snapshot slice as JSON, substituting empty
// for nil the same way marshalMCPStrings does — so a round trip through the
// database always returns an empty slice, never nil, whichever of the three
// lists it holds.
func marshalMCPJSON[T any](v []T, empty []T) (string, error) {
	if v == nil {
		v = empty
	}
	b, err := json.Marshal(v)
	return string(b), err
}
