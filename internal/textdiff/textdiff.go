// Package textdiff renders a unified diff between two versions of a small
// configuration file. It exists so every previewed file edit in tui-network —
// a .network file, a dnsmasq drop-in — shows the same kind of diff in its
// confirm dialog: the setting that changed, with a little context, and nothing
// else.
//
// It is a real line diff (a longest-common-subsequence walk) rather than
// "everything out, everything in", because the confirm dialog for a file edit
// has one job: show the setting that changed. A diff that repeats the whole
// file every time a comment moved buries exactly that.
//
// The files here are a few dozen lines, so the quadratic table costs nothing.
package textdiff

import (
	"fmt"
	"strings"
)

// Context is how many unchanged lines are shown around a change. Two is enough
// to place a changed line in its section without turning a short file into a
// wall of text in the confirm dialog.
const Context = 2

// Unified renders a unified diff between two versions of a file. It returns the
// empty string when nothing would change.
func Unified(path, before, after string) string {
	if before == after {
		return ""
	}
	oldLines, newLines := splitLines(before), splitLines(after)
	ops := diffOps(oldLines, newLines)
	hunks := hunksOf(ops)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n", labelFor(path, before))
	fmt.Fprintf(&b, "+++ %s\n", path)
	for _, hunk := range hunks {
		oldCount, newCount := 0, 0
		for _, op := range hunk.ops {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
			hunk.oldStart+1, oldCount, hunk.newStart+1, newCount)
		for _, op := range hunk.ops {
			fmt.Fprintf(&b, "%c%s\n", op.kind, op.text)
		}
	}
	return b.String()
}

// op is one line of a diff: kept (' '), removed ('-') or added ('+').
type op struct {
	kind byte
	text string
	// oldIndex and newIndex are the line's position in each file, used to
	// number the hunk headers.
	oldIndex, newIndex int
}

// diffOps walks the longest common subsequence of the two line lists and
// returns the operations that turn the first into the second.
func diffOps(oldLines, newLines []string) []op {
	// lcs[i][j] is the length of the longest common subsequence of
	// oldLines[i:] and newLines[j:].
	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}

	var ops []op
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, op{' ', oldLines[i], i, j})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', oldLines[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', newLines[j], i, j})
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		ops = append(ops, op{'-', oldLines[i], i, j})
	}
	for ; j < len(newLines); j++ {
		ops = append(ops, op{'+', newLines[j], i, j})
	}
	return ops
}

// hunk is a run of changes with its surrounding context.
type hunk struct {
	oldStart, newStart int
	ops                []op
}

// hunksOf groups the operations into hunks, keeping Context unchanged lines
// around each change and merging changes that are close enough to share their
// context.
func hunksOf(ops []op) []hunk {
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		for j := max(i-Context, 0); j <= min(i+Context, len(ops)-1); j++ {
			keep[j] = true
		}
	}

	var hunks []hunk
	var current *hunk
	for i, o := range ops {
		if !keep[i] {
			current = nil
			continue
		}
		if current == nil {
			hunks = append(hunks, hunk{oldStart: o.oldIndex, newStart: o.newIndex})
			current = &hunks[len(hunks)-1]
		}
		current.ops = append(current.ops, o)
	}
	return hunks
}

// labelFor names the left side of the diff: the file, or /dev/null when it does
// not exist yet.
func labelFor(path, before string) string {
	if before == "" {
		return "/dev/null"
	}
	return path
}

// splitLines splits a file into lines, dropping the empty element a trailing
// newline produces.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
