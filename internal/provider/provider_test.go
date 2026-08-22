package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testClient() *http.Client {
	return NewHTTPClient(config.Upstream{
		ConnectTimeout:          config.Duration(2 * time.Second),
		ResponseHeaderTimeout:   config.Duration(5 * time.Second),
		RequestTimeout:          config.Duration(5 * time.Second),
		StreamFirstChunkTimeout: config.Duration(5 * time.Second),
		StreamIdleTimeout:       config.Duration(2 * time.Second),
		MaxIdleConns:            10,
		MaxIdleConnsPerHost:     10,
		IdleConnTimeout:         config.Duration(30 * time.Second),
	})
}

func mustRequest(t *testing.T, body string) *ChatRequest {
	t.Helper()
	req, err := ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseChatRequest: %v", err)
	}
	return req
}

// --- error classification --------------------------------------------------

// TestStatusClassification pins down the two decisions the proxy makes from an
// error. Getting these wrong is not a cosmetic bug: marking 400 retryable turns
// one malformed client request into N upstream requests, and letting it count
// as a failure lets bad client traffic trip breakers on healthy providers.
func TestStatusClassification(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		wantKind        Kind
		wantRetryable   bool
		wantCountsAsErr bool
	}{
		{"bad request", 400, KindInvalidRequest, false, false},
		{"unprocessable", 422, KindInvalidRequest, false, false},
		{"payload too large", 413, KindInvalidRequest, false, false},
		{"unauthorized", 401, KindAuth, true, true},
		{"forbidden", 403, KindAuth, true, true},
		{"model not found", 404, KindNotFound, true, true},
		{"rate limited", 429, KindRateLimit, true, true},
		{"gateway timeout", 504, KindTimeout, true, true},
		{"internal error", 500, KindUpstream, true, true},
		{"bad gateway", 502, KindUpstream, true, true},
		{"overloaded", 529, KindUpstream, true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyStatus("p", tc.status, []byte(`{"error":{"message":"boom"}}`))
			if err.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", err.Kind, tc.wantKind)
			}
			if err.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v", err.Retryable, tc.wantRetryable)
			}
			if err.CountsAsFailure != tc.wantCountsAsErr {
				t.Errorf("CountsAsFailure = %v, want %v", err.CountsAsFailure, tc.wantCountsAsErr)
			}
			if err.Message != "boom" {
				t.Errorf("Message = %q, want the upstream's message", err.Message)
			}
		})
	}
}

func TestExtractUpstreamMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai shape", `{"error":{"message":"bad key","type":"auth"}}`, "bad key"},
		{"ollama shape", `{"error":"model not found"}`, "model not found"},
		{"plain text", `something broke`, "something broke"},
		{"empty", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractUpstreamMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- request rewriting -----------------------------------------------------

// TestRequestPassthroughPreservesUnknownFields guards the design decision to
// patch the client's original JSON rather than re-marshal a struct: a parameter
// Conduit has never heard of must still reach the provider.
func TestRequestPassthroughPreservesUnknownFields(t *testing.T) {
	req := mustRequest(t, `{
		"model":"client-model",
		"messages":[{"role":"user","content":"hi"}],
		"some_future_param":{"nested":true},
		"logprobs":5
	}`)

	body, err := requestBodyFor(req, "upstream-model", true, nil)
	if err != nil {
		t.Fatalf("requestBodyFor: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if got["model"] != "upstream-model" {
		t.Errorf("model = %v, want the routed upstream name", got["model"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true", got["stream"])
	}
	if _, ok := got["some_future_param"]; !ok {
		t.Error("unknown field some_future_param was dropped; passthrough is lossy")
	}
	if got["logprobs"] != float64(5) {
		t.Errorf("logprobs = %v, want 5", got["logprobs"])
	}
}

func TestMessageTextHandlesMultimodalContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"plain string", `"hello"`, "hello"},
		{"text parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"mixed parts", `[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"x"}}]`, "describe"},
		{"empty", `""`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Role: "user", Content: json.RawMessage(tc.content)}
			if got := m.Text(); got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateRejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"no messages", `{"model":"m","messages":[]}`},
		{"empty role", `{"model":"m","messages":[{"role":"","content":"hi"}]}`},
		{"temperature too high", `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":5}`},
		{"top_p out of range", `{"model":"m","messages":[{"role":"user","content":"x"}],"top_p":2}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t, tc.body)
			if err := req.Validate(); err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}

func TestChatCompletionsURL(t *testing.T) {
	tests := []struct{ base, want string }{
		{"http://localhost:11434", "http://localhost:11434/v1/chat/completions"},
		{"http://localhost:11434/", "http://localhost:11434/v1/chat/completions"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://x.com/v1/chat/completions", "https://x.com/v1/chat/completions"},
	}
	for _, tc := range tests {
		if got := chatCompletionsURL(tc.base); got != tc.want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestResponseRequestBuildsPortableChatRequest(t *testing.T) {
	req, err := ParseResponseRequest([]byte(`{"model":"quality","instructions":"Be concise","input":"Explain failover","max_output_tokens":80}`))
	if err != nil || req.Validate() != nil {
		t.Fatalf("valid response request rejected: parse=%v validate=%v", err, req.Validate())
	}
	chat, err := req.ChatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != "quality" || len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Text() != "Explain failover" {
		t.Fatalf("unexpected chat translation: %+v", chat)
	}
	if chat.MaxCompletionTokens == nil || *chat.MaxCompletionTokens != 80 {
		t.Fatalf("max output tokens were not translated")
	}
}

func TestResponseRequestRejectsProviderOwnedState(t *testing.T) {
	for _, body := range []string{
		`{"model":"quality","input":"hi","store":true}`,
		`{"model":"quality","input":"hi","previous_response_id":"resp_old"}`,
		`{"model":"quality","input":"hi","tools":[{"type":"web_search_preview"}]}`,
	} {
		req, err := ParseResponseRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := req.Validate(); err == nil {
			t.Fatalf("request should have been rejected: %s", body)
		}
	}
}

func TestEmbeddingRequestValidation(t *testing.T) {
	valid, _ := ParseEmbeddingRequest([]byte(`{"model":"search","input":["one","two"],"encoding_format":"float"}`))
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid embeddings request rejected: %v", err)
	}
	invalid, _ := ParseEmbeddingRequest([]byte(`{"model":"search","input":"x","dimensions":0}`))
	if err := invalid.Validate(); err == nil {
		t.Fatal("zero dimensions should be rejected")
	}
}

// --- OpenAI-compatible adapter ---------------------------------------------

func TestOpenAICompleteSuccess(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer srv.Close()

	p := newOpenAI(config.ProviderConfig{
		Name: "p", Type: config.TypeOpenAI, BaseURL: srv.URL, APIKey: "sk-test",
	}, testClient(), testLogger())

	req := mustRequest(t, `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.Complete(context.Background(), req, "upstream-model")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the configured credential", gotAuth)
	}
	if gotBody["model"] != "upstream-model" {
		t.Errorf("upstream model = %v, want the routed name", gotBody["model"])
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false for a blocking completion", gotBody["stream"])
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 4 {
		t.Errorf("usage not parsed: %+v", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw is empty; the provider's own bytes should be preserved for relay")
	}
}

func TestOpenAICompleteClassifiesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()

	p := newOpenAI(config.ProviderConfig{Name: "p", Type: config.TypeOpenAI, BaseURL: srv.URL},
		testClient(), testLogger())

	_, err := p.Complete(context.Background(), mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "m")
	if err == nil {
		t.Fatal("want an error for a 429 response")
	}
	pe, ok := AsError(err)
	if !ok {
		t.Fatalf("error is %T, want *provider.Error", err)
	}
	if pe.Kind != KindRateLimit {
		t.Errorf("Kind = %q, want %q", pe.Kind, KindRateLimit)
	}
	if !pe.Retryable || !pe.CountsAsFailure {
		t.Error("a 429 should be both retryable and counted against provider health")
	}
}

func TestOpenAIStreamParsesChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		frames := []string{
			`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"1","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			`: keep-alive comment`,
			`{"id":"1","choices":[{"index":0,"delta":{"content":" world"}}]}`,
			`{"id":"1","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`,
		}
		for i, f := range frames {
			if i == 2 {
				io.WriteString(w, f+"\n\n") // raw comment line, not a data frame
			} else {
				io.WriteString(w, "data: "+f+"\n\n")
			}
			fl.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	p := newOllama(config.ProviderConfig{Name: "p", Type: config.TypeOllama, BaseURL: srv.URL},
		testClient(), testLogger())

	stream, err := p.Stream(context.Background(), mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "m")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var usage *Usage
	count := 0
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		count++
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				text.WriteString(c.Delta.Text())
			}
		}
	}

	if got := text.String(); got != "Hello world" {
		t.Errorf("assembled text = %q, want %q", got, "Hello world")
	}
	if count != 4 {
		t.Errorf("chunk count = %d, want 4 (the comment line must not become a chunk)", count)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Errorf("usage = %+v, want total 4", usage)
	}
}

// TestStreamSurfacesMidStreamError covers the case where a provider returns 200
// and then fails: the HTTP status is already committed, so the only signal is an
// error object inside the stream. Missing it makes a failed generation look like
// a successful but truncated one.
func TestStreamSurfacesMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
		fl.Flush()
		io.WriteString(w, `data: {"error":{"message":"upstream exploded","type":"server_error"}}`+"\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	p := newOpenAI(config.ProviderConfig{Name: "p", Type: config.TypeOpenAI, BaseURL: srv.URL},
		testClient(), testLogger())

	stream, err := p.Stream(context.Background(), mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "m")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	_, err = stream.Recv()
	if err == nil || err == io.EOF {
		t.Fatalf("second Recv = %v, want a classified error", err)
	}
	pe, ok := AsError(err)
	if !ok {
		t.Fatalf("error is %T, want *provider.Error", err)
	}
	if pe.Message != "upstream exploded" {
		t.Errorf("Message = %q, want the upstream's message", pe.Message)
	}
}

func TestStreamRejectsNonOKStatusBeforeStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"down for maintenance"}}`)
	}))
	defer srv.Close()

	p := newOpenAI(config.ProviderConfig{Name: "p", Type: config.TypeOpenAI, BaseURL: srv.URL},
		testClient(), testLogger())

	_, err := p.Stream(context.Background(), mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "m")
	if err == nil {
		t.Fatal("want an error before any streaming begins")
	}
	if !IsRetryable(err) {
		t.Error("a 503 before streaming must stay retryable so failover can happen")
	}
}

// --- Anthropic translation -------------------------------------------------

func TestAnthropicTranslatesRequest(t *testing.T) {
	var got anthropicRequest
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude",
			"content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":7,"output_tokens":2}}`)
	}))
	defer srv.Close()

	p := newAnthropic(config.ProviderConfig{
		Name: "a", Type: config.TypeAnthropic, BaseURL: srv.URL, APIKey: "sk-ant",
	}, testClient(), testLogger())

	req := mustRequest(t, `{
		"model":"m",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hello"},
			{"role":"user","content":"again"}
		],
		"temperature":1.8
	}`)

	resp, err := p.Complete(context.Background(), req, "claude-sonnet")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotHeaders.Get("x-api-key") != "sk-ant" {
		t.Errorf("x-api-key = %q, want the credential", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotHeaders.Get("anthropic-version"), anthropicVersion)
	}
	// The system message must be hoisted out of the conversation.
	if got.System != "be brief" {
		t.Errorf("system = %q, want the hoisted system message", got.System)
	}
	// Consecutive same-role turns must be merged; Anthropic rejects them.
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 merged user turn: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Content != "hello\n\nagain" {
		t.Errorf("merged content = %q", got.Messages[0].Content)
	}
	// max_tokens is mandatory for Anthropic and optional for OpenAI.
	if got.MaxTokens != defaultAnthropicMaxTokens {
		t.Errorf("max_tokens = %d, want the default %d", got.MaxTokens, defaultAnthropicMaxTokens)
	}
	// OpenAI allows temperature up to 2, Anthropic caps at 1.
	if got.Temperature == nil || *got.Temperature != 1.0 {
		t.Errorf("temperature = %v, want it clamped to 1.0", got.Temperature)
	}

	// And the response must come back in OpenAI shape.
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Text() != "hi there" {
		t.Fatalf("response not translated to OpenAI shape: %+v", resp.Choices)
	}
	if *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop (mapped from end_turn)", *resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want input 7 / output 2", resp.Usage)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":5}}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			io.WriteString(w, e+"\n\n")
			fl.Flush()
		}
	}))
	defer srv.Close()

	p := newAnthropic(config.ProviderConfig{Name: "a", Type: config.TypeAnthropic, BaseURL: srv.URL},
		testClient(), testLogger())

	stream, err := p.Stream(context.Background(), mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "claude")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var finish string
	var usage *Usage
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				text.WriteString(c.Delta.Text())
			}
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if got := text.String(); got != "Hello" {
		t.Errorf("assembled text = %q, want %q", got, "Hello")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want input 5 / output 2 accumulated across events", usage)
	}
}

