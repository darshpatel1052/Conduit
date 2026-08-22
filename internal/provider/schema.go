package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The wire types below mirror OpenAI's chat-completions schema.
//
// Conduit speaks that schema at its edge on purpose: it is the de-facto
// standard, so every OpenAI client library (and LangChain, and llama-index,
// and curl examples from the docs) can point at the gateway by changing a base
// URL and nothing else. Adoption cost is the whole game for a proxy.

// ChatRequest is an inbound /v1/chat/completions request.
//
// Only the fields the gateway actually reasons about are modelled. Everything
// else survives via Raw: for OpenAI-compatible upstreams the original bytes are
// forwarded with just the routed fields patched, so a parameter this build has
// never heard of still reaches the provider intact. Parsing a field here is a
// decision to *use* it, not a requirement for passing it through.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`

	// Temperature is a pointer because 0 is a meaningful value and must be
	// distinguishable from "not set" — semantic caching in a later phase keys
	// off exactly that difference.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           *int     `json:"n,omitempty"`

	MaxTokens           *int `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`

	Stop          json.RawMessage `json:"stop,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
	User          string          `json:"user,omitempty"`

	// Tools and Seed are parsed because the semantic cache has to reason about
	// them: a tool call's correct response depends on state the cache cannot
	// see, and a caller supplying a seed is pinning determinism at the provider
	// and expects its behaviour rather than ours.
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Seed       *int            `json:"seed,omitempty"`

	// Raw holds the exact bytes the client sent. Never serialised back out as
	// a field; see requestBodyFor.
	Raw json.RawMessage `json:"-"`
}

// StreamOptions carries OpenAI's stream_options object.
type StreamOptions struct {
	// IncludeUsage asks the provider to append a final chunk carrying token
	// counts. Conduit wants this for cost accounting, so it is requested even
	// when the client did not ask, and stripped again if the client did not.
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Message is one chat message.
type Message struct {
	Role string `json:"role"`

	// Content is raw because OpenAI allows both a plain string and an array of
	// typed parts (text, image_url). Decoding into a concrete type would lose
	// multimodal requests; Text() handles the common cases.
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Text flattens a message's content to plain text, joining the text parts of a
// multimodal message and ignoring non-text parts.
func (m Message) Text() string {
	if len(m.Content) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ChatResponse is a non-streaming completion.
type ChatResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	Choices           []Choice        `json:"choices"`
	Usage             *Usage          `json:"usage,omitempty"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	Raw               json.RawMessage `json:"-"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

// Usage carries token counts, the input to cost accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChunk is one streamed delta.
type ChatChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`

	// Raw is the chunk exactly as the provider sent it. Relaying these bytes
	// rather than a re-marshalled struct keeps fields Conduit does not model
	// (logprobs, tool-call deltas, provider extensions) intact for the client.
	Raw json.RawMessage `json:"-"`
}

// ParseChatRequest decodes an inbound request and keeps the original bytes.
func ParseChatRequest(body []byte) (*ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	req.Raw = append(json.RawMessage(nil), body...)
	return &req, nil
}

// Validate checks the request is well-formed before any upstream is contacted.
// Rejecting here saves a round trip and, more importantly, keeps a malformed
// request from burning a failover attempt against every provider in the pool.
func (r *ChatRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("field \"model\" is required")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("field \"messages\" must contain at least one message")
	}
	for i, m := range r.Messages {
		if strings.TrimSpace(m.Role) == "" {
			return fmt.Errorf("messages[%d].role is required", i)
		}
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("field \"temperature\" must be between 0 and 2")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return fmt.Errorf("field \"top_p\" must be between 0 and 1")
	}
	if r.N != nil && *r.N < 1 {
		return fmt.Errorf("field \"n\" must be at least 1")
	}
	return nil
}

// PromptText flattens the conversation, for logging and later for cache keys.
func (r *ChatRequest) PromptText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text())
	}
	return b.String()
}

// EmbeddingRequest is the OpenAI-compatible /v1/embeddings request shape.
// Input stays raw so both strings and token arrays can pass through unchanged.
type EmbeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	User           string          `json:"user,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

type Embedding struct {
	Object    string          `json:"object"`
	Embedding json.RawMessage `json:"embedding"`
	Index     int             `json:"index"`
}

type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []Embedding     `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

func ParseEmbeddingRequest(body []byte) (*EmbeddingRequest, error) {
	var req EmbeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	req.Raw = append(json.RawMessage(nil), body...)
	return &req, nil
}

