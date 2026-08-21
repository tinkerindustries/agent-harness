package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The four fixed tools that reach an MCP server's resources and prompts
// (docs/MCP.md, "Resources", "Prompts"). They exist as four definitions
// rather than one per resource because the tool array is frozen for the
// life of a run: a server holding a documentation set would otherwise put
// hundreds of entries in every request, and any change to what it holds
// would move the array underneath a session already using it.
//
// They are offered only to a session that has MCP tools at all
// (definitions.go, WithMCP), so a run with no servers configured sends the
// array it always sent.

// mcpListResourcesArgs takes an optional server filter.
type mcpListResourcesArgs struct {
	Server string `json:"server"`
}

// execMCPListResources lists every enabled server's resources and resource
// templates from the stored snapshot.
func execMCPListResources(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	if e.MCP == nil {
		return errorResult("no MCP servers are configured")
	}
	var args mcpListResourcesArgs
	if len(argsRaw) > 0 && string(argsRaw) != "null" {
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return errorResult("decode arguments: %v", err)
		}
	}
	resources, err := e.MCP.Resources(ctx)
	if err != nil {
		return errorResult("list MCP resources: %v", err)
	}

	var b strings.Builder
	var shown int
	for _, r := range resources {
		if args.Server != "" && r.Server != args.Server {
			continue
		}
		shown++
		kind := "resource"
		if r.Template {
			kind = "template"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s", r.Server, kind, r.URI)
		if r.Name != "" {
			fmt.Fprintf(&b, "\t%s", r.Name)
		}
		if r.Description != "" {
			fmt.Fprintf(&b, "\t%s", r.Description)
		}
		b.WriteString("\n")
	}
	if shown == 0 {
		if args.Server != "" {
			return Result{Content: fmt.Sprintf("The %q MCP server advertises no resources.", args.Server)}
		}
		return Result{Content: "No configured MCP server advertises any resources."}
	}
	out, truncated := truncate(strings.TrimRight(b.String(), "\n"), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// mcpReadResourceArgs names one resource on one server.
type mcpReadResourceArgs struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// execMCPReadResource reads one resource, live.
func execMCPReadResource(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	if e.MCP == nil {
		return errorResult("no MCP servers are configured")
	}
	var args mcpReadResourceArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("decode arguments: %v", err)
	}
	if args.Server == "" || args.URI == "" {
		return errorResult("both server and uri are required")
	}
	content, err := e.MCP.ReadResource(ctx, args.Server, args.URI)
	if err != nil {
		return errorResult("%v", err)
	}
	return e.placeMCPContent(ctx, args.Server, "resource", content)
}

// mcpListPromptsArgs takes an optional server filter.
type mcpListPromptsArgs struct {
	Server string `json:"server"`
}

// execMCPListPrompts lists every enabled server's prompts, with the
// arguments each takes — a prompt named without its arguments is a prompt
// the model can only call wrong.
func execMCPListPrompts(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	if e.MCP == nil {
		return errorResult("no MCP servers are configured")
	}
	var args mcpListPromptsArgs
	if len(argsRaw) > 0 && string(argsRaw) != "null" {
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return errorResult("decode arguments: %v", err)
		}
	}
	prompts, err := e.MCP.Prompts(ctx)
	if err != nil {
		return errorResult("list MCP prompts: %v", err)
	}

	var b strings.Builder
	var shown int
	for _, p := range prompts {
		if args.Server != "" && p.Server != args.Server {
			continue
		}
		shown++
		fmt.Fprintf(&b, "%s\t%s", p.Server, p.Name)
		if p.Description != "" {
			fmt.Fprintf(&b, "\t%s", p.Description)
		}
		if len(p.Arguments) > 0 {
			names := make([]string, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				if a.Required {
					names = append(names, a.Name+" (required)")
					continue
				}
				names = append(names, a.Name)
			}
			sort.Strings(names)
			fmt.Fprintf(&b, "\targuments: %s", strings.Join(names, ", "))
		}
		b.WriteString("\n")
	}
	if shown == 0 {
		if args.Server != "" {
			return Result{Content: fmt.Sprintf("The %q MCP server advertises no prompts.", args.Server)}
		}
		return Result{Content: "No configured MCP server advertises any prompts."}
	}
	out, truncated := truncate(strings.TrimRight(b.String(), "\n"), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// mcpGetPromptArgs names one prompt and the arguments to render it with.
type mcpGetPromptArgs struct {
	Server    string            `json:"server"`
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

// execMCPGetPrompt renders one prompt, live.
func execMCPGetPrompt(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	if e.MCP == nil {
		return errorResult("no MCP servers are configured")
	}
	var args mcpGetPromptArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("decode arguments: %v", err)
	}
	if args.Server == "" || args.Name == "" {
		return errorResult("both server and name are required")
	}
	content, err := e.MCP.GetPrompt(ctx, args.Server, args.Name, args.Arguments)
	if err != nil {
		return errorResult("%v", err)
	}
	return e.placeMCPContent(ctx, args.Server, "prompt", content)
}
