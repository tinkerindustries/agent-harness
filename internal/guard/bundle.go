package guard

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// BundleSchemaVersion is the only bundle format this build reads.
const BundleSchemaVersion = 1

// BundleError is a refused bundle. Field is the JSON path of the offending
// field inside the bundle, empty for the bundle as a whole. Neither field
// carries any text from the bundle.
type BundleError struct {
	Field  string
	Reason string
}

func (e *BundleError) Error() string {
	if e.Field == "" {
		return "guard policy: " + e.Reason
	}
	return "guard policy: " + e.Field + " " + e.Reason
}

func refuse(field, reason string) error { return &BundleError{Field: field, Reason: reason} }

// Bundle is a validated policy: everything that says what the guard blocks.
// Only LoadBundle makes one. It is read-only after that.
type Bundle struct {
	version string
	cutoff  float64
	stage1  *stage1Policy
	stage2  stage2Policy
	rules   Rules
	needed  FieldSet
}

type stage1Policy struct {
	model     string
	fields    []string
	questions json.RawMessage
	question  string
}

type stage2Policy struct {
	model     string
	effort    string
	maxTokens int
	fields    []string
	system    string
	schema    json.RawMessage
}

// Version is the guard version recorded on every decision: a hash of every
// field of the bundle.
func (b *Bundle) Version() string { return b.version }

// Cutoff is the stage 1 threshold. A command with p(block) at or above it
// goes to stage 2.
func (b *Bundle) Cutoff() float64 { return b.cutoff }

// HasStage1 reports whether the bundle configures stage 1.
func (b *Bundle) HasStage1() bool { return b.stage1 != nil }

// Stage1Model is the stage 1 model id, empty without a stage 1.
func (b *Bundle) Stage1Model() string {
	if b.stage1 == nil {
		return ""
	}
	return b.stage1.model
}

// Stage2Model is the stage 2 model id.
func (b *Bundle) Stage2Model() string { return b.stage2.model }

// Rules is the rule checker built from the bundle's rules.
func (b *Bundle) Rules() Rules { return b.rules }

// StateFields are the state fields the guard needs filled in: those a stage
// receives, the command, the scripts the escalation check reads, and the
// branch when the push check is on. A StateBuilder given this set skips the
// rest.
func (b *Bundle) StateFields() FieldSet {
	out := FieldSet{}
	for f := range b.needed {
		out[f] = true
	}
	return out
}

type bundleJSON struct {
	SchemaVersion *int      `json:"schema_version"`
	Cutoff        *float64  `json:"cutoff"`
	Stage1        *stage1In `json:"stage1"`
	Stage2        *stage2In `json:"stage2"`
	Rules         *rulesIn  `json:"rules"`
}

type stage1In struct {
	Model       *string         `json:"model"`
	StateFields []string        `json:"state_fields"`
	Questions   json.RawMessage `json:"questions"`
}

type stage2In struct {
	Model       *string         `json:"model"`
	Effort      *string         `json:"effort"`
	MaxTokens   *int            `json:"max_tokens"`
	StateFields []string        `json:"state_fields"`
	System      *string         `json:"system"`
	Schema      json.RawMessage `json:"schema"`
}

type rulesIn struct {
	Macros          map[string]string `json:"macros"`
	Table           []ruleIn          `json:"table"`
	DefaultBranches []string          `json:"default_branches"`
}

type ruleIn struct {
	Name     *string     `json:"name"`
	Patterns []patternIn `json:"patterns"`
}

type patternIn struct {
	Match            *string `json:"match"`
	UnlessFollowedBy *string `json:"unless_followed_by"`
}

// shape lists the keys each object of the format may hold, so a misspelt
// optional field is refused and not ignored.
type shape struct {
	keys map[string]*shape
	elem *shape
}

