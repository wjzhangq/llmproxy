package proxy

import (
	"encoding/json"
	"testing"
)

var chunkCorpus = []string{
	`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"id":"c","choices":[{"index":0,"delta":{"content":"héllo \"x\" \\ \né中😀"}}]}`,
	`{"choices":[{"delta":{"content":null,"reasoning_content":"think"}}]}`,
	`{"choices":[{"delta":{"reasoning":"abc","reasoning_content":"abc"}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]}}]}`,
	`{"choices":[{"index":0,"text":"completion text","logprobs":null}]}`,
	`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`,
	`{"choices":[{"delta":{"content":"a"}}],"usage":null}`,
	`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":null}}`,
	` { "choices" : [ { "delta" : { "content" : "sp" } } ] , "x" : [1,-2.5e3,true,false,null,{"y":"z"}] } `,
	`{"choices":[{"delta":{"content":"a"}},{"delta":{"content":"bc"}}]}`,
	`{"choices":null}`,
	`{"choices":[{"delta":null}]}`,
}

func checkSame(t *testing.T, data string) {
	fast, ok := scanChunk([]byte(data))
	slow, sok := slowChunk([]byte(data), &observation{})
	if !ok {
		return // fallback handles it
	}
	if !sok {
		t.Fatalf("fast parser accepted invalid JSON %q", data)
	}
	same := fast.contentBytes == slow.contentBytes && fast.hasContent == slow.hasContent &&
		fast.choices == slow.choices && fast.hasUsage == slow.hasUsage && fast.hasError == slow.hasError
	if same && fast.hasUsage {
		eq := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		same = eq(fast.usage.PromptTokens, slow.usage.PromptTokens) && eq(fast.usage.CompletionTokens, slow.usage.CompletionTokens)
	}
	if !same {
		t.Fatalf("%q\nfast %+v\nslow %+v", data, fast, slow)
	}
}

func TestScanChunkMatchesEncodingJSON(t *testing.T) {
	for _, c := range chunkCorpus {
		if _, ok := scanChunk([]byte(c)); !ok && json.Valid([]byte(c)) && c != `{"choices":null}` {
			t.Errorf("fast parser rejected %q", c)
		}
		checkSame(t, c)
	}
	for _, bad := range []string{`{"choices":[`, `{"a":"\x01"}`, `{"a":1}x`, `{"a" 1}`} {
		if _, ok := scanChunk([]byte(bad)); ok {
			t.Errorf("accepted invalid %q", bad)
		}
	}
}

func FuzzScanChunk(f *testing.F) {
	for _, c := range chunkCorpus {
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, data string) {
		checkSame(t, data)
	})
}
