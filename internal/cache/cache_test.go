package cache

import (
	"testing"

	"github.com/darshpatel1052/conduit/internal/provider"
)

func mustRequest(t *testing.T, body string) *provider.ChatRequest {
	t.Helper()
	req, err := provider.ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseChatRequest: %v", err)
	}
	return req
}

// TestCacheableGating pins the judgement calls. Each exclusion is a
// correctness decision, not a performance one.
func TestCacheableGating(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
		why  string
	}{
		{
			"deterministic request is cacheable",
			`{"model":"m","messages":[{"role":"user","content":"capital of France?"}],"temperature":0}`,
			true, "",
		},
		{
			"no temperature specified is cacheable",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			true, "",
		},
		{
			"low temperature is cacheable",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`,
			true, "",
		},
		{
			"high temperature is not",
			`{"model":"m","messages":[{"role":"user","content":"write a poem"}],"temperature":0.9}`,
			false, "the caller is paying for variety; replaying a sample removes it",
		},
		{
			"multiple samples is not",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0,"n":3}`,
			false, "n>1 explicitly asks for distinct samples",
		},
		{
			"tool calls are not",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0,"tools":[{"type":"function"}]}`,
			false, "correctness depends on state the cache cannot see",
		},
		{
			"explicit seed is not",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0,"seed":42}`,
			false, "the caller pinned determinism at the provider",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t, tc.body)
			got, reason := Cacheable(req, 0.3)
			if got != tc.want {
				t.Errorf("Cacheable = %v (%s), want %v — %s", got, reason, tc.want, tc.why)
			}
			if !got && reason == "" {
				t.Error("a rejection must explain itself; the reason is logged for tuning")
			}
		})
	}
}

func TestCacheableRespectsConfiguredMaxTemperature(t *testing.T) {
	req := mustRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	if ok, _ := Cacheable(req, 0.3); ok {
		t.Error("0.7 should exceed a 0.3 maximum")
	}
	// The dial is genuinely tunable, not hard-coded.
	if ok, _ := Cacheable(req, 0.8); !ok {
		t.Error("0.7 should be within a 0.8 maximum")
	}
}

func TestPromptKeyUsesCompleteRequest(t *testing.T) {
	base := mustRequest(t, `{"model":"m","messages":[{"role":"user","content":"What is the capital of France?"}]}`)

	same := []struct{ name, body string }{
		{"different JSON formatting", `{ "messages": [{"role":"user", "content":"What is the capital of France?"}], "model":"other" }`},
		{"streaming transport", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"What is the capital of France?"}]}`},
	}
	for _, tc := range same {
		t.Run("same/"+tc.name, func(t *testing.T) {
			if PromptKey(base) != PromptKey(mustRequest(t, tc.body)) {
				t.Error("trivially-different repeat missed the exact-match fast path")
			}
		})
	}

	different := []struct{ name, body string }{
		{"different case", `{"model":"m","messages":[{"role":"user","content":"what is the capital of france?"}]}`},
		{"different max tokens", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"What is the capital of France?"}]}`},
		{"different temperature", `{"model":"m","temperature":0.2,"messages":[{"role":"user","content":"What is the capital of France?"}]}`},
		{"different question", `{"model":"m","messages":[{"role":"user","content":"What is the capital of Germany?"}]}`},
		{"different punctuation changes intent", `{"model":"m","messages":[{"role":"user","content":"Delete the file!"}]}`},
		{"different role", `{"model":"m","messages":[{"role":"system","content":"What is the capital of France?"}]}`},
		{"extra message", `{"model":"m","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"What is the capital of France?"}]}`},
	}
	for _, tc := range different {
		t.Run("different/"+tc.name, func(t *testing.T) {
			if PromptKey(base) == PromptKey(mustRequest(t, tc.body)) {
				t.Error("distinct requests collided on the same cache key")
			}
		})
	}
}

// TestPunctuationIsSignificant guards against over-normalising. "Delete the
// file?" and "Delete the file!" are not the same request.
func TestPunctuationIsSignificant(t *testing.T) {
	a := mustRequest(t, `{"model":"m","messages":[{"role":"user","content":"Delete the file?"}]}`)
	b := mustRequest(t, `{"model":"m","messages":[{"role":"user","content":"Delete the file!"}]}`)

	if PromptKey(a) == PromptKey(b) {
		t.Error("punctuation was normalised away; a question and a command now share a cache entry")
	}
}

func TestOptionsKeyExcludesPromptButIncludesGenerationSettings(t *testing.T) {
	a := mustRequest(t, `{"model":"alias-a","messages":[{"role":"user","content":"capital of France"}],"temperature":0,"max_tokens":20}`)
	b := mustRequest(t, `{"model":"alias-b","messages":[{"role":"user","content":"capital city of France"}],"temperature":0,"max_tokens":20,"stream":true}`)
	c := mustRequest(t, `{"model":"alias-a","messages":[{"role":"user","content":"capital of France"}],"temperature":0,"max_tokens":200}`)

	if OptionsKey(a) != OptionsKey(b) {
		t.Error("prompt, model alias, and streaming transport should not change semantic-cache options")
	}
	if OptionsKey(a) == OptionsKey(c) {
		t.Error("different generation limits must not share a semantic cache entry")
	}
}
