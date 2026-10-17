package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamingUsageAvailabilityAndFailure(t *testing.T) {
	const answer = `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`
	const partial = `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`
	const usage = `{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"prompt_tokens_details":{"cached_tokens":67}}}`
	const zeroUsage = `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`
	for _, tc := range []struct {
		name     string
		chunks   []string
		reported bool
		want     Usage
		wantErr  bool
	}{
		{name: "reported", chunks: []string{answer, usage}, reported: true, want: Usage{123, 67, 45}},
		{name: "missing", chunks: []string{answer}},
		{name: "null", chunks: []string{answer, `{"choices":[],"usage":null}`}},
		{name: "explicit zero", chunks: []string{answer, zeroUsage}, reported: true},
		{name: "cutoff with usage", chunks: []string{partial, usage}, reported: true, want: Usage{123, 67, 45}, wantErr: true},
		{name: "cutoff without usage", chunks: []string{partial}, wantErr: true},
		{name: "usage without response", chunks: []string{usage}, reported: true, want: Usage{123, 67, 45}, wantErr: true},
		{name: "stream error after usage", chunks: []string{partial, usage, `{"error":{"message":"private provider diagnostic","type":"server_error"}}`}, reported: true, want: Usage{123, 67, 45}, wantErr: true},
		{name: "invalid tool call", chunks: []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, usage}, reported: true, want: Usage{123, 67, 45}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, chunk := range tc.chunks {
					fmt.Fprintf(w, "data: %s\n\n", chunk)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			resp, err := NewOpenAI(server.URL+"/v1/", "").Chat(context.Background(), profileRequest("", "test-model"), nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
			}
			if resp.UsageReported != tc.reported || resp.Usage != tc.want {
				t.Fatalf("usage=%+v reported=%v, want %+v reported=%v", resp.Usage, resp.UsageReported, tc.want, tc.reported)
			}
			if tc.wantErr && (resp.Message.Content != "" || len(resp.Message.ToolCalls) != 0) {
				t.Fatalf("failed response exposed partial model output: %+v", resp)
			}
		})
	}
}
