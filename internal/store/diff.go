package store

import "strings"

// DiffLine tags in ComputeDiff's output. A context line is unchanged and
// carries both line numbers; add and remove carry only the side they belong
// to (docs/DESIGN.md §5.4: "Go computes diffs and sends structured line
// arrays. The browser renders a table. No diff algorithm runs in a render
// pass.").
const (
	DiffContext = "context"
	DiffAdd     = "add"
	DiffRemove  = "remove"
)

// DiffLine is one line of a computed diff, in display order.
type DiffLine struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	OldLine int    `json:"old_line,omitempty"`
	NewLine int    `json:"new_line,omitempty"`
}

// maxLCSLines bounds the O(n*m) table ComputeDiff builds. Edit's old_string
// and new_string are normally a targeted snippet rather than a whole file, so
// this ceiling is generous; a text past it falls back to a coarse
// remove-all/add-all diff instead of allocating an oversized table.
const maxLCSLines = 800

// ComputeDiff aligns oldText and newText line by line via longest common
// subsequence, so unchanged lines show as context instead of every line
// re-appearing as a remove paired with an add. Line numbers are 1-based.
func ComputeDiff(oldText, newText string) []DiffLine {
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	if len(oldLines) > maxLCSLines || len(newLines) > maxLCSLines {
		return coarseDiff(oldLines, newLines)
	}
	return lcsDiff(oldLines, newLines)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func coarseDiff(oldLines, newLines []string) []DiffLine {
	out := make([]DiffLine, 0, len(oldLines)+len(newLines))
	for i, l := range oldLines {
		out = append(out, DiffLine{Kind: DiffRemove, Text: l, OldLine: i + 1})
	}
	for i, l := range newLines {
		out = append(out, DiffLine{Kind: DiffAdd, Text: l, NewLine: i + 1})
	}
	return out
}

// lcsDiff builds the standard longest-common-subsequence table bottom-up,
// then walks it forward to emit context/remove/add in one pass.
func lcsDiff(oldLines, newLines []string) []DiffLine {
	n, m := len(oldLines), len(newLines)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	out := make([]DiffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			out = append(out, DiffLine{Kind: DiffContext, Text: oldLines[i], OldLine: i + 1, NewLine: j + 1})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, DiffLine{Kind: DiffRemove, Text: oldLines[i], OldLine: i + 1})
			i++
		default:
			out = append(out, DiffLine{Kind: DiffAdd, Text: newLines[j], NewLine: j + 1})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Kind: DiffRemove, Text: oldLines[i], OldLine: i + 1})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Kind: DiffAdd, Text: newLines[j], NewLine: j + 1})
	}
	return out
}
