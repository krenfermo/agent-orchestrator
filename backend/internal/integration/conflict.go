package integration

import (
	"bytes"
	"strings"
)

// appendOnlyResolution is the ONLY conflict this package resolves by itself.
//
// The rule is narrow on purpose. A conflict qualifies when, for a file both
// sides edited:
//
//  1. the common ancestor's content is a whole-line PREFIX of both sides --
//     neither side changed or deleted a single existing line;
//  2. the lines each side added are disjoint -- no line was added by both, so
//     there is no "same change made twice" to deduplicate and no ordering
//     question between two versions of one line;
//  3. the ancestor ends on a line boundary, so appending cannot fuse a new
//     line onto an existing one;
//  4. nothing involved is binary.
//
// Under those four conditions there is exactly one sensible result -- the
// ancestor, then one side's additions, then the other's -- and producing it
// loses nothing from either side. That is what makes it deterministic and
// low-risk rather than a guess: this is the append-only file (a changelog, a
// registry list, a fixture index) that two independent tasks both added a line
// to, which is the common conflict between tasks that were correctly
// classified as non-overlapping.
//
// Everything else -- a changed line, a deleted line, a file only one side
// still has, the same line added twice, a binary blob -- returns false and
// becomes a Needs attention. Widening this function is how an integration
// coordinator starts silently choosing between two people's intentions.
//
// sideA is git's stage 2 and sideB its stage 3. Which physical branch each one
// is depends on the operation (rebase inverts them relative to a merge), which
// is why the result is defined by git's stage order rather than by "ours" and
// "theirs": the same inputs always produce the same output.
func appendOnlyResolution(base, sideA, sideB []byte) ([]byte, bool) {
	if isBinary(base) || isBinary(sideA) || isBinary(sideB) {
		return nil, false
	}
	baseLines := splitLines(base)
	aLines := splitLines(sideA)
	bLines := splitLines(sideB)

	if !linesHavePrefix(aLines, baseLines) || !linesHavePrefix(bLines, baseLines) {
		return nil, false
	}
	// An ancestor whose last line has no newline would have its final line
	// extended rather than followed by the first appended one.
	if len(baseLines) > 0 && !strings.HasSuffix(baseLines[len(baseLines)-1], "\n") {
		return nil, false
	}
	addedA := aLines[len(baseLines):]
	addedB := bLines[len(baseLines):]
	if len(addedA) == 0 || len(addedB) == 0 {
		// Only one side appended, so git would not have called this a conflict.
		// Reaching here means the file differs for some reason this rule has
		// not actually examined, and it must not be resolved on that basis.
		return nil, false
	}
	if sharesLine(addedA, addedB) {
		return nil, false
	}

	var out bytes.Buffer
	out.Grow(len(base) + len(sideA) + len(sideB))
	out.Write(base)
	for _, line := range addedA {
		out.WriteString(line)
	}
	// A side whose last appended line lacks a newline would fuse with the next
	// side's first line, so the boundary is restored explicitly.
	if n := out.Len(); n > 0 && out.Bytes()[n-1] != '\n' {
		out.WriteByte('\n')
	}
	for _, line := range addedB {
		out.WriteString(line)
	}
	return out.Bytes(), true
}

// splitLines splits content into lines that keep their terminators, so
// concatenating the result reproduces the input exactly.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return strings.SplitAfter(string(content), "\n")[:countLines(content)]
}

// countLines is how many entries splitLines keeps. strings.SplitAfter always
// yields a trailing empty element for content that ends in a newline, and that
// element is not a line.
func countLines(content []byte) int {
	n := strings.Count(string(content), "\n")
	if !bytes.HasSuffix(content, []byte("\n")) {
		n++
	}
	return n
}

func linesHavePrefix(lines, prefix []string) bool {
	if len(lines) < len(prefix) {
		return false
	}
	for i := range prefix {
		if lines[i] != prefix[i] {
			return false
		}
	}
	return true
}

func sharesLine(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, line := range a {
		seen[strings.TrimRight(line, "\r\n")] = struct{}{}
	}
	for _, line := range b {
		if _, ok := seen[strings.TrimRight(line, "\r\n")]; ok {
			return true
		}
	}
	return false
}

// isBinary uses git's own test: content with a NUL byte in it is not a text
// file, and nothing about lines applies to it.
func isBinary(content []byte) bool { return bytes.IndexByte(content, 0) >= 0 }
