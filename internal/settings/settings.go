// Package settings names the harness's stored settings and resolves them
// through the SQLite store. Settings live in the database rather than in
// the environment so a key can change while the process is running and the
// next request picks the change up without a restart.
//
// Every setting is one entry in the registry (registry.go): its key, type,
// default, validation bounds, description, and the secret flag. Every write
// path validates against that one registry. Values are stored as text in the
// settings table and parsed on read; nothing about the schema changes.
package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// UnknownKeyError reports a read or write of a key outside the known set, so
// a typo like "deepsek.api_key" fails loudly instead of silently storing a
// setting nothing ever reads.
type UnknownKeyError struct{ Key string }

func (e UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown setting %q; valid settings: %s", e.Key, strings.Join(ValidKeys, ", "))
}

// Store is the settings surface of *store.Store, declared as an interface so
// this package depends on the three methods it uses, not the whole store.
type Store interface {
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
}

// Resolver reads and writes settings through to the store on every call and
// caches nothing, so a key changed by another process takes effect on the
// next request without restarting anything.
type Resolver struct {
	store Store
}

// NewResolver returns a Resolver reading and writing st.
func NewResolver(st Store) *Resolver {
	return &Resolver{store: st}
}

// Get returns key's stored value, or ok=false when it is unset. The value is
// exactly what is in the settings table — callers that want the resolved
// value (the default when unset) use String, Int, or Duration instead.
func (r *Resolver) Get(ctx context.Context, key string) (string, bool, error) {
	if err := validate(key); err != nil {
		return "", false, err
	}
	return r.store.Setting(ctx, key)
}

// Set writes key, rejecting any key outside the known set with an error that
// lists the valid ones, and any value that fails the registry's type or
// bounds check with a ValidationError. A rejected value is identical whether
// it arrives.
func (r *Resolver) Set(ctx context.Context, key, value string) error {
	if err := validate(key); err != nil {
		return err
	}
	d, _ := Lookup(key)
	if err := d.validate(value); err != nil {
		return ValidationError{Key: key, Value: value, Err: err}
	}
	return r.store.SetSetting(ctx, key, value)
}

// Unset deletes key. Deleting an unset key is not an error.
func (r *Resolver) Unset(ctx context.Context, key string) error {
	if err := validate(key); err != nil {
		return err
	}
	return r.store.DeleteSetting(ctx, key)
}

// String returns key's resolved value: the stored value when set, the
// registry default when unset.
func (r *Resolver) String(ctx context.Context, key string) (string, error) {
	v, ok, err := r.Get(ctx, key)
	if err != nil || ok {
		return v, err
	}
	d, _ := Lookup(key)
	return d.Default, nil
}

// Int returns key's resolved value as an integer.
func (r *Resolver) Int(ctx context.Context, key string) (int, error) {
	v, err := r.String(ctx, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: stored value %q is not an integer: %w", key, v, err)
	}
	return n, nil
}

// Duration returns key's resolved value as a time span.
func (r *Resolver) Duration(ctx context.Context, key string) (time.Duration, error) {
	v, err := r.String(ctx, key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: stored value %q is not a duration: %w", key, v, err)
	}
	return d, nil
}

// DeepSeekAPIKey returns the stored DeepSeek API key, "" when unset — the
// exact shape the deepseek client's per-request key provider needs.
func (r *Resolver) DeepSeekAPIKey(ctx context.Context) (string, error) {
	return r.String(ctx, KeyDeepSeekAPIKey)
}

// KimiAPIKey returns the stored Kimi API key, "" when unset — the exact
// shape the kimi client's per-request key provider needs.
func (r *Resolver) KimiAPIKey(ctx context.Context) (string, error) {
	return r.String(ctx, KeyKimiAPIKey)
}

// GoogleAPIKey returns the stored Google API key, "" when unset — the exact
// shape the gemini client's per-request key provider needs.
func (r *Resolver) GoogleAPIKey(ctx context.Context) (string, error) {
	return r.String(ctx, KeyGoogleAPIKey)
}

// GoogleVisionModel returns the stored Gemini vision model, the registry's
// google.vision_model default when unset — the exact shape the vision
// tools' (Glance, Ground, Detect) per-call model provider needs.
func (r *Resolver) GoogleVisionModel(ctx context.Context) (string, error) {
	return r.String(ctx, KeyGoogleVisionModel)
}

func validate(key string) error {
	_, ok := Lookup(key)
	if ok {
		return nil
	}
	return UnknownKeyError{Key: key}
}