var bundleShape = &shape{keys: map[string]*shape{
	"schema_version": nil,
	"cutoff":         nil,
	"stage1": {keys: map[string]*shape{
		"model": nil, "state_fields": nil, "questions": nil,
	}},
	"stage2": {keys: map[string]*shape{
		"model": nil, "effort": nil, "max_tokens": nil, "state_fields": nil, "system": nil, "schema": nil,
	}},
	"rules": {keys: map[string]*shape{
		"macros": nil,
		"table": {elem: &shape{keys: map[string]*shape{
			"name": nil,
			"patterns": {elem: &shape{keys: map[string]*shape{
				"match": nil, "unless_followed_by": nil,
			}}},
		}}},
		"default_branches": nil,
	}},
}}

func checkShape(v any, s *shape, path string) error {
	if s == nil {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		if s.keys == nil {
			return nil
		}
		for k, child := range x {
			sub, known := s.keys[k]
			if !known {
				return refuse(path, "has a field the format does not define")
			}
			if err := checkShape(child, sub, joinPath(path, k)); err != nil {
				return err
			}
		}
	case []any:
		if s.elem == nil {
			return nil
		}
		for i, child := range x {
			if err := checkShape(child, s.elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// LoadBundle parses and validates a policy bundle. Every error is a
// *BundleError naming the field; none repeats text from the bundle.
func LoadBundle(data []byte) (*Bundle, error) {
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, refuse("", "is not valid JSON")
	}
	if _, ok := generic.(map[string]any); !ok {
		return nil, refuse("", "must be a JSON object")
	}
	if err := checkShape(generic, bundleShape, ""); err != nil {
		return nil, err
	}
	var in bundleJSON
	if err := json.Unmarshal(data, &in); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, refuse(typeErr.Field, "has the wrong type")
		}
		return nil, refuse("", "could not be decoded")
	}
	if in.SchemaVersion == nil {
		return nil, refuse("schema_version", "is required")
	}
	if *in.SchemaVersion != BundleSchemaVersion {
		return nil, refuse("schema_version", "is not a version this build reads")
	}
	b := &Bundle{}
	if in.Cutoff != nil {
		c := *in.Cutoff
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > 1 {
			return nil, refuse("cutoff", "must be a probability between 0 and 1")
		}
		b.cutoff = c
	}
	if in.Stage1 != nil {
		if in.Cutoff == nil {
			return nil, refuse("cutoff", "is required when stage1 is present")
		}
		s1, err := buildStage1(in.Stage1)
		if err != nil {
			return nil, err
		}
		b.stage1 = s1
	}
	if in.Stage2 == nil {
		return nil, refuse("stage2", "is required")
	}
	s2, err := buildStage2(in.Stage2)
	if err != nil {
		return nil, err
	}
	b.stage2 = s2
	if in.Rules == nil {
		return nil, refuse("rules", "is required")
	}
	rules, err := buildRules(in.Rules)
	if err != nil {
		return nil, err
	}
	b.rules = rules

	b.needed = FieldSet{FieldProposedCommand: true, FieldScriptsRun: true}
	if len(rules.defaultBranches) > 0 {
		b.needed[FieldBranch] = true
	}
	for _, f := range b.stage2.fields {
		b.needed[f] = true
	}
	if b.stage1 != nil {
		for _, f := range b.stage1.fields {
			b.needed[f] = true
		}
	}
	b.version = b.hash(in.Rules)
	return b, nil
}

func requireText(p *string, field string) (string, error) {
	if p == nil {
		return "", refuse(field, "is required")
	}
	if strings.TrimSpace(*p) == "" {
		return "", refuse(field, "must not be empty")
	}
	return *p, nil
}

func buildStage1(in *stage1In) (*stage1Policy, error) {
	model, err := requireText(in.Model, "stage1.model")
	if err != nil {
		return nil, err
	}
	fields, err := checkStateFields(in.StateFields, "stage1.state_fields")
	if err != nil {
		return nil, err
	}
	questions, err := compactObject(in.Questions, "stage1.questions")
	if err != nil {
		return nil, err
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(questions, &named); err != nil || len(named) != 1 {
		return nil, refuse("stage1.questions", "must hold exactly one question")
	}
	p := &stage1Policy{model: model, fields: fields, questions: questions}
	for name := range named {
		p.question = name
	}
	return p, nil
}

func buildStage2(in *stage2In) (stage2Policy, error) {
	model, err := requireText(in.Model, "stage2.model")
	if err != nil {
		return stage2Policy{}, err
	}
	effort, err := requireText(in.Effort, "stage2.effort")
	if err != nil {
		return stage2Policy{}, err
	}
	if in.MaxTokens == nil {
		return stage2Policy{}, refuse("stage2.max_tokens", "is required")
	}
	if *in.MaxTokens <= 0 {
		return stage2Policy{}, refuse("stage2.max_tokens", "must be positive")
	}
	fields, err := checkStateFields(in.StateFields, "stage2.state_fields")
	if err != nil {
		return stage2Policy{}, err
	}
	system, err := requireText(in.System, "stage2.system")
	if err != nil {
		return stage2Policy{}, err
	}
	schema, err := compactObject(in.Schema, "stage2.schema")
	if err != nil {
		return stage2Policy{}, err
	}
	return stage2Policy{model: model, effort: effort, maxTokens: *in.MaxTokens, fields: fields, system: system, schema: schema}, nil
}

// compactObject checks raw is a JSON object and returns it with the
// insignificant whitespace removed. Key order and content are untouched.
func compactObject(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, refuse(field, "is required")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, refuse(field, "must be a JSON object")
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, refuse(field, "must be a JSON object")
	}
	return out.Bytes(), nil
}

func checkStateFields(fields []string, path string) ([]string, error) {
	if fields == nil {
		return nil, refuse(path, "is required")
	}
	seen := map[string]bool{}
	for i, f := range fields {
		at := fmt.Sprintf("%s[%d]", path, i)
		if !knownField(f) {
			return nil, refuse(at, "is not a state field the guard knows")
		}
		if seen[f] {
			return nil, refuse(at, "repeats an earlier entry")
		}
		seen[f] = true
	}
	if !seen[FieldProposedCommand] {
		return nil, refuse(path, "must include proposed_command")
	}
	return append([]string(nil), fields...), nil
}

var (
	macroName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	macroRef  = regexp.MustCompile(`\$\{([^}]*)\}`)
)

var (
	errUnknownMacro = errors.New("unknown macro")
	errMacroCycle   = errors.New("macro cycle")
)

// expandMacros replaces each ${name} in text with the macro's body, which may
// itself use macros.
func expandMacros(text string, macros map[string]string, active map[string]bool) (string, error) {
	var failure error
	out := macroRef.ReplaceAllStringFunc(text, func(ref string) string {
		name := ref[2 : len(ref)-1]
		body, ok := macros[name]
		if !ok {
			failure = errUnknownMacro
			return ""
		}
		if active[name] {
			failure = errMacroCycle
			return ""
		}
		active[name] = true
		expanded, err := expandMacros(body, macros, active)
		delete(active, name)
		if err != nil {
			failure = err
			return ""
		}
		return expanded
	})
	return out, failure
}

func buildRules(in *rulesIn) (Rules, error) {
	if in.Table == nil {
		return Rules{}, refuse("rules.table", "is required")
	}
	if in.DefaultBranches == nil {
		return Rules{}, refuse("rules.default_branches", "is required; an empty list turns the push check off")
	}
	for name := range in.Macros {
		if !macroName.MatchString(name) {
			return Rules{}, refuse("rules.macros", "has a name that is not lower case letters, digits and underscores")
		}
	}
	for name, body := range in.Macros {
		if _, err := expandMacros(body, in.Macros, map[string]bool{name: true}); err != nil {
			if errors.Is(err, errMacroCycle) {
				return Rules{}, refuse("rules.macros", "has a macro that uses itself")
			}
			return Rules{}, refuse("rules.macros", "has a macro that uses an undefined macro")
		}
	}
	rules := Rules{defaultBranches: map[string]bool{}}
	for i, branch := range in.DefaultBranches {
		if strings.TrimSpace(branch) == "" {
			return Rules{}, refuse(fmt.Sprintf("rules.default_branches[%d]", i), "must not be empty")
		}
		rules.defaultBranches[branch] = true
	}
	for i, rule := range in.Table {
		at := fmt.Sprintf("rules.table[%d]", i)
		name, err := requireText(rule.Name, at+".name")
		if err != nil {
			return Rules{}, err
		}
		if len(rule.Patterns) == 0 {
			return Rules{}, refuse(at+".patterns", "must hold at least one pattern")
		}
		entry := ruleEntry{name: name}
		for j, pat := range rule.Patterns {
			pat := pat
			pAt := fmt.Sprintf("%s.patterns[%d]", at, j)
			if pat.Match == nil {
				return Rules{}, refuse(pAt+".match", "is required")
			}
			re, err := compileRule(*pat.Match, in.Macros, pAt+".match")
			if err != nil {
				return Rules{}, err
			}
			if re.MatchString("") {
				return Rules{}, refuse(pAt+".match", "must not match the empty string")
			}
			compiled := rulePattern{re: re}
			if pat.UnlessFollowedBy != nil {
				unless, err := compileRule(*pat.UnlessFollowedBy, in.Macros, pAt+".unless_followed_by")
				if err != nil {
					return Rules{}, err
				}
				compiled.unless = unless
			}
			entry.patterns = append(entry.patterns, compiled)
		}
		rules.table = append(rules.table, entry)
	}
	return rules, nil
}

// compileRule expands the macros in body and compiles it anchored at the
// start of the text it is matched against.
func compileRule(body string, macros map[string]string, field string) (*regexp.Regexp, error) {
	expanded, err := expandMacros(body, macros, map[string]bool{})
	if err != nil {
		return nil, refuse(field, "uses a macro that is not defined in rules.macros")
	}
	re, err := regexp.Compile(`^(?:` + expanded + `)`)
	if err != nil {
		return nil, refuse(field, "is not a valid RE2 pattern")
	}
	return re, nil
}

// hash is the guard version: a digest of every field, with each part length
// prefixed so a byte moved between two parts changes it.
func (b *Bundle) hash(rules *rulesIn) string {
	h := sha256.New()
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	putList := func(list []string) {
		put(strconv.Itoa(len(list)))
		for _, s := range list {
			put(s)
		}
	}
	put(strconv.Itoa(BundleSchemaVersion))
	put(strconv.FormatFloat(b.cutoff, 'g', -1, 64))
	if b.stage1 == nil {
		put("no stage1")
	} else {
		put("stage1")
		put(b.stage1.model)
		putList(b.stage1.fields)
		put(string(b.stage1.questions))
	}
	put(b.stage2.model)
	put(b.stage2.effort)
	put(strconv.Itoa(b.stage2.maxTokens))
	putList(b.stage2.fields)
	put(b.stage2.system)
	put(string(b.stage2.schema))

	names := make([]string, 0, len(rules.Macros))
	for name := range rules.Macros {
		names = append(names, name)
	}
	sort.Strings(names)
	put(strconv.Itoa(len(names)))
	for _, name := range names {
		put(name)
		put(rules.Macros[name])
	}
	putList(rules.DefaultBranches)
	put(strconv.Itoa(len(rules.Table)))
	for _, rule := range rules.Table {
		put(*rule.Name)
		put(strconv.Itoa(len(rule.Patterns)))
		for _, pat := range rule.Patterns {
			put(*pat.Match)
			if pat.UnlessFollowedBy == nil {
				put("no exception")
			} else {
				put("exception")
				put(*pat.UnlessFollowedBy)
			}
		}
	}
	return "guard-" + hex.EncodeToString(h.Sum(nil))[:16]
}
