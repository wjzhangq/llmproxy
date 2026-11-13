package proxy

import (
	"bytes"
	"unicode/utf8"
)

// chunkInfo is what the proxy needs from one streaming chunk.
type chunkInfo struct {
	contentBytes int64 // content + reasoning + text + tool call argument bytes
	hasContent   bool  // any non-empty content, reasoning, text or tool call
	choices      int   // number of entries in "choices"
	hasUsage     bool
	usage        usage
	hasError     bool
}

// scanChunk extracts chunkInfo from a streaming chunk without allocating.
// It understands only the shapes OpenAI-compatible servers emit; ok is false
// for anything else so the caller can fall back to encoding/json.
func scanChunk(b []byte) (info chunkInfo, ok bool) {
	s := scanner{b: b}
	var seen uint32
	ok = s.object(func(key []byte) bool {
		switch s.key(key, &seen, "choices", "usage", "error") {
		case 0:
			return s.array(func() bool {
				info.choices++
				var seen uint32
				return s.object(func(key []byte) bool {
					switch s.key(key, &seen, "delta", "text") {
					case 0:
						return s.delta(&info)
					case 1:
						return s.stringInto(&info)
					}
					return s.skip()
				})
			})
		case 1:
			if s.null() {
				return true
			}
			info.hasUsage = true
			var seen uint32
			return s.object(func(key []byte) bool {
				switch s.key(key, &seen, "prompt_tokens", "completion_tokens") {
				case 0:
					return s.intPtr(&info.usage.PromptTokens)
				case 1:
					return s.intPtr(&info.usage.CompletionTokens)
				}
				return s.skip()
			})
		case 2:
			if !s.null() {
				info.hasError = true
				return s.skip()
			}
			return true
		}
		return s.skip()
	})
	if ok {
		s.ws()
		ok = s.i == len(s.b) && !s.bad
	}
	return info, ok
}

func (s *scanner) delta(info *chunkInfo) bool {
	if s.null() {
		return true
	}
	reasoning := int64(-1)
	var seen uint32
	ok := s.object(func(key []byte) bool {
		switch s.key(key, &seen, "content", "reasoning_content", "reasoning", "tool_calls") {
		case 0:
			return s.stringInto(info)
		case 1, 2:
			// Some servers send both with the same text; count one.
			var tmp chunkInfo
			if !s.stringInto(&tmp) {
				return false
			}
			if tmp.contentBytes > reasoning {
				reasoning = tmp.contentBytes
			}
			return true
		case 3:
			if s.null() {
				return true
			}
			return s.array(func() bool {
				info.hasContent = true
				var seen uint32
				return s.object(func(key []byte) bool {
					if s.key(key, &seen, "function") != 0 {
						return s.skip()
					}
					if s.null() {
						return true
					}
					var seen uint32
					return s.object(func(key []byte) bool {
						if s.key(key, &seen, "arguments") == 0 {
							var tmp chunkInfo
							if !s.stringInto(&tmp) {
								return false
							}
							info.contentBytes += tmp.contentBytes
							return true
						}
						return s.skip()
					})
				})
			})
		}
		return s.skip()
	})
	if reasoning > 0 {
		info.contentBytes += reasoning
		info.hasContent = true
	}
	return ok
}

// scanner is a minimal JSON walker over a byte slice.
type scanner struct {
	b     []byte
	i     int
	bad   bool // input needs encoding/json semantics; fall back
	depth int
}

// maxDepth bounds nesting like encoding/json does, so hostile input cannot
// exhaust the stack.
const maxDepth = 10000

// key returns the index of the name key matches, or -1. encoding/json
// matches keys case-insensitively, unescapes them and keeps the last of
// duplicate keys; such inputs set s.bad so the caller falls back.
func (s *scanner) key(key []byte, seen *uint32, names ...string) int {
	if bytes.IndexByte(key, '\\') >= 0 {
		s.bad = true
		return -1
	}
	for i, n := range names {
		if string(key) == n || bytes.EqualFold(key, []byte(n)) {
			if string(key) != n || *seen&(1<<i) != 0 {
				s.bad = true
			}
			*seen |= 1 << i
			return i
		}
	}
	return -1
}

func (s *scanner) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

func (s *scanner) peek() byte {
	s.ws()
	if s.i < len(s.b) {
		return s.b[s.i]
	}
	return 0
}

func (s *scanner) null() bool {
	if s.peek() == 'n' && len(s.b)-s.i >= 4 && string(s.b[s.i:s.i+4]) == "null" {
		s.i += 4
		return true
	}
	return false
}

// object iterates the members of an object; key is the raw (escaped) key.
func (s *scanner) object(member func(key []byte) bool) bool {
	if s.peek() != '{' || s.depth >= maxDepth {
		return false
	}
	s.depth++
	defer func() { s.depth-- }()
	s.i++
	if s.peek() == '}' {
		s.i++
		return true
	}
	for {
		if s.peek() != '"' {
			return false
		}
		start := s.i + 1
		if _, ok := s.str(); !ok {
			return false
		}
		key := s.b[start : s.i-1]
		if s.peek() != ':' {
			return false
		}
		s.i++
		if !member(key) {
			return false
		}
		switch s.peek() {
		case ',':
			s.i++
		case '}':
			s.i++
			return true
		default:
			return false
		}
	}
}

