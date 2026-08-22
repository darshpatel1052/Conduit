package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
)

// openAICompatible adapts any endpoint that already speaks OpenAI's
// chat-completions API.
//
// Ollama and OpenAI share this one implementation, which is the payoff for
// Ollama exposing an OpenAI-compatible surface: the difference between "free
// model on my laptop" and "paid hosted API" collapses to a base URL and a
// credential. Anthropic gets its own adapter because its API is genuinely
// different in shape, and pretending otherwise would mean translating in the
// wrong place.
type openAICompatible struct {
	name               string
	typ                string
	endpoint           string
	embeddingsEndpoint string
	apiKey             string
	headers            map[string]string
	timeout            time.Duration
	client             *http.Client
	log                *slog.Logger
}

func newOpenAI(cfg config.ProviderConfig, client *http.Client, log *slog.Logger) Provider {
	return &openAICompatible{
		name:               cfg.Name,
		typ:                config.TypeOpenAI,
		endpoint:           chatCompletionsURL(cfg.BaseURL),
		embeddingsEndpoint: embeddingsURL(cfg.BaseURL),
		apiKey:             cfg.Credential(),
		headers:            cfg.Headers,
		timeout:            cfg.Timeout.D(),
		client:             client,
		log:                log.With("provider", cfg.Name, "provider_type", config.TypeOpenAI),
	}
}

func newOllama(cfg config.ProviderConfig, client *http.Client, log *slog.Logger) Provider {
	return &openAICompatible{
		name:               cfg.Name,
		typ:                config.TypeOllama,
		endpoint:           chatCompletionsURL(cfg.BaseURL),
		embeddingsEndpoint: embeddingsURL(cfg.BaseURL),
		// Ollama ignores auth entirely when served locally, but honour a key
		// so the same adapter works behind an authenticating reverse proxy.
		apiKey:  cfg.Credential(),
		headers: cfg.Headers,
		timeout: cfg.Timeout.D(),
		client:  client,
		log:     log.With("provider", cfg.Name, "provider_type", config.TypeOllama),
	}
}

func embeddingsURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, "/embeddings"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/embeddings"
	default:
		return base + "/v1/embeddings"
	}
}

// chatCompletionsURL tolerates a base URL given with or without the /v1
// prefix, because both spellings appear in every provider's own docs.
func chatCompletionsURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

func (p *openAICompatible) Name() string { return p.name }
func (p *openAICompatible) Type() string { return p.typ }

func (p *openAICompatible) newRequest(ctx context.Context, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		// Proxies between here and the provider will happily buffer a response
		// into oblivion; this asks them not to.
		req.Header.Set("Cache-Control", "no-cache")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (p *openAICompatible) Complete(ctx context.Context, in *ChatRequest, model string) (*ChatResponse, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	body, err := requestBodyFor(in, model, false, nil)
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
		// Drain before close so the connection can go back to the idle pool
		// instead of being torn down; an undrained body is a wasted handshake
		// on the next request.
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

	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &Error{
			Provider: p.name, Kind: KindUpstream,
			Message:         "upstream returned a body that is not a chat completion: " + truncate(raw, 256),
			Retryable:       true,
			CountsAsFailure: true,
			Err:             err,
		}
	}
	out.Raw = raw
	return &out, nil
}

func (p *openAICompatible) Stream(ctx context.Context, in *ChatRequest, model string) (Stream, error) {
	// Usage is requested even when the client did not ask for it, because the
	// gateway needs token counts for logging and cost accounting. The proxy
	// layer drops the extra frame if the client is not expecting it.
	extra := map[string]any{
		"stream_options": map[string]any{"include_usage": true},
	}

	body, err := requestBodyFor(in, model, true, extra)
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

	return &openAIStream{
		provider: p.name,
		resp:     resp,
		dec:      newSSEDecoder(resp.Body),
		ctx:      ctx,
	}, nil
}

func (p *openAICompatible) Embed(ctx context.Context, in *EmbeddingRequest, model string) (*EmbeddingResponse, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	body, err := embeddingBodyFor(in, model)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.embeddingsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindInternal, Message: err.Error(), Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, classifyTransport(p.name, ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, classifyTransport(p.name, ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus(p.name, resp.StatusCode, raw)
	}
	var out EmbeddingResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &Error{Provider: p.name, Kind: KindUpstream, Message: "upstream returned an invalid embeddings response", Retryable: true, CountsAsFailure: true, Err: err}
	}
	out.Raw = raw
	return &out, nil
}

// openAIStream turns an SSE response body into a sequence of ChatChunks.
type openAIStream struct {
	provider string
	resp     *http.Response
	dec      *sseDecoder
	ctx      context.Context

	closeOnce sync.Once
	done      bool
}

func (s *openAIStream) Recv() (*ChatChunk, error) {
	if s.done {
		return nil, io.EOF
	}

	for {
		ev, err := s.dec.Next()
		if err != nil {
			s.done = true
			if errors.Is(err, io.EOF) {
				// The stream ended without [DONE]. Treat a clean close as a
				// normal end: some providers just stop.
				return nil, io.EOF
			}
			return nil, classifyTransport(s.provider, s.ctx, err)
		}

		data := bytes.TrimSpace(ev.Data)
		if len(data) == 0 {
			continue // keep-alive comment or empty frame
		}
		if bytes.Equal(data, sseTerminator) {
			s.done = true
			return nil, io.EOF
		}

		if perr := errorPayload(s.provider, data); perr != nil {
			s.done = true
			return nil, perr
		}

		var chunk ChatChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			// Unparseable frames are skipped rather than fatal: a provider
			// extension frame should not kill an otherwise healthy stream.
			continue
		}
		chunk.Raw = append(json.RawMessage(nil), data...)
		return &chunk, nil
	}
}

func (s *openAIStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.resp != nil && s.resp.Body != nil {
			err = s.resp.Body.Close()
		}
	})
	return err
}
