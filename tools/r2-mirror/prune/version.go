package prune

import "strings"

// byVersionSort orders version/stamp strings ascending the way GNU `sort -V`
// does, so the newest is last. This matches tools/prune-releases.sh (real
// `sort -V`) on which stamps are the newest-N — including the trailing git-sha
// tie-break, where alpha-leading and numeric-leading shas interleave in a way
// that field-wise lexical comparison gets wrong.
//
// This file is a verbatim copy of the same port in the Burrowee release repo
// (internal/register/prune.go). Both buckets are pruned by the same retention
// rule, so the two must agree on "newest"; if one is corrected, copy the fix
// rather than re-deriving it.
type byVersionSort []string

func (s byVersionSort) Len() int           { return len(s) }
func (s byVersionSort) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s byVersionSort) Less(i, j int) bool { return versionLess(s[i], s[j]) }

// versionLess reports whether a sorts before b the way GNU `sort -V` does,
// matching it for our stamp shapes (vX.Y.Z[.date.sha], no '~'). `sort -V` strips
// a trailing file suffix from each string, compares the prefixes with the
// coreutils version comparator (verrevcmp), and breaks a tie with a plain byte
// comparison of the original strings. That suffix handling is what makes the
// trailing git-sha tie-break agree with `sort -V`: an alpha-leading sha forms a
// strippable ".<sha>" suffix (so the prefixes tie and the byte order of the full
// stamp decides), while a numeric-leading sha does not — which a naive
// field-wise comparison gets wrong. Our stamps are never empty and never start
// with '.' / '~', so filevercmp's leading-dot/empty special cases are omitted.
func versionLess(a, b string) bool { return filevercmp(a, b) < 0 }

func filevercmp(a, b string) int {
	if c := verrevcmp(a[:filePrefixLen(a)], b[:filePrefixLen(b)]); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// filePrefixLen returns the length of the prefix of s left after removing the
// longest trailing file suffix matching (\.[A-Za-z~][A-Za-z0-9~]*)*$, never
// consuming all of a non-empty s. Port of coreutils file_prefixlen.
func filePrefixLen(s string) int {
	n := len(s)
	prefixLen := 0
	for i := 0; ; {
		if i == n {
			return prefixLen
		}
		i++
		prefixLen = i
		for i+1 < n && s[i] == '.' && (isASCIILetter(s[i+1]) || s[i+1] == '~') {
			for i += 2; i < n && (isASCIIAlnum(s[i]) || s[i] == '~'); i++ {
			}
		}
	}
}

// order maps the byte at pos to its non-digit-phase sort weight, per coreutils
// filevercmp.c order: past-the-end is -1, '~' is -2, a digit weighs 0, a letter
// keeps its byte value, and any other byte is pushed above the letters (+256).
// The digit weight of 0 (vs -1 for end-of-string) is what makes a longer string
// sort after a shorter one when the extra run starts with a digit.
func order(s string, pos int) int {
	if pos == len(s) {
		return -1
	}
	c := s[pos]
	switch {
	case isASCIIDigit(c):
		return 0
	case isASCIILetter(c):
		return int(c)
	case c == '~':
		return -2
	default:
		return int(c) + 256
	}
}

func isASCIIDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isASCIILetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
func isASCIIAlnum(c byte) bool  { return isASCIIDigit(c) || isASCIILetter(c) }

// verrevcmp returns -1/0/1 comparing a and b as GNU version strings. Port of
// coreutils filevercmp.c verrevcmp.
func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		firstDiff := 0
		// Non-digit phase: compare byte by byte by order weight until both sides
		// reach a digit (or end). A digit weighs 0 and end weighs -1, so the loop
		// stops when both are at digits, and a shorter side that ends here sorts
		// before a longer side continuing with a digit.
		for (i < len(a) && !isASCIIDigit(a[i])) || (j < len(b) && !isASCIIDigit(b[j])) {
			ac := order(a, i)
			bc := order(b, j)
			if ac != bc {
				return sign(ac - bc)
			}
			i++
			j++
		}
		// Digit phase: skip leading zeros, then compare digit runs by length
		// (longer run is the larger number) with a tie broken by the first
		// differing digit.
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		for i < len(a) && j < len(b) && isASCIIDigit(a[i]) && isASCIIDigit(b[j]) {
			if firstDiff == 0 {
				firstDiff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isASCIIDigit(a[i]) {
			return 1
		}
		if j < len(b) && isASCIIDigit(b[j]) {
			return -1
		}
		if firstDiff != 0 {
			return sign(firstDiff)
		}
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
