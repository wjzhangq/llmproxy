package proxy

import (
	"io"
	"strings"
	"testing"
)

// A typical vLLM chat streaming chunk.
const benchChunk = `data: {"id":"chatcmpl-9f8e7d6c5b4a","object":"chat.completion.chunk","created":1731470000,"model":"qwen3-32b","choices":[{"index":0,"delta":{"content":" token"},"logprobs":null,"finish_reason":null}]}` + "\n\n"

func BenchmarkSSEObserve(b *testing.B) {
	stream := strings.Repeat(benchChunk, 1000)
	buf := make([]byte, 32<<10)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := newSSEReader(io.NopCloser(strings.NewReader(stream)), &observation{}, true)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
	}
}

func benchBody(imageBytes, textBytes int) []byte {
	img := strings.Repeat("A", imageBytes)
	txt := strings.Repeat("长文本 text ", textBytes/15)
	return []byte(`{"model":"gpt-4o","stream":true,"temperature":0.7,"messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":[{"type":"text","text":"` + txt + `"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + img + `"}}]},` +
		`{"role":"assistant","content":"ok"},{"role":"user","content":"` + txt + `"}]}`)
}

func BenchmarkPrepareRequestImage(b *testing.B) {
	body := benchBody(1<<20, 4<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		prepareRequest(body, "B", true)
	}
}

func BenchmarkPrepareRequestText(b *testing.B) {
	body := benchBody(0, 100<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		prepareRequest(body, "B", true)
	}
}