func (r *EmbeddingRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("field \"model\" is required")
	}
	if len(r.Input) == 0 || string(r.Input) == "null" {
		return fmt.Errorf("field \"input\" is required")
	}
	if r.EncodingFormat != "" && r.EncodingFormat != "float" && r.EncodingFormat != "base64" {
		return fmt.Errorf("field \"encoding_format\" must be \"float\" or \"base64\"")
	}
	if r.Dimensions != nil && *r.Dimensions < 1 {
		return fmt.Errorf("field \"dimensions\" must be at least 1")
	}
	return nil
}

func embeddingBodyFor(req *EmbeddingRequest, model string) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(req.Raw, &obj); err != nil {
		return nil, fmt.Errorf("re-encode request: %w", err)
	}
	b, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = b
	return json.Marshal(obj)
}

// ResponseRequest is Conduit's portable subset of OpenAI's Responses API.
// It intentionally excludes provider-owned state and provider-hosted tools.
type ResponseRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Store           *bool           `json:"store,omitempty"`
	PreviousID      string          `json:"previous_response_id,omitempty"`
	Conversation    json.RawMessage `json:"conversation,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
}

func ParseResponseRequest(body []byte) (*ResponseRequest, error) {
	var req ResponseRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "max_output_tokens": true,
		"temperature": true, "top_p": true, "stream": true, "store": true,
		"previous_response_id": true, "conversation": true, "tools": true,
	}
	for field := range fields {
		if !allowed[field] {
			return nil, fmt.Errorf("field %q is not supported by Conduit's portable Responses API", field)
		}
	}
	return &req, nil
}

func (r *ResponseRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("field \"model\" is required")
	}
	if len(r.Input) == 0 || string(r.Input) == "null" {
		return fmt.Errorf("field \"input\" is required")
	}
	if r.Store != nil && *r.Store {
		return fmt.Errorf("field \"store\" is not supported; Conduit does not persist response state")
	}
	if r.PreviousID != "" || len(r.Conversation) > 0 {
		return fmt.Errorf("provider-owned conversations are not supported; send the conversation in input")
	}
	if len(r.Tools) > 0 && string(r.Tools) != "null" && string(r.Tools) != "[]" {
		return fmt.Errorf("Responses tools are not supported in this release")
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens < 1 {
		return fmt.Errorf("field \"max_output_tokens\" must be at least 1")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("field \"temperature\" must be between 0 and 2")
	}
	return nil
}

func (r *ResponseRequest) ChatRequest() (*ChatRequest, error) {
	messages := make([]Message, 0, 4)
	if strings.TrimSpace(r.Instructions) != "" {
		content, _ := json.Marshal(r.Instructions)
		messages = append(messages, Message{Role: "system", Content: content})
	}
	var text string
	if err := json.Unmarshal(r.Input, &text); err == nil {
		content, _ := json.Marshal(text)
		messages = append(messages, Message{Role: "user", Content: content})
	} else {
		var inputMessages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(r.Input, &inputMessages); err != nil || len(inputMessages) == 0 {
			return nil, fmt.Errorf("field \"input\" must be a string or an array of message objects")
		}
		for i, item := range inputMessages {
			if item.Role == "" || len(item.Content) == 0 {
				return nil, fmt.Errorf("input[%d] requires role and content", i)
			}
			messages = append(messages, Message{Role: item.Role, Content: item.Content})
		}
	}
	return &ChatRequest{Model: r.Model, Messages: messages, Stream: r.Stream,
		Temperature: r.Temperature, TopP: r.TopP, MaxCompletionTokens: r.MaxOutputTokens}, nil
}

// requestBodyFor rebuilds the upstream body from the client's original bytes,
// overriding only what routing decided: the upstream model name and the stream
// flag. Patching a generic JSON object instead of re-marshalling ChatRequest is
// what makes the passthrough lossless.
func requestBodyFor(req *ChatRequest, model string, stream bool, extra map[string]any) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if len(req.Raw) > 0 {
		if err := json.Unmarshal(req.Raw, &obj); err != nil {
			return nil, fmt.Errorf("re-encode request: %w", err)
		}
	} else {
		// Constructed in-process (tests, replays) rather than parsed off the
		// wire, so there are no original bytes to preserve.
		b, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return nil, err
		}
	}

	set := func(key string, val any) error {
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		obj[key] = b
		return nil
	}

	if err := set("model", model); err != nil {
		return nil, err
	}
	if err := set("stream", stream); err != nil {
		return nil, err
	}
	if !stream {
		delete(obj, "stream_options")
	}
	for k, v := range extra {
		if v == nil {
			delete(obj, k)
			continue
		}
		if err := set(k, v); err != nil {
			return nil, err
		}
	}
	return json.Marshal(obj)
}
