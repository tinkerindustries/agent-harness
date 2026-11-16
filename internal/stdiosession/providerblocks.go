package stdiosession

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Claude's response blocks are stored whole, as store.ReasoningDeltaPayload's
// ProviderBlocks, because the next request has to replay them byte for byte.
// That makes them the only place the structure a flattened transcript loses
// survives: which text a citation supports, and what a server-side search or
// fetch did. This file reads them for the parent, and only for what is safe to
// show. The blocks also carry encrypted search results, citation indices and
// thinking signatures, which exist for replay and mean nothing to a parent, so
// none of them is copied out.

// pbCitation is one citation attached to a text block, narrowed to what a
// parent can display. Kind is Anthropic's own citation type
// (web_search_result_location, char_location, page_location, ...).
type pbCitation struct {
	Kind      string
	URL       string
	Title     string
	CitedText string
}

// pbTextBlock is one text block of the response, in order. Concatenating
// every block's Text gives the answer text the loop committed.
type pbTextBlock struct {
	Text      string
	Citations []pbCitation
}

// pbSource is one page a server-side search returned or a fetch retrieved.
type pbSource struct {
	URL   string
	Title string
}

// pbServerTool is one server-side tool call and what came back: Anthropic runs
// web_search and web_fetch itself, so the call and its result arrive in the
// same response rather than through the harness's tool path.
type pbServerTool struct {
	ID      string
	Name    string
	Input   json.RawMessage
	Sources []pbSource
	// Error is the provider's error code when the tool failed, such as
	// "max_uses_exceeded" or "url_not_accessible".
	Error string
}

// pbProjection is the parent-visible part of one response's provider blocks.
type pbProjection struct {
	Text        []pbTextBlock
	ServerTools []pbServerTool
}

// projectProviderBlocks reads a response's stored blocks. Anything it does not
// recognise, or cannot parse, is left out: a parent loses an annotation it
// would have shown, never the answer.
func projectProviderBlocks(raw json.RawMessage) pbProjection {
	var out pbProjection
	if len(raw) == 0 {
		return out
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return out
	}
	byID := map[string]int{}
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) != nil {
			continue
		}
		switch {
		case head.Type == "text":
			var blk struct {
				Text      string `json:"text"`
				Citations []struct {
					Type          string `json:"type"`
					URL           string `json:"url"`
					Title         string `json:"title"`
					DocumentTitle string `json:"document_title"`
					CitedText     string `json:"cited_text"`
				} `json:"citations"`
			}
			if json.Unmarshal(b, &blk) != nil {
				continue
			}
			tb := pbTextBlock{Text: blk.Text}
			for _, c := range blk.Citations {
				title := c.Title
				if title == "" {
					title = c.DocumentTitle
				}
				tb.Citations = append(tb.Citations, pbCitation{Kind: c.Type, URL: c.URL, Title: title, CitedText: c.CitedText})
			}
			out.Text = append(out.Text, tb)
		case head.Type == "server_tool_use":
			var blk struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(b, &blk) != nil || blk.ID == "" {
				continue
			}
			byID[blk.ID] = len(out.ServerTools)
			out.ServerTools = append(out.ServerTools, pbServerTool{ID: blk.ID, Name: blk.Name, Input: blk.Input})
		case strings.HasSuffix(head.Type, "_tool_result"):
			var blk struct {
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(b, &blk) != nil {
				continue
			}
			i, ok := byID[blk.ToolUseID]
			if !ok {
				continue
			}
			sources, errCode := pbResultSources(blk.Content)
			out.ServerTools[i].Sources = sources
			out.ServerTools[i].Error = errCode
		}
	}
	return out
}

// pbResultSources reads a server tool result's content for the pages it names
// and the error code it failed with. A search result is an array of pages; a
// fetch result is one object holding the page; either is an error object when
// the tool failed. The pages' own content is encrypted for replay and never
// read.
func pbResultSources(content json.RawMessage) ([]pbSource, string) {
	if len(content) == 0 {
		return nil, ""
	}
	type page struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		ErrorCode string `json:"error_code"`
		Content   struct {
			Title string `json:"title"`
		} `json:"content"`
	}
	var pages []page
	if json.Unmarshal(content, &pages) != nil {
		var one page
		if json.Unmarshal(content, &one) != nil {
			return nil, ""
		}
		pages = []page{one}
	}
	var out []pbSource
	for _, p := range pages {
		if p.ErrorCode != "" {
			return nil, p.ErrorCode
		}
		if p.URL == "" {
			continue
		}
		title := p.Title
		if title == "" {
			title = p.Content.Title
		}
		out = append(out, pbSource{URL: p.URL, Title: title})
	}
	return out, ""
}

// matches reports whether the response's text blocks concatenate to text, the
// answer the loop committed. A projection is only attached to text it
// describes: if the two disagree — a block the translator never saw, an edit
// to the committed text — an annotation's offsets would point at the wrong
// characters, so none is attached.
func (p pbProjection) matches(text string) bool {
	var b strings.Builder
	for _, t := range p.Text {
		b.WriteString(t.Text)
	}
	return b.String() == text
}

// annotated reports whether any text block carries a citation.
func (p pbProjection) annotated() bool {
	for _, t := range p.Text {
		if len(t.Citations) > 0 {
			return true
		}
	}
	return false
}

// runeLen is a string's length in characters, the unit annotation offsets are
// given in.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