func TestMapStopReason(t *testing.T) {
	tests := []struct{ in, want string }{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := mapStopReason(tc.in); got != tc.want {
			t.Errorf("mapStopReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- SSE decoding ----------------------------------------------------------

func TestSSEDecoderHandlesFraming(t *testing.T) {
	input := strings.Join([]string{
		": a comment",
		"",
		"data: {\"a\":1}",
		"",
		"event: custom",
		"data: line1",
		"data: line2",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	dec := newSSEDecoder(strings.NewReader(input))

	ev, err := dec.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if string(ev.Data) != `{"a":1}` {
		t.Errorf("first data = %q", ev.Data)
	}

	ev, err = dec.Next()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if ev.Event != "custom" {
		t.Errorf("event name = %q, want custom", ev.Event)
	}
	// Multi-line data fields join with a newline, per the SSE spec.
	if string(ev.Data) != "line1\nline2" {
		t.Errorf("multi-line data = %q, want %q", ev.Data, "line1\nline2")
	}

	ev, err = dec.Next()
	if err != nil {
		t.Fatalf("third event: %v", err)
	}
	if string(ev.Data) != "[DONE]" {
		t.Errorf("terminator = %q", ev.Data)
	}
}

// TestSSEDecoderHandlesLargeFrames guards the choice of bufio.Reader over
// bufio.Scanner, which caps tokens at 64 KiB and would fail on a chunk carrying
// a long tool-call argument or an inlined image.
func TestSSEDecoderHandlesLargeFrames(t *testing.T) {
	big := strings.Repeat("x", 256<<10) // 256 KiB, well past Scanner's limit
	input := "data: " + big + "\n\n"

	dec := newSSEDecoder(strings.NewReader(input))
	ev, err := dec.Next()
	if err != nil {
		t.Fatalf("large frame: %v", err)
	}
	if len(ev.Data) != len(big) {
		t.Errorf("data length = %d, want %d", len(ev.Data), len(big))
	}
}

func TestSSEDecoderHandlesCRLF(t *testing.T) {
	dec := newSSEDecoder(strings.NewReader("data: {\"a\":1}\r\n\r\n"))
	ev, err := dec.Next()
	if err != nil {
		t.Fatalf("crlf frame: %v", err)
	}
	if string(ev.Data) != `{"a":1}` {
		t.Errorf("data = %q, want the CR stripped", ev.Data)
	}
}

// --- cancellation ----------------------------------------------------------

// TestStreamStopsOnContextCancel is the behaviour that stops the gateway paying
// a provider to generate tokens for a client that has already left.
func TestStreamStopsOnContextCancel(t *testing.T) {
	released := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
		fl.Flush()
		// Block until the client goes away.
		<-r.Context().Done()
		close(released)
	}))
	defer srv.Close()

	p := newOpenAI(config.ProviderConfig{Name: "p", Type: config.TypeOpenAI, BaseURL: srv.URL},
		testClient(), testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Stream(ctx, mustRequest(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "m")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}

	cancel() // the client disconnects

	select {
	case <-released:
		// The server observed the cancellation: the upstream request was torn
		// down rather than left running.
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled when the client context was cancelled")
	}
	stream.Close()
}
