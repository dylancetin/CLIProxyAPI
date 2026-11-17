package webtools

import "unicode"

// findContext is how many characters before a find match the window starts.
const findContext = 300

// window is one slice of a page's markdown, in code points.
type window struct {
	Start, End int
	Found      *bool
	MatchAt    int
}

// sliceWindow picks [offset, offset+maxChars) of md, moving the end back to a
// paragraph or line break in the last 20 % of the window. With find, the
// window starts up to findContext characters before the first match at or
// after offset (at a line start when possible).
func sliceWindow(md []rune, offset, maxChars int, find []rune) window {
	total := len(md)
	if offset > total {
		offset = total
	}
	w := window{Start: offset}
	if len(find) > 0 {
		match := indexFold(md, find, offset)
		found := match >= 0
		w.Found = &found
		if !found {
			w.End = offset
			return w
		}
		w.MatchAt = match
		start := match - findContext
		if start < 0 {
			start = 0
		}
		// Prefer the first line start between start and the match.
		if start > 0 && md[start-1] != '\n' {
			for i := start; i < match; i++ {
				if md[i] == '\n' {
					start = i + 1
					break
				}
			}
		}
		for start < match && md[start] == '\n' {
			start++
		}
		w.Start = start
	}
	end := w.Start + maxChars
	if end >= total {
		w.End = total
		return w
	}
	// Move the end back to a break within the last 20 %, never before a match.
	floor := end - maxChars/5
	if w.Found != nil && floor < w.MatchAt+len(find) {
		floor = w.MatchAt + len(find)
	}
	if cut := lastBreak(md, floor, end); cut > 0 {
		end = cut
	}
	w.End = end
	return w
}

// lastBreak returns the position just after the last paragraph break (or,
// failing that, line break) in md[from:to], or 0.
func lastBreak(md []rune, from, to int) int {
	if from < 1 {
		from = 1
	}
	for i := to - 1; i >= from; i-- {
		if md[i] == '\n' && md[i-1] == '\n' {
			return i + 1
		}
	}
	for i := to - 1; i >= from; i-- {
		if md[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// indexFold finds needle in haystack from start, case-insensitively and
// accent-sensitively. Rune-wise lowercasing keeps indexes aligned.
func indexFold(haystack, needle []rune, start int) int {
	if len(needle) == 0 {
		return -1
	}
	lowerNeedle := make([]rune, len(needle))
	for i, r := range needle {
		lowerNeedle[i] = unicode.ToLower(r)
	}
outer:
	for i := start; i+len(lowerNeedle) <= len(haystack); i++ {
		for j, r := range lowerNeedle {
			if unicode.ToLower(haystack[i+j]) != r {
				continue outer
			}
		}
		return i
	}
	return -1
}
