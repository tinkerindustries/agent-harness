package mcpclient

import (
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// flatten turns one server's raw CallToolResult into the vocabulary
// internal/tools.Executor consumes (docs/MCP.md, "Calling"): text content
// blocks joined with a blank line between them, image blocks collected
// separately rather than dropped, any other content type noted with a
// short placeholder line instead of silently vanishing, and
// StructuredContent marshalled as JSON when the call produced no text at
// all. A result with nothing usable in it becomes the text "(no content)"
// — Manager.Call still returns a value the model can see and route around,
// never an empty one that looks like the executor lost something.
func flatten(res *mcpsdk.CallToolResult) tools.MCPContent {
	var texts []string
	var images []tools.MCPImage
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			texts = append(texts, v.Text)
		case *mcpsdk.ImageContent:
			images = append(images, tools.MCPImage{MIMEType: v.MIMEType, Data: v.Data})
		default:
			texts = append(texts, fmt.Sprintf("[unsupported content type: %T]", c))
		}
	}

	text := strings.Join(texts, "\n\n")
	if text == "" && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			text = string(b)
		}
	}
	if text == "" && len(images) == 0 {
		text = "(no content)"
	}

	return tools.MCPContent{
		Text:    text,
		Images:  images,
		IsError: res.IsError,
	}
}
