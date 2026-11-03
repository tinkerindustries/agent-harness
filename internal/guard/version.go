package guard

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

// VersionInputs are the parts of the guard that are not embedded prompts.
type VersionInputs struct {
	// Rules identifies the rule set; the rules package supplies it and it
	// changes whenever a rule, script detection or package script resolution does.
	Rules       string
	Cutoff      float64
	Stage1Model string
	Stage2Model string
}

// Version returns one identifier for the guard's rules, both prompts, the
// stage 2 schema, the cut-off and the model ids. It changes when any of them
// does.
func Version(in VersionInputs) (string, error) {
	p, err := loadPrompts()
	if err != nil {
		return "", err
	}
	return versionOf(in, p), nil
}

func versionOf(in VersionInputs, p prompts) string {
	if in.Stage1Model == "" {
		in.Stage1Model = DefaultStage1Model
	}
	if in.Stage2Model == "" {
		in.Stage2Model = DefaultStage2Model
	}
	h := sha256.New()
	for _, part := range []string{
		in.Rules,
		strconv.FormatFloat(in.Cutoff, 'g', -1, 64),
		in.Stage1Model,
		in.Stage2Model,
		Stage2Effort,
		fmt.Sprint(Stage2MaxTokens),
		string(p.Stage1Questions),
		p.Stage2System,
		string(p.Stage2Schema),
	} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return "guard-" + hex.EncodeToString(h.Sum(nil))[:16]
}
