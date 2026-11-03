package guard

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
)

// The Python files the rules, script detection and package script resolution
// are ported from. They are embedded so the rules identity changes when the
// source of truth does.

//go:embed testdata/assets/source/guard_rules.py
var guardRulesSource string

//go:embed testdata/assets/source/script_detection.py
var scriptDetectionSource string

//go:embed testdata/assets/source/package_scripts.py
var packageScriptsSource string

// RulesIdentity returns a digest of the three Python sources the rules code is
// ported from. Pass it as VersionInputs.Rules.
func RulesIdentity() string {
	h := sha256.New()
	for _, part := range []string{guardRulesSource, scriptDetectionSource, packageScriptsSource} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return "rules-" + hex.EncodeToString(h.Sum(nil))[:16]
}
