package flc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"unicode/utf8"
)

// Source is one Go file. All offsets used internally are BYTE offsets into Original (BOM included), which is what
// go/token and Go tooling (gopls uses UTF-16 only at the LSP boundary) work with. UTF-16 offsets are derived for
// IntelliJ/LSP parity: UTF-16 code units into the decoded text with the BOM removed (identical to the C# pipeline).
//
// Line breaks: LF and CRLF only (the Go scanner treats '\r' as whitespace). Files with a lone CR are rejected in
// discovery (lone_cr), so a "line" is unambiguous for both Go tooling and IntelliJ.
type Source struct {
	Original []byte
	BOMLen   int // 0 or 3
	Sha256   string
	// LineStarts[i] = byte offset of the first byte of line i. LineContentEnd[i] = end of content (excl. CR LF).
	LineStarts     []int
	LineContentEnd []int
	LineBreakEnd   []int // offset after the line break (== content end at EOF)
	lineUTF16Start []int // UTF-16 offset of LineStarts[i] (BOM excluded)
}

var (
	ErrInvalidUTF8 = errors.New("invalid_utf8")
	ErrUTF16       = errors.New("utf16_encoding")
	ErrNUL         = errors.New("binary_nul")
	ErrLoneCR      = errors.New("lone_cr")
)

func Sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// DecodeSource checks the encoding policy and builds the line/offset tables.
func DecodeSource(b []byte) (*Source, error) {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		return nil, ErrUTF16
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, ErrNUL
	}
	s := &Source{Original: b, Sha256: Sha256Hex(b)}
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		s.BOMLen = 3
	}
	if !utf8.Valid(b[s.BOMLen:]) {
		return nil, ErrInvalidUTF8
	}
	start := s.BOMLen
	u16 := 0
	for i := s.BOMLen; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			end := i
			if end > start && b[end-1] == '\r' {
				end--
			}
			if bytes.IndexByte(b[start:end], '\r') >= 0 {
				return nil, ErrLoneCR
			}
			s.LineStarts = append(s.LineStarts, start)
			s.LineContentEnd = append(s.LineContentEnd, end)
			s.lineUTF16Start = append(s.lineUTF16Start, u16)
			brk := i
			if i < len(b) {
				brk = i + 1
			}
			s.LineBreakEnd = append(s.LineBreakEnd, brk)
			u16 += utf16Len(b[start:brk])
			start = i + 1
			if i == len(b) {
				break
			}
			if i+1 == len(b) {
				// file ends with a newline: the final empty "line" is still a valid caret line (EOF)
				s.LineStarts = append(s.LineStarts, start)
				s.LineContentEnd = append(s.LineContentEnd, start)
				s.LineBreakEnd = append(s.LineBreakEnd, start)
				s.lineUTF16Start = append(s.lineUTF16Start, u16)
				break
			}
		}
	}
	return s, nil
}

// Text returns the decoded text (BOM removed).
func (s *Source) Text() []byte { return s.Original[s.BOMLen:] }

func (s *Source) NumLines() int { return len(s.LineStarts) }

// LineOf returns the zero-based line containing byte offset off.
func (s *Source) LineOf(off int) int {
	i := sort.Search(len(s.LineStarts), func(i int) bool { return s.LineStarts[i] > off }) - 1
	if i < 0 {
		return 0
	}
	return i
}

// UTF16 converts a byte offset (at a rune boundary) into a UTF-16 offset of the decoded text.
func (s *Source) UTF16(off int) int {
	if off <= s.BOMLen {
		return 0
	}
	l := s.LineOf(off)
	return s.lineUTF16Start[l] + utf16Len(s.Original[s.LineStarts[l]:off])
}

// ByteOfUTF16 converts a UTF-16 offset of the decoded text back to a byte offset. ok=false if it splits a surrogate
// pair or is out of range.
func (s *Source) ByteOfUTF16(u int) (int, bool) {
	l := sort.Search(len(s.lineUTF16Start), func(i int) bool { return s.lineUTF16Start[i] > u }) - 1
	if l < 0 {
		return 0, false
	}
	off := s.LineStarts[l]
	cur := s.lineUTF16Start[l]
	for cur < u {
		if off >= len(s.Original) {
			return 0, false
		}
		r, n := utf8.DecodeRune(s.Original[off:])
		off += n
		if r >= 0x10000 {
			cur += 2
		} else {
			cur++
		}
	}
	return off, cur == u
}

// BackByUTF16 returns the smallest byte offset >= floor such that utf16(caret)-utf16(start) <= budget, never splitting a
// rune (and so never a surrogate pair).
func (s *Source) BackByUTF16(caret, budget, floor int) int {
	target := s.UTF16(caret) - budget
	if target <= s.UTF16(floor) {
		return floor
	}
	b, ok := s.ByteOfUTF16(target)
	if !ok { // target splits a surrogate pair: move right to the next rune boundary
		b, _ = s.ByteOfUTF16(target + 1)
	}
	return b
}

// ForwardByUTF16 returns the largest byte offset <= len such that utf16(end)-utf16(from) <= budget.
func (s *Source) ForwardByUTF16(from, budget int) int {
	off, cnt := from, 0
	for off < len(s.Original) {
		r, n := utf8.DecodeRune(s.Original[off:])
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if cnt+w > budget {
			break
		}
		cnt += w
		off += n
	}
	return off
}

func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		b = b[size:]
	}
	return n
}

// UTF16Len counts UTF-16 code units of a valid UTF-8 string.
func UTF16Len(s string) int { return utf16Len([]byte(s)) }

func isHSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\v' || c == '\f' }

// TrimRightHSpace returns the end offset of b[start:end] with trailing horizontal whitespace removed.
func TrimRightHSpace(b []byte, start, end int) int {
	for end > start && isHSpace(b[end-1]) {
		end--
	}
	return end
}
