package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

type usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

type toolCall struct {
	Function struct {
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiError struct {
	Message string `json:"message"`
}

// errorField accepts both {"error":{"message":..}} and {"error":"..."}.
type errorField struct{ msg string }

func (e *errorField) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		e.msg = s
		return nil
	}
	var o apiError
	if json.Unmarshal(b, &o) == nil && o.Message != "" {
		e.msg = o.Message
		return nil
	}
	e.msg = string(b)
	return nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			Reasoning        string     `json:"reasoning"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"delta"`
		Text string `json:"text"`
	} `json:"choices"`
	Usage *usage      `json:"usage"`
	Error *errorField `json:"error"`
}

type fullResponse struct {
	Choices []struct {
		Message struct {
			Content          *string    `json:"content"`
			ReasoningContent *string    `json:"reasoning_content"`
			Reasoning        *string    `json:"reasoning"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
	Usage *usage      `json:"usage"`
	Error *errorField `json:"error"`
}

func toolBytes(tc []toolCall) int64 {
	var n int64
	for _, t := range tc {
		n += int64(len(t.Function.Arguments))
	}
	return n
}

// observation accumulates what is learnt from the upstream response.
type observation struct {
	start      time.Time
	firstToken time.Time
	respBytes  int64
	usage      *usage
	errMsg     string
}

// sseReader passes an SSE stream through event by event, observing each
// event and optionally dropping the usage-only chunk the proxy asked for.
type sseReader struct {
	src        io.ReadCloser
	obs        *observation
	dropUsage  bool
	buf        []byte
	pending    []byte
	out        []byte
	err        error
	sawContent bool
	scanned    int // bytes of pending already searched for an event end
}

func newSSEReader(src io.ReadCloser, obs *observation, dropUsage bool) *sseReader {
	return &sseReader{src: src, obs: obs, dropUsage: dropUsage, buf: make([]byte, 32<<10)}
}

func (s *sseReader) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			if len(s.pending) > 0 {
				s.out, s.pending = s.pending, nil
				break
			}
			return 0, s.err
		}
		n, err := s.src.Read(s.buf)
		if n > 0 {
			s.process(s.buf[:n])
		}
		if err != nil {
			s.err = err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *sseReader) Close() error { return s.src.Close() }

func (s *sseReader) process(b []byte) {
	s.pending = append(s.pending, b...)
	for {
		end, sepLen := eventEnd(s.pending, s.scanned)
		if end < 0 {
			// Resume next time just before the tail, in case a separator
			// is split across reads.
			s.scanned = max(0, len(s.pending)-2)
			return
		}
		s.scanned = 0
		ev := s.pending[:end+sepLen]
		if s.observe(s.pending[:end]) {
			s.out = append(s.out, ev...)
		}
		s.pending = s.pending[end+sepLen:]
	}
}

// eventEnd finds the blank line terminating the first SSE event, starting
// the search at from.
func eventEnd(b []byte, from int) (int, int) {
	for i := from; i < len(b); i++ {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			return -1, 0
		}
		i += j
		if i+1 < len(b) && b[i+1] == '\n' {
			return i, 2
		}
		if i+2 < len(b) && b[i+1] == '\r' && b[i+2] == '\n' {
			return i, 3
		}
	}
	return -1, 0
}

// eventData returns the concatenated "data:" field of an SSE event. It only
// allocates when the event has several data lines.
func eventData(ev []byte) []byte {
	var data []byte
	lines := 0
	for len(ev) > 0 {
		line := ev
		if i := bytes.IndexByte(ev, '\n'); i >= 0 {
			line, ev = ev[:i], ev[i+1:]
		} else {
			ev = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		v, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		v = bytes.TrimPrefix(v, []byte(" "))
		lines++
		switch lines {
		case 1:
			data = v
		case 2:
			data = append(append(append([]byte(nil), data...), '\n'), v...)
		default:
			data = append(append(data, '\n'), v...)
		}
	}
	return data
}

// observe inspects one event and reports whether it should be forwarded.
func (s *sseReader) observe(ev []byte) bool {
	data := eventData(ev)
	if len(data) == 0 || string(data) == "[DONE]" {
		return true
	}
	info, ok := scanChunk(data)
	if !ok || info.hasError {
		info, ok = slowChunk(data, s.obs)
		if !ok {
			return true
		}
	}
	s.obs.respBytes += info.contentBytes
	if !s.sawContent && info.hasContent {
		s.sawContent = true
		s.obs.firstToken = time.Now()
	}
	if info.hasUsage {
		u := info.usage
		s.obs.usage = &u
		if s.dropUsage && info.choices == 0 {
			return false
		}
	}
	return true
}

// slowChunk is the encoding/json fallback of scanChunk; it also extracts
// error messages.
func slowChunk(data []byte, obs *observation) (chunkInfo, bool) {
	var c streamChunk
	if json.Unmarshal(data, &c) != nil {
		return chunkInfo{}, false
	}
	if c.Error != nil && obs.errMsg == "" {
		obs.errMsg = c.Error.msg
	}
	info := chunkInfo{choices: len(c.Choices), hasError: c.Error != nil}
	for _, ch := range c.Choices {
		d := ch.Delta
		reasoning := d.ReasoningContent
		if len(d.Reasoning) > len(reasoning) {
			reasoning = d.Reasoning
		}
		info.contentBytes += int64(len(d.Content)+len(reasoning)+len(ch.Text)) + toolBytes(d.ToolCalls)
		if d.Content != "" || reasoning != "" || len(d.ToolCalls) > 0 || ch.Text != "" {
			info.hasContent = true
		}
	}
	if c.Usage != nil {
		info.hasUsage = true
		info.usage = *c.Usage
	}
	return info, true
}

// teeReader copies up to limit bytes of a non-streaming body aside.
type teeReader struct {
	src   io.ReadCloser
	buf   *bytes.Buffer
	limit int
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 && t.buf.Len() < t.limit {
		room := t.limit - t.buf.Len()
		if room > n {
			room = n
		}
		t.buf.Write(p[:room])
	}
	return n, err
}

func (t *teeReader) Close() error { return t.src.Close() }

// observeFull parses a complete non-streaming response body.
func observeFull(body []byte, obs *observation, status int) {
	var r fullResponse
	if err := json.Unmarshal(body, &r); err != nil {
		if status >= 400 {
			obs.errMsg = summarize(string(body))
		}
		return
	}
	if r.Error != nil {
		obs.errMsg = summarize(r.Error.msg)
	} else if status >= 400 {
		obs.errMsg = summarize(string(body))
	}
	for _, ch := range r.Choices {
		m := ch.Message
		obs.respBytes += int64(len(ch.Text)) + toolBytes(m.ToolCalls)
		if m.Content != nil {
			obs.respBytes += int64(len(*m.Content))
		}
		if m.ReasoningContent != nil {
			obs.respBytes += int64(len(*m.ReasoningContent))
		} else if m.Reasoning != nil {
			obs.respBytes += int64(len(*m.Reasoning))
		}
	}
	obs.usage = r.Usage
}

func summarize(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}
