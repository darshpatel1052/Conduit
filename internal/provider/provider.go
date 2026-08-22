// Package provider adapts each upstream LLM API to one interface.
//
// Everything downstream of this package — routing, load balancing, failover,
// circuit breaking, and later caching and analytics — is written against
// Provider alone. That is what keeps "add Bedrock" a matter of writing one new
// file rather than threading a vendor conditional through the whole gateway.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
)

// Provider is a single upstream endpoint the gateway can send inference to.
type Provider interface {
	// Name is the operator-assigned identifier from routes.yaml.
	Name() string

	// Type is the adapter family: ollama, openai, anthropic.
	Type() string

	// Complete performs a non-streaming completion. model is the upstream
	// model name chosen by routing, which may differ from req.Model.
	Complete(ctx context.Context, req *ChatRequest, model string) (*ChatResponse, error)

	// Stream starts a streaming completion. The returned Stream must be closed
	// by the caller; closing it releases the upstream connection.
	Stream(ctx context.Context, req *ChatRequest, model string) (Stream, error)

	// Embed turns one or more inputs into vectors. Providers that do not offer
	// embeddings return an invalid-request error; embedding profiles are
	// validated before they reach this point in the project-backed runtime.
	Embed(ctx context.Context, req *EmbeddingRequest, model string) (*EmbeddingResponse, error)
}

// Stream is a server-to-client token stream.
type Stream interface {
	// Recv returns the next chunk, or io.EOF when the stream ends normally.
	Recv() (*ChatChunk, error)

	// Close releases the underlying connection. It is safe to call more than
	// once and safe to call while Recv is blocked.
	Close() error
}

// New builds the adapter for a provider config.
func New(cfg config.ProviderConfig, client *http.Client, log *slog.Logger) (Provider, error) {
	switch cfg.Type {
	case config.TypeOllama:
		return newOllama(cfg, client, log), nil
	case config.TypeOpenAI:
		return newOpenAI(cfg, client, log), nil
	case config.TypeAnthropic:
		return newAnthropic(cfg, client, log), nil
	default:
		return nil, fmt.Errorf("unknown provider type %q", cfg.Type)
	}
}

// NewHTTPClient builds the shared client used for every upstream call.
//
// Two choices here matter more than the rest:
//
//   - There is no http.Client.Timeout. That field is an absolute deadline
//     covering the entire response body read, so any value at all would cut
//     long streams off mid-generation. Bounds come from ResponseHeaderTimeout
//     (catches a wedged provider before a single byte arrives) and per-request
//     context deadlines the proxy sets according to streaming vs not.
//
//   - MaxIdleConnsPerHost is raised well above the net/http default of 2.
//     A gateway concentrates traffic onto a handful of hosts, so with the
//     default every request past the second to a provider opens a fresh
//     connection and discards it afterwards — paying a TCP and TLS handshake,
//     tens of milliseconds, on requests that should have reused a warm socket.
//
// One client is shared by every provider so the connection pool is shared too;
// per-provider clients would fragment the pool and multiply idle connections.
func NewHTTPClient(u config.Upstream) *http.Client {
	dialer := &net.Dialer{
		Timeout:   u.ConnectTimeout.D(),
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          u.MaxIdleConns,
		MaxIdleConnsPerHost:   u.MaxIdleConnsPerHost,
		MaxConnsPerHost:       u.MaxConnsPerHost,
		IdleConnTimeout:       u.IdleConnTimeout.D(),
		TLSHandshakeTimeout:   u.ConnectTimeout.D(),
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: u.ResponseHeaderTimeout.D(),

		// Compression is off by default: token deltas are tiny frequent writes,
		// and gzip's buffering fights the whole point of streaming.
		DisableCompression: u.DisableCompression,
	}

	return &http.Client{
		Transport: transport,
		// Never follow redirects to a different host: a compromised or
		// misconfigured upstream could otherwise redirect the gateway into
		// forwarding API credentials somewhere they do not belong.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
