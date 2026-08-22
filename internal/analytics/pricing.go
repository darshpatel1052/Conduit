package analytics

import (
	"strings"
	"sync"
)

// Price is the cost of a model per million tokens, in USD.
//
// Per *million* rather than per token because that is how every provider
// publishes it, and because float64 arithmetic on per-token prices
// (0.00000015) loses precision in ways that show up once you multiply by
// billions of tokens.
type Price struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// Pricing resolves a model name to its price.
//
// Output tokens cost several times input tokens at every provider, so a single
// blended rate would misreport cost badly for the two workloads that matter
// most: long-prompt/short-answer (RAG) and short-prompt/long-answer
// (generation). They are tracked separately for that reason.
type Pricing struct {
	mu     sync.RWMutex
	exact  map[string]Price
	prefix []prefixPrice
}

type prefixPrice struct {
	prefix string
	price  Price
}

// DefaultPricing returns a starting table.
//
// These are list prices and they change; the table is config-overridable
// precisely because a number hard-coded in a binary goes stale the week after
// it ships. Local models are zero — the electricity is real but it is not a
// per-token cost, and pretending otherwise would make the savings figures
// meaningless.
func DefaultPricing() *Pricing {
	p := &Pricing{
		exact: map[string]Price{
			// OpenAI
			"gpt-5-mini":    {InputPerMillion: 0.25, OutputPerMillion: 2.00},
			"gpt-4o":        {InputPerMillion: 2.50, OutputPerMillion: 10.00},
			"gpt-4o-mini":   {InputPerMillion: 0.15, OutputPerMillion: 0.60},
			"gpt-4-turbo":   {InputPerMillion: 10.00, OutputPerMillion: 30.00},
			"gpt-3.5-turbo": {InputPerMillion: 0.50, OutputPerMillion: 1.50},
			"o1-mini":       {InputPerMillion: 3.00, OutputPerMillion: 12.00},

			// Anthropic
			"claude-3-5-sonnet": {InputPerMillion: 3.00, OutputPerMillion: 15.00},
			"claude-3-5-haiku":  {InputPerMillion: 0.80, OutputPerMillion: 4.00},
			"claude-3-opus":     {InputPerMillion: 15.00, OutputPerMillion: 75.00},

			// Embeddings
			"text-embedding-3-small": {InputPerMillion: 0.02},
			"text-embedding-3-large": {InputPerMillion: 0.13},
		},
		prefix: []prefixPrice{
			// Locally-hosted models have no per-token price. Matched by prefix
			// so every Ollama tag (llama3.2:3b, llama3.2:1b, ...) resolves.
			{prefix: "llama", price: Price{}},
			{prefix: "qwen", price: Price{}},
			{prefix: "mistral", price: Price{}},
			{prefix: "gemma", price: Price{}},
			{prefix: "phi", price: Price{}},
			{prefix: "nomic-embed", price: Price{}},
			{prefix: "deepseek-r1", price: Price{}},
		},
	}
	return p
}

// Set adds or overrides a price.
func (p *Pricing) Set(model string, price Price) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exact[strings.ToLower(model)] = price
}

// Lookup resolves a model's price. Exact match wins, then the longest matching
// prefix, so "gpt-4o-2024-11-20" falls back to the "gpt-4o" entry rather than
// silently costing nothing.
func (p *Pricing) Lookup(model string) (Price, bool) {
	key := strings.ToLower(strings.TrimSpace(model))

	p.mu.RLock()
	defer p.mu.RUnlock()

	if price, ok := p.exact[key]; ok {
		return price, true
	}

	var best string
	var bestPrice Price
	found := false

	for name, price := range p.exact {
		if strings.HasPrefix(key, name) && len(name) > len(best) {
			best, bestPrice, found = name, price, true
		}
	}
	for _, pp := range p.prefix {
		if strings.HasPrefix(key, pp.prefix) && len(pp.prefix) > len(best) {
			best, bestPrice, found = pp.prefix, pp.price, true
		}
	}
	return bestPrice, found
}

// Cost computes the USD cost of a completion.
//
// Returns 0 for unknown models rather than guessing. An invented price is worse
// than a missing one: it produces a spend figure someone will put in a slide.
func (p *Pricing) Cost(model string, promptTokens, completionTokens int) float64 {
	price, ok := p.Lookup(model)
	if !ok {
		return 0
	}
	const perMillion = 1_000_000.0
	return (float64(promptTokens)/perMillion)*price.InputPerMillion +
		(float64(completionTokens)/perMillion)*price.OutputPerMillion
}

// Known reports whether a price exists, so callers can distinguish "free" from
// "unpriced" — both are 0.0, and conflating them makes cost reporting a lie.
func (p *Pricing) Known(model string) bool {
	_, ok := p.Lookup(model)
	return ok
}
