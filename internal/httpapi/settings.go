package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/redact"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// The settings surface (docs/DATA-API.md): GET /api/settings serves the
// whole registry, each entry masked when secret, and PUT/DELETE on one key
// write through internal/settings' own validation, so a value the settings
// screen rejects and a value a PUT to /api/settings rejects carry the same
// message.

// --- settings ---

// settingEntry is one row of GET /api/settings: the registry descriptor
// (group, type, default, description, secret, restart) plus the run's own
// state — whether it is set, whether the current value is an override or the
// default, and the display value. A secret key (settings.IsSecretKey) is
// masked to at most its last four characters; the full value never leaves
// the process over HTTP. An unset key omits value.
//
// Min and Max carry the validation bounds in display form and are present
// only for the types that have them — a JSON number for TypeInteger, Go
// duration text ("1s", "24h") for TypeDuration, nothing for TypeString — so a
// plain string setting's payload does not grow a pair of meaningless zeroes.
// The screen shows the bound; the registry still enforces it. Allowed, when
// non-empty, is a TypeString setting's closed set of accepted values
// (model.effort), serialised so the screen can render a ToggleGroup instead
// of a text input.
type settingEntry struct {
	Key         string   `json:"key"`
	Group       string   `json:"group"`
	Type        string   `json:"type"`
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret"`
	Restart     bool     `json:"restart"`
	Set         bool     `json:"set"`
	Override    bool     `json:"override"`
	Value       string   `json:"value,omitempty"`
	Min         any      `json:"min,omitempty"`
	Max         any      `json:"max,omitempty"`
	Allowed     []string `json:"allowed,omitempty"`
}

// handleGetSettings serves GET /api/settings: every key in settings.ValidKeys
// in order (registry order, grouped), each with its descriptor and whether
// it is set, its display value, and whether the stored value differs from
// the default. The override flag is computed here, where the real value is
// known — a masked secret can never be compared client-side. There is
// deliberately no reveal parameter — the full secret never leaves the
// process over HTTP, for anyone, full stop.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	entries := make([]settingEntry, 0, len(settings.ValidKeys))
	for _, d := range settings.Descriptors() {
		value, ok, err := s.Settings.Get(r.Context(), d.Key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		entry := settingEntry{
			Key: d.Key, Group: d.Group, Type: d.Type.String(), Default: d.Default,
			Description: d.Description, Secret: d.Secret, Restart: d.Restart,
			Set: ok, Override: ok && value != d.Default,
		}
		if d.Type == settings.TypeInteger {
			entry.Min, entry.Max = d.Min, d.Max
		} else if d.Type == settings.TypeDuration {
			// A duration's Min and Max are stored as nanoseconds; the screen
			// wants "1s" and "24h", not 1000000000 and 86400000000000, and the
			// bound is display-only (validation stays in the registry), so
			// send the text Go already knows how to format.
			entry.Min, entry.Max = durationText(time.Duration(d.Min)), durationText(time.Duration(d.Max))
		}
		if len(d.Allowed) > 0 {
			entry.Allowed = d.Allowed
		}
		if ok {
			entry.Value = value
			if d.Secret {
				entry.Value = redact.Secret(value)
			}
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, entries)
}

// durationText formats a duration bound the way the settings screen shows it
// — "1s", "2m", "24h" — rather than time.Duration.String()'s "24h0m0s".
// Every bound in the registry is a whole number of seconds, and a range
// rendered as "24h0m0s" reads like a bug next to a field whose own values
// are "30s", "10m", "1h".
func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return d.String()
	}
}

// putSettingBody is the JSON body PUT /api/settings/{key} accepts.
type putSettingBody struct {
	Value string `json:"value"`
}

// handlePutSetting serves PUT /api/settings/{key}: writes key through the
// settings resolver. An unknown key is a 400 carrying UnknownKeyError's
// message; a missing or non-JSON content type is a 415; and a cross-origin
// request (an Origin header that does not match the request's own Host) is a
// 403 — the guards that keep a page open in the operator's browser from
// writing keys to a loopback port (docs/DESIGN.md §4.2, docs/DATA-API.md).
func (s *Server) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	var body putSettingBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `invalid JSON body: expected {"value": "..."}`})
		return
	}
	key := r.PathValue("key")
	if err := s.Settings.Set(r.Context(), key, body.Value); err != nil {
		writeSettingError(w, err)
		return
	}
	if s.OnSettingChanged != nil {
		s.OnSettingChanged(r.Context(), key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteSetting serves DELETE /api/settings/{key}: unsets key.
// Deleting an unset key is not an error. It carries the same content-type
// and origin guards as PUT (docs/DESIGN.md §4.2).
func (s *Server) handleDeleteSetting(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	key := r.PathValue("key")
	if err := s.Settings.Unset(r.Context(), key); err != nil {
		writeSettingError(w, err)
		return
	}
	if s.OnSettingChanged != nil {
		s.OnSettingChanged(r.Context(), key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeSettingError writes a settings resolver error as JSON: an unknown key
// or a value that fails the registry's validation is a 400 carrying the
// resolver's message (an unknown key names the valid keys; a rejected value
// names the type or bounds — never a value), anything else is a 500 like
// every other handler.
func writeSettingError(w http.ResponseWriter, err error) {
	var ue settings.UnknownKeyError
	var ve settings.ValidationError
	switch {
	case errors.As(err, &ue):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.Error()})
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error()})
	default:
		writeInternalError(w, err)
	}
}
