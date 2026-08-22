package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
)

// anthropicVersion is the API version header Anthropic requires on every call.
// It is pinned rather than tracking "latest" so a server-side release cannot
// change response shapes underneath a running gateway.
const anthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens is used when the client omits max_tokens.
// Anthropic requires the field; OpenAI treats it as optional. Rejecting the
// request would make the gateway a leaky abstraction, so a default is supplied.
const defaultAnthropicMaxTokens = 4096

// anthropicProvider translates between OpenAI's chat-completions schema and
// Anthropic's Messages API.
//
// This adapter is where the "provider-agnostic core" claim gets tested. The
// differences are real: system prompts are a top-level field rather than a
// message, max_tokens is mandatory, and the token stream is a typed event
// sequence instead of uniform delta frames. Confining all of that here is what
// lets the router, breakers and proxy stay ignorant of vendors.
type anthropicProvider struct {
	name     string
	endpoint string
	apiKey   string
	headers  map[string]string
	timeout  time.Duration
	client   *http.Client
	log      *slog.Logger
}

func newAnthropic(cfg config.ProviderConfig, client *http.Client, log *slog.Logger) Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	endpoint := base + "/v1/messages"
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/messages"
	}
	return &anthropicProvider{
		name:     cfg.Name,
		endpoint: endpoint,
		apiKey:   cfg.Credential(),
		headers:  cfg.Headers,
		timeout:  cfg.Timeout.D(),
		client:   client,
		log:      log.With("provider", cfg.Name, "provider_type", config.TypeAnthropic),
	}
}

func (p *anthropicProvider) Name() string { return p.name }
func (p *anthropicProvider) Type() string { return config.TypeAnthropic }

// --- request translation ---------------------------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []anthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (p *anthropicProvider) translateRequest(in *ChatRequest, model string, stream bool) (*anthropicRequest, error) {
	out := &anthropicRequest{
		Model:       model,
		Stream:      stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}

	switch {
	case in.MaxCompletionTokens != nil:
		out.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		out.MaxTokens = *in.MaxTokens
	default:
		out.MaxTokens = defaultAnthropicMaxTokens
	}

	// OpenAI allows temperature up to 2; Anthropic caps at 1. Clamping keeps a
	// portable request from being rejected by only some of the pool.
	if out.Temperature != nil && *out.Temperature > 1 {
		clamped := 1.0
		out.Temperature = &clamped
	}

	if len(in.Stop) > 0 {
		var one string
		if err := json.Unmarshal(in.Stop, &one); err == nil {
			out.StopSequences = []string{one}
		} else {
			var many []string
			if err := json.Unmarshal(in.Stop, &many); err == nil {
				out.StopSequences = many
			}
		}
	}

	var systemParts []string
	for _, m := range in.Messages {
		text := m.Text()
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			// Hoisted: Anthropic carries the system prompt outside the
			// conversation, not as the first turn.
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			out.Messages = appendAnthropicMessage(out.Messages, "assistant", text)
		case "tool", "function":
			// Without full tool translation, tool output is surfaced as user
			// context. That keeps the conversation coherent instead of
			// dropping a turn silently.
			out.Messages = appendAnthropicMessage(out.Messages, "user", text)
		default:
			out.Messages = appendAnthropicMessage(out.Messages, "user", text)
		}
	}
	out.System = strings.Join(systemParts, "\n\n")

	if len(out.Messages) == 0 {
		return nil, &Error{
			Provider: p.name, Kind: KindInvalidRequest,
			Message: "request contains no user or assistant messages to send to Anthropic",
		}
	}
	// Anthropic requires the conversation to open with a user turn.
	if out.Messages[0].Role != "user" {
		out.Messages = append([]anthropicMessage{{Role: "user", Content: "(continue)"}}, out.Messages...)
	}
	return out, nil
}

// appendAnthropicMessage merges consecutive same-role turns, which Anthropic
// rejects but OpenAI-shaped clients emit freely.
func appendAnthropicMessage(msgs []anthropicMessage, role, text string) []anthropicMessage {
	if text == "" {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content += "\n\n" + text
		return msgs
	}
	return append(msgs, anthropicMessage{Role: role, Content: text})
}

