// Package embed turns prompts into vectors for the semantic cache.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder produces a vector for a piece of text.
type Embedder interface {
	// Embed returns the vector for text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Dimensions is the vector width this embedder produces. The pgvector
	// column is declared with a fixed width, so a mismatch is a hard error
	// rather than a degraded result — swapping embedding models means a
	// migration, not a config edit.
	Dimensions() int

	// Model identifies the embedding model, stored alongside each cache entry.
	// Vectors from different models are not comparable: cosine distance
	// between them is arithmetic, not meaning.
	Model() string
}

// Client calls an OpenAI-compatible /v1/embeddings endpoint.
//
// Ollama, OpenAI and most hosted providers all expose this shape, so switching
// from a free local model to a paid hosted one is a base URL and a credential —
// plus a migration, because the dimensions change.
type Client struct {
	endpoint         string
	fallbackEndpoint string
	model            string
	apiKey           string
	dimensions       int
	timeout          time.Duration
	http             *http.Client
}

// Config configures an embedding client.
type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	Dimensions int
	Timeout    time.Duration
}

// New builds an embedding client.
func New(cfg Config, httpClient *http.Client) *Client {
	base := strings.TrimRight(cfg.BaseURL, "/")
	endpoint := base + "/v1/embeddings"
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/embeddings"
	}
	if strings.HasSuffix(base, "/embeddings") {
		endpoint = base
	}
	// Ollama also exposes its native embeddings endpoint. Keep it as a
	// compatibility fallback for installations that predate, or disable, the
	// OpenAI-compatible /v1/embeddings surface.
	nativeBase := strings.TrimSuffix(base, "/v1")

	return &Client{
		endpoint:         endpoint,
		fallbackEndpoint: nativeBase + "/api/embed",
		model:            cfg.Model,
		apiKey:           cfg.APIKey,
		dimensions:       cfg.Dimensions,
		timeout:          cfg.Timeout,
		http:             httpClient,
	}
}

func (c *Client) Dimensions() int { return c.dimensions }
func (c *Client) Model() string   { return c.model }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type nativeEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed returns the vector for text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("cannot embed empty text")
	}

	// The embedding call sits in front of the provider call on every cacheable
	// request, so its timeout must be tight. A slow embedder would add latency
	// to requests it is supposed to be making faster; better to give up and go
	// straight to the provider.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body, err := json.Marshal(embedRequest{Model: c.model, Input: text})
	if err != nil {
		return nil, err
	}

	raw, status, err := c.post(ctx, c.endpoint, body)
	if err != nil {
		return nil, err
	}

	// OpenAI-compatible embeddings are the normal path. Older Ollama servers
	// may only implement the native endpoint, which returns `embeddings` rather
	// than OpenAI's `data[0].embedding` envelope.
	if status == http.StatusNotFound && c.fallbackEndpoint != c.endpoint {
		raw, status, err = c.post(ctx, c.fallbackEndpoint, body)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("embedding provider returned %d: %s", status, truncate(raw, 256))
		}
		var native nativeEmbedResponse
		if err := json.Unmarshal(raw, &native); err != nil {
			return nil, fmt.Errorf("decode native embedding response: %w", err)
		}
		if len(native.Embeddings) == 0 || len(native.Embeddings[0]) == 0 {
			return nil, fmt.Errorf("embedding provider returned no vector")
		}
		return c.validateDimensions(native.Embeddings[0])
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("embedding provider returned %d: %s", status, truncate(raw, 256))
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("embedding provider error: %s", out.Error.Message)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding provider returned no vector")
	}

	return c.validateDimensions(out.Data[0].Embedding)
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("embedding request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read embedding response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

func (c *Client) validateDimensions(vec []float32) ([]float32, error) {
	// A width mismatch means the configured model is not the one the cache
	// table was built for. Silently accepting it would either error deep in the
	// database driver or, worse, compare vectors that mean different things.
	if c.dimensions > 0 && len(vec) != c.dimensions {
		return nil, fmt.Errorf("embedding model %q returned %d dimensions but the cache expects %d: "+
			"changing embedding models requires a schema migration", c.model, len(vec), c.dimensions)
	}
	return vec, nil
}

// Normalize scales a vector to unit length.
//
// With unit vectors, cosine distance and inner product agree, so the similarity
// score is directly interpretable as "1.0 means identical". Most embedding APIs
// already return normalised vectors; doing it again is cheap insurance against
// one that does not.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// CosineSimilarity is used by tests and by the in-memory cache fallback.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton's method converges fast enough here and keeps the package free of
	// a math import purely for one call.
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
