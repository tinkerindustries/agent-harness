package wire

// ToolCallAssembler reconstructs complete tool calls from streamed deltas.
// Deltas are keyed by index; id, type, and name arrive once on the opening
// frame for that index, and arguments arrive as fragments that must be
// concatenated in arrival order, mid-token and mid-string
// (docs/OBSERVED.md).
type ToolCallAssembler struct {
	order []int
	calls map[int]*assembledCall
}

type assembledCall struct {
	id, typ, name string
	arguments     []byte
}

// AssembledToolCall is a tool call with its arguments fully concatenated.
// Arguments is the raw text the model produced; it is not guaranteed to be
// valid JSON and must be validated before use.
type AssembledToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments string
}

// NewToolCallAssembler returns an empty assembler.
func NewToolCallAssembler() *ToolCallAssembler {
	return &ToolCallAssembler{calls: make(map[int]*assembledCall)}
}

// Add folds one streamed delta into the assembler.
func (a *ToolCallAssembler) Add(d ToolCallDelta) {
	c, ok := a.calls[d.Index]
	if !ok {
		c = &assembledCall{}
		a.calls[d.Index] = c
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		c.id = d.ID
	}
	if d.Type != "" {
		c.typ = d.Type
	}
	if d.Function.Name != "" {
		c.name = d.Function.Name
	}
	c.arguments = append(c.arguments, d.Function.Arguments...)
}

// Finalize returns the assembled calls in the order their index first
// appeared.
func (a *ToolCallAssembler) Finalize() []AssembledToolCall {
	out := make([]AssembledToolCall, 0, len(a.order))
	for _, idx := range a.order {
		c := a.calls[idx]
		out = append(out, AssembledToolCall{
			ID:        c.id,
			Type:      c.typ,
			Name:      c.name,
			Arguments: string(c.arguments),
		})
	}
	return out
}