func (p *anthropicProvider) newRequest(ctx context.Context, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if p.apiKey != "" {
		req.Header.Set("x-api-key", p.apiKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// --- response translation --------------------------------------------------

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// mapStopReason converts Anthropic stop reasons to OpenAI finish reasons, so
// clients can branch on finish_reason without knowing who served them.
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return reason
	}
}

func (p *anthropicProvider) Complete(ctx context.Context, in *ChatRequest, model string) (*ChatResponse, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	translated, err := p.translateRequest(in, model, false)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(translated)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}

	req, err := p.newRequest(ctx, body, false)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, classifyTransport(p.name, ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, classifyTransport(p.name, ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus(p.name, resp.StatusCode, raw)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, &Error{
			Provider: p.name, Kind: KindUpstream,
			Message:         "upstream returned a body that is not an Anthropic message: " + truncate(raw, 256),
			Retryable:       true,
			CountsAsFailure: true,
			Err:             err,
		}
	}

	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}

	finish := mapStopReason(ar.StopReason)
	content, _ := json.Marshal(text.String())

	out := &ChatResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: content},
			FinishReason: &finish,
		}},
		Usage: &Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
	// Re-serialise into OpenAI shape: unlike the passthrough adapter there are
	// no original bytes worth preserving, since the client expects OpenAI JSON.
	if b, err := json.Marshal(out); err == nil {
		out.Raw = b
	}
	return out, nil
}

func (p *anthropicProvider) Stream(ctx context.Context, in *ChatRequest, model string) (Stream, error) {
	translated, err := p.translateRequest(in, model, true)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(translated)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}

	req, err := p.newRequest(ctx, body, true)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, classifyTransport(p.name, ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, classifyStatus(p.name, resp.StatusCode, raw)
	}

	return &anthropicStream{
		provider: p.name,
		resp:     resp,
		dec:      newSSEDecoder(resp.Body),
		ctx:      ctx,
		model:    model,
		created:  time.Now().Unix(),
	}, nil
}

func (p *anthropicProvider) Embed(context.Context, *EmbeddingRequest, string) (*EmbeddingResponse, error) {
	return nil, &Error{Provider: p.name, Kind: KindInvalidRequest, Message: "Anthropic connections do not provide an embeddings API", Retryable: false, CountsAsFailure: false}
}

// anthropicStream converts Anthropic's typed event stream into OpenAI-shaped
// delta chunks.
type anthropicStream struct {
	provider string
	resp     *http.Response
	dec      *sseDecoder
	ctx      context.Context

	id      string
	model   string
	created int64

	promptTokens     int
	completionTokens int

	closeOnce sync.Once
	done      bool
}

func (s *anthropicStream) Recv() (*ChatChunk, error) {
	if s.done {
		return nil, io.EOF
	}

	for {
		ev, err := s.dec.Next()
		if err != nil {
			s.done = true
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, classifyTransport(s.provider, s.ctx, err)
		}

		data := bytes.TrimSpace(ev.Data)
		if len(data) == 0 {
			continue
		}

		var envelope struct {
			Type    string `json:"type"`
			Message *struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta *struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}

		switch envelope.Type {
		case "message_start":
			if envelope.Message != nil {
				s.id = envelope.Message.ID
				if envelope.Message.Model != "" {
					s.model = envelope.Message.Model
				}
				s.promptTokens = envelope.Message.Usage.InputTokens
			}
			// OpenAI streams open with a role-only delta; emitting it keeps
			// clients that assemble messages from deltas happy.
			return s.chunk(&Message{Role: "assistant"}, nil, nil), nil

		case "content_block_delta":
			if envelope.Delta == nil || envelope.Delta.Text == "" {
				continue
			}
			content, _ := json.Marshal(envelope.Delta.Text)
			return s.chunk(&Message{Content: content}, nil, nil), nil

		case "message_delta":
			if envelope.Usage != nil {
				s.completionTokens = envelope.Usage.OutputTokens
			}
			if envelope.Delta != nil && envelope.Delta.StopReason != "" {
				finish := mapStopReason(envelope.Delta.StopReason)
				usage := &Usage{
					PromptTokens:     s.promptTokens,
					CompletionTokens: s.completionTokens,
					TotalTokens:      s.promptTokens + s.completionTokens,
				}
				return s.chunk(&Message{}, &finish, usage), nil
			}

		case "message_stop":
			s.done = true
			return nil, io.EOF

		case "error":
			s.done = true
			if perr := errorPayload(s.provider, data); perr != nil {
				return nil, perr
			}
			return nil, &Error{
				Provider: s.provider, Kind: KindUpstream,
				Message:         fmt.Sprintf("anthropic stream error: %s", truncate(data, 256)),
				Retryable:       true,
				CountsAsFailure: true,
			}

		default:
			// ping, content_block_start, content_block_stop: no client-visible
			// payload.
			continue
		}
	}
}

func (s *anthropicStream) chunk(delta *Message, finish *string, usage *Usage) *ChatChunk {
	c := &ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}},
		Usage:   usage,
	}
	if b, err := json.Marshal(c); err == nil {
		c.Raw = b
	}
	return c
}

func (s *anthropicStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.resp != nil && s.resp.Body != nil {
			err = s.resp.Body.Close()
		}
	})
	return err
}
