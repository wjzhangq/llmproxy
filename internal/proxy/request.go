package proxy

import (
	"encoding/json"
	"sort"
)

// reqInfo is what the proxy learns from a tracked request body.
type reqInfo struct {
	clientModel   string
	stream        bool
	injected      bool // include_usage was added by the proxy
	contentBytes  int64
	imageCount    int
	parsed        bool
	forwardedBody []byte
}

// span is a byte range [start, end) of the body.
type span struct{ start, end int }

// reqScan holds the positions found by one pass over a request body.
type reqScan struct {
	open, close int // positions of the top-level '{' and '}'
	members     int
	model       *span
	streamOpts  *span
	soClose     int   // '}' of stream_options
	soMembers   int   // members inside stream_options
	soUsage     *span // value of stream_options.include_usage
	soUsageTrue bool
}

// edit replaces body[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// prepareRequest walks the client body once: it extracts metrics, rewrites
// "model" and (optionally) injects stream_options.include_usage. Only the
// changed values are spliced in; every other byte is forwarded as sent. A
// body that is not a JSON object is forwarded unchanged.
func prepareRequest(body []byte, upstreamModel string, injectUsage bool) reqInfo {
	info := reqInfo{forwardedBody: body}
	var rs reqScan
	s := scanner{b: body}
	s.ws()
	rs.open = s.i
	ok := s.object(func(key []byte) bool {
		rs.members++
		start := s.peekPos()
		var ok bool
		switch string(key) {
		case "model":
			ok = s.skip()
			rs.model = &span{start, s.i}
			info.clientModel = ""
			_ = json.Unmarshal(body[start:s.i], &info.clientModel)
		case "stream":
			ok = s.skip()
			info.stream = string(body[start:s.i]) == "true"
		case "stream_options":
			ok = s.streamOptions(&rs)
			rs.streamOpts = &span{start, s.i}
		case "messages":
			ok = s.messages(&info)
		case "prompt":
			ok = s.prompt(&info)
		default:
			ok = s.skip()
		}
		return ok
	})
	if !ok {
		return info
	}
	rs.close = s.i - 1
	s.ws()
	if s.i != len(body) {
		return info
	}
	info.parsed = true

	modelJSON, _ := json.Marshal(upstreamModel)
	var edits []edit
	if rs.model != nil {
		edits = append(edits, edit{rs.model.start, rs.model.end, string(modelJSON)})
	} else {
		text := `"model":` + string(modelJSON)
		if rs.members > 0 {
			text += ","
		}
		edits = append(edits, edit{rs.open + 1, rs.open + 1, text})
		rs.members++
	}
	if injectUsage && info.stream {
		switch {
		case rs.streamOpts == nil:
			edits = append(edits, edit{rs.close, rs.close, `,"stream_options":{"include_usage":true}`})
			info.injected = true
		case string(body[rs.streamOpts.start:rs.streamOpts.end]) == "null":
			edits = append(edits, edit{rs.streamOpts.start, rs.streamOpts.end, `{"include_usage":true}`})
			info.injected = true
		case rs.soClose > 0 && rs.soUsage == nil:
			text := `"include_usage":true`
			if rs.soMembers > 0 {
				text = "," + text
			}
			edits = append(edits, edit{rs.soClose, rs.soClose, text})
			info.injected = true
		case rs.soUsage != nil && !rs.soUsageTrue:
			edits = append(edits, edit{rs.soUsage.start, rs.soUsage.end, "true"})
			info.injected = true
		}
	}
	info.forwardedBody = applyEdits(body, edits)
	return info
}

func applyEdits(body []byte, edits []edit) []byte {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	n := len(body)
	for _, e := range edits {
		n += len(e.text) - (e.end - e.start)
	}
	out := make([]byte, 0, n)
	pos := 0
	for _, e := range edits {
		out = append(out, body[pos:e.start]...)
		out = append(out, e.text...)
		pos = e.end
	}
	return append(out, body[pos:]...)
}

// peekPos returns the position of the next non-space byte.
func (s *scanner) peekPos() int {
	s.ws()
	return s.i
}

func (s *scanner) streamOptions(rs *reqScan) bool {
	if s.peek() != '{' {
		return s.skip()
	}
	ok := s.object(func(key []byte) bool {
		rs.soMembers++
		if string(key) != "include_usage" {
			return s.skip()
		}
		start := s.peekPos()
		if !s.skip() {
			return false
		}
		rs.soUsage = &span{start, s.i}
		rs.soUsageTrue = string(s.b[start:s.i]) == "true"
		return true
	})
	rs.soClose = s.i - 1
	return ok
}

// messages counts text bytes and images of a chat "messages" array.
func (s *scanner) messages(info *reqInfo) bool {
	if s.peek() != '[' {
		return s.skip()
	}
	return s.array(func() bool {
		if s.peek() != '{' {
			return s.skip()
		}
		return s.object(func(key []byte) bool {
			if string(key) != "content" {
				return s.skip()
			}
			switch s.peek() {
			case '"':
				n, ok := s.str()
				info.contentBytes += int64(n)
				return ok
			case '[':
				return s.array(func() bool { return s.contentPart(info) })
			}
			return s.skip()
		})
	})
}

// contentPart handles one element of a multimodal content array: only
// type=text counts towards bytes, image parts are counted.
func (s *scanner) contentPart(info *reqInfo) bool {
	if s.peek() != '{' {
		return s.skip()
	}
	var typ []byte
	textLen := 0
	ok := s.object(func(key []byte) bool {
		switch string(key) {
		case "type":
			if s.peek() != '"' {
				return s.skip()
			}
			start := s.i + 1
			if _, ok := s.str(); !ok {
				return false
			}
			typ = s.b[start : s.i-1]
			return true
		case "text":
			if s.peek() != '"' {
				return s.skip()
			}
			n, ok := s.str()
			textLen = n
			return ok
		}
		return s.skip()
	})
	switch string(typ) {
	case "text":
		info.contentBytes += int64(textLen)
	case "image_url", "image", "input_image":
		info.imageCount++
	}
	return ok
}

// prompt counts the bytes of a completions "prompt" (string or array of
// strings).
func (s *scanner) prompt(info *reqInfo) bool {
	switch s.peek() {
	case '"':
		n, ok := s.str()
		info.contentBytes += int64(n)
		return ok
	case '[':
		return s.array(func() bool {
			if s.peek() != '"' {
				return s.skip()
			}
			n, ok := s.str()
			info.contentBytes += int64(n)
			return ok
		})
	}
	return s.skip()
}
