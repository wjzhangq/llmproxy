package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// member is one top-level key of a JSON object, in original order.
type member struct {
	key string
	raw json.RawMessage
}

// parseObject decodes a JSON object while keeping key order.
func parseObject(b []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("request body is not a JSON object")
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{key, raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return out, nil
}

func encodeObject(ms []member) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

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

// prepareRequest parses the client body once, extracts metrics, rewrites
// "model" and (optionally) injects stream_options.include_usage. If the body
// cannot be parsed it is forwarded unchanged.
func prepareRequest(body []byte, path, upstreamModel string, injectUsage bool) reqInfo {
	info := reqInfo{forwardedBody: body}
	ms, err := parseObject(body)
	if err != nil {
		return info
	}
	info.parsed = true
	modelIdx, soIdx := -1, -1
	for i, m := range ms {
		switch m.key {
		case "model":
			modelIdx = i
			_ = json.Unmarshal(m.raw, &info.clientModel)
		case "stream":
			_ = json.Unmarshal(m.raw, &info.stream)
		case "stream_options":
			soIdx = i
		case "messages":
			info.contentBytes, info.imageCount = countMessages(m.raw)
		case "prompt":
			info.contentBytes = countPrompt(m.raw)
		}
	}
	modelRaw, _ := json.Marshal(upstreamModel)
	if modelIdx >= 0 {
		ms[modelIdx].raw = modelRaw
	} else {
		ms = append([]member{{"model", modelRaw}}, ms...)
		if soIdx >= 0 {
			soIdx++
		}
	}
	if injectUsage && info.stream {
		if soIdx < 0 {
			ms = append(ms, member{"stream_options", json.RawMessage(`{"include_usage":true}`)})
			info.injected = true
		} else if so, err := parseObject(ms[soIdx].raw); err == nil {
			found := false
			for i := range so {
				if so[i].key == "include_usage" {
					found = true
					var v bool
					if json.Unmarshal(so[i].raw, &v) == nil && !v {
						so[i].raw = json.RawMessage("true")
						info.injected = true
					}
				}
			}
			if !found {
				so = append(so, member{"include_usage", json.RawMessage("true")})
				info.injected = true
			}
			ms[soIdx].raw = encodeObject(so)
		} else if string(bytes.TrimSpace(ms[soIdx].raw)) == "null" {
			ms[soIdx].raw = json.RawMessage(`{"include_usage":true}`)
			info.injected = true
		}
	}
	info.forwardedBody = encodeObject(ms)
	return info
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// countMessages returns the UTF-8 byte count of text content and the number
// of images in a chat "messages" array.
func countMessages(raw json.RawMessage) (int64, int) {
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return 0, 0
	}
	var n int64
	images := 0
	for _, m := range msgs {
		if len(m.Content) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			n += int64(len(s))
			continue
		}
		var parts []contentPart
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				n += int64(len(p.Text))
			case "image_url", "image", "input_image":
				images++
			}
		}
	}
	return n, images
}

// countPrompt counts the bytes of a completions "prompt" (string or array of
// strings).
func countPrompt(raw json.RawMessage) int64 {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return int64(len(s))
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		var n int64
		for _, v := range arr {
			n += int64(len(v))
		}
		return n
	}
	return 0
}