func (s *scanner) array(elem func() bool) bool {
	if s.peek() != '[' || s.depth >= maxDepth {
		return false
	}
	s.depth++
	defer func() { s.depth-- }()
	s.i++
	if s.peek() == ']' {
		s.i++
		return true
	}
	for {
		if !elem() {
			return false
		}
		switch s.peek() {
		case ',':
			s.i++
		case ']':
			s.i++
			return true
		default:
			return false
		}
	}
}

// stringInto adds the decoded UTF-8 length of a string (or null) to info.
func (s *scanner) stringInto(info *chunkInfo) bool {
	if s.null() {
		return true
	}
	if s.peek() != '"' {
		return false
	}
	n, ok := s.str()
	if n > 0 {
		info.contentBytes += int64(n)
		info.hasContent = true
	}
	return ok
}

// str consumes a string starting at s.i and returns its decoded UTF-8 length.
func (s *scanner) str() (int, bool) {
	s.i++ // opening quote
	n := 0
	for s.i < len(s.b) {
		// Fast path: a run of plain printable ASCII.
		start := s.i
		for s.i < len(s.b) {
			c := s.b[s.i]
			if c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
				break
			}
			s.i++
		}
		n += s.i - start
		if s.i >= len(s.b) {
			break
		}
		c := s.b[s.i]
		switch {
		case c == '"':
			s.i++
			return n, true
		case c == '\\':
			if s.i+1 >= len(s.b) {
				return 0, false
			}
			switch s.b[s.i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				n++
				s.i += 2
				continue
			case 'u':
			default:
				return 0, false
			}
			r, ok := s.hex4(s.i + 2)
			if !ok {
				return 0, false
			}
			s.i += 6
			if r >= 0xD800 && r < 0xDC00 && s.i+1 < len(s.b) && s.b[s.i] == '\\' && s.b[s.i+1] == 'u' {
				if lo, ok := s.hex4(s.i + 2); ok && lo >= 0xDC00 && lo < 0xE000 {
					s.i += 6
					n += 4
					continue
				}
			}
			if r >= 0xD800 && r < 0xE000 {
				r = utf8.RuneError
			}
			n += utf8.RuneLen(r)
		case c < 0x20:
			return 0, false
		case c < utf8.RuneSelf:
			n++
			s.i++
		default:
			// Invalid UTF-8 is replaced by U+FFFD, as encoding/json does.
			r, size := utf8.DecodeRune(s.b[s.i:])
			if r == utf8.RuneError && size == 1 {
				n += 3
			} else {
				n += size
			}
			s.i += size
		}
	}
	return 0, false
}

func (s *scanner) hex4(at int) (rune, bool) {
	if at+4 > len(s.b) {
		return 0, false
	}
	var r rune
	for _, c := range s.b[at : at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

// intPtr parses a non-negative integer (or null) into *p.
func (s *scanner) intPtr(p **int64) bool {
	if s.null() {
		return true
	}
	s.ws()
	start := s.i
	var v int64
	for s.i < len(s.b) && s.b[s.i] >= '0' && s.b[s.i] <= '9' {
		v = v*10 + int64(s.b[s.i]-'0')
		s.i++
	}
	if s.i == start || s.i-start > 18 || (s.b[start] == '0' && s.i-start > 1) {
		return false
	}
	if s.i < len(s.b) && (s.b[s.i] == '.' || s.b[s.i] == 'e' || s.b[s.i] == 'E') {
		return false
	}
	*p = new(int64)
	**p = v
	return true
}

// skip consumes any JSON value.
func (s *scanner) skip() bool {
	switch c := s.peek(); {
	case c == '{':
		return s.object(func([]byte) bool { return s.skip() })
	case c == '[':
		return s.array(s.skip)
	case c == '"':
		_, ok := s.str()
		return ok
	case c == 't' || c == 'f' || c == 'n':
		for _, lit := range []string{"true", "false", "null"} {
			if len(s.b)-s.i >= len(lit) && string(s.b[s.i:s.i+len(lit)]) == lit {
				s.i += len(lit)
				return true
			}
		}
		return false
	case c == '-' || (c >= '0' && c <= '9'):
		return s.number()
	}
	return false
}

// number consumes a JSON number: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func (s *scanner) number() bool {
	digits := func() int {
		n := 0
		for s.i < len(s.b) && s.b[s.i] >= '0' && s.b[s.i] <= '9' {
			s.i++
			n++
		}
		return n
	}
	if s.i < len(s.b) && s.b[s.i] == '-' {
		s.i++
	}
	if s.i < len(s.b) && s.b[s.i] == '0' {
		s.i++
	} else if digits() == 0 {
		return false
	}
	if s.i < len(s.b) && s.b[s.i] == '.' {
		s.i++
		if digits() == 0 {
			return false
		}
	}
	if s.i < len(s.b) && (s.b[s.i] == 'e' || s.b[s.i] == 'E') {
		s.i++
		if s.i < len(s.b) && (s.b[s.i] == '+' || s.b[s.i] == '-') {
			s.i++
		}
		if digits() == 0 {
			return false
		}
	}
	return true
}
