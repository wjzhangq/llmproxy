package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareRequestSplice(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{}`, `{"model":"B"}`},
		{`{"stream":true}`, `{"model":"B","stream":true,"stream_options":{"include_usage":true}}`},
		{"{ \"model\" : \"x\",\n  \"stream\": true ,\"stream_options\":null }", "{ \"model\" : \"B\",\n  \"stream\": true ,\"stream_options\":{\"include_usage\":true} }"},
		{`{"stream":true,"stream_options":{}}`, `{"model":"B","stream":true,"stream_options":{"include_usage":true}}`},
		{`{"stream":true,"stream_options":{"include_usage":false,"a":1}}`, `{"model":"B","stream":true,"stream_options":{"include_usage":true,"a":1}}`},
		{`{"stream":true,"stream_options":{"include_usage":true}}`, `{"model":"B","stream":true,"stream_options":{"include_usage":true}}`},
		{`{"stream":false,"model":null}`, `{"stream":false,"model":"B"}`},
		{`{"model":"a\"b","messages":[]}`, `{"model":"B","messages":[]}`},
	}
	for _, c := range cases {
		got := prepareRequest([]byte(c.in), "B", true)
		if string(got.forwardedBody) != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.in, got.forwardedBody, c.want)
		}
	}
	info := prepareRequest([]byte(`{"model":"a\"b","stream":true,"stream_options":{"include_usage":true}}`), "B", true)
	if info.clientModel != `a"b` || info.injected {
		t.Fatalf("%+v", info)
	}
	info = prepareRequest([]byte(`{"messages":[{"role":"user","content":"hé"},{"content":[{"text":"abc","type":"text"},{"type":"image_url","image_url":{"url":"x"}},{"type":"input_image"}]},{"content":null}]}`), "B", true)
	if info.contentBytes != 6 || info.imageCount != 2 {
		t.Fatalf("%+v", info)
	}
	for _, bad := range []string{`[]`, `{"a":1`, `{"a":1}x`, `"s"`, ``} {
		if info := prepareRequest([]byte(bad), "B", true); info.parsed || string(info.forwardedBody) != bad {
			t.Errorf("invalid body %q must be forwarded unchanged", bad)
		}
	}
}

// expected computes the intended rewrite with encoding/json semantics.
func expectedRewrite(in map[string]any, inject bool) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	out["model"] = "B"
	if inject && in["stream"] == true {
		switch so := in["stream_options"].(type) {
		case nil:
			if _, present := in["stream_options"]; !present || so == nil {
				out["stream_options"] = map[string]any{"include_usage": true}
			}
		case map[string]any:
			m := map[string]any{}
			for k, v := range so {
				m[k] = v
			}
			if v, ok := so["include_usage"].(bool); !ok || !v {
				if _, exists := so["include_usage"]; !exists || v == false {
					m["include_usage"] = true
				}
			}
			out["stream_options"] = m
		}
	}
	return out
}

func FuzzPrepareRequest(f *testing.F) {
	for _, s := range []string{
		`{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"stream":true,"stream_options":{"include_usage":false}}`,
		`{"prompt":["a","b"],"n":2}`,
		`{"stream":true,"stream_options":null,"model":1}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		info := prepareRequest([]byte(s), "B", true)
		var in map[string]any
		if json.Unmarshal([]byte(s), &in) != nil || in == nil || hasDupOrFoldKeys(s) {
			return
		}
		if !info.parsed {
			t.Fatalf("valid object not parsed: %q", s)
		}
		var got map[string]any
		if err := json.Unmarshal(info.forwardedBody, &got); err != nil {
			t.Fatalf("invalid output %q -> %q: %v", s, info.forwardedBody, err)
		}
		if want := expectedRewrite(in, true); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q\n got %v\nwant %v", s, got, want)
		}
	})
}

// hasDupOrFoldKeys reports top-level or stream_options keys that encoding/json
// would merge (duplicates, case variants, escaped spellings).
func hasDupOrFoldKeys(s string) bool {
	check := func(raw []byte) bool {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		dec := json.NewDecoder(bytesReader(raw))
		dec.Token()
		n := 0
		for dec.More() {
			tok, _ := dec.Token()
			var v json.RawMessage
			dec.Decode(&v)
			n++
			if k, ok := tok.(string); ok {
				for _, want := range []string{"model", "stream", "stream_options", "include_usage"} {
					if k != want && equalFold(k, want) {
						return true
					}
				}
			}
		}
		return n != len(m)
	}
	var top map[string]json.RawMessage
	json.Unmarshal([]byte(s), &top)
	if check([]byte(s)) {
		return true
	}
	if so, ok := top["stream_options"]; ok && len(so) > 0 && so[0] == '{' {
		return check(so)
	}
	return false
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
func equalFold(a, b string) bool         { return strings.EqualFold(a, b) }
