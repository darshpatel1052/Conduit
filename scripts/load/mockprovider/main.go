// Command mockprovider is an OpenAI-compatible upstream that answers instantly.
//
// It exists because throughput measured against a real model is not a
// measurement of the gateway. llama3.2:3b answers in ~450ms and saturates one
// GPU at a handful of concurrent requests, so any load test pointed at it
// reports the model's ceiling no matter how the gateway is written — the
// gateway could be ten times slower and the number would not move.
//
// Pointing the gateway at this instead removes the model from the experiment,
// so what is left is the gateway's own capacity: its routing, its middleware,
// its connection pooling and its per-request allocation.
//
//	go run ./scripts/load/mockprovider -addr :19100
//
// Flags shape the upstream's behaviour so failure paths can be loaded too:
//
//	-latency 50ms     fixed think time before responding
//	-fail-rate 0.1    fraction of requests answered with a 500
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var requests atomic.Uint64

func main() {
	addr := flag.String("addr", ":19100", "listen address")
	latency := flag.Duration("latency", 0, "artificial think time before responding")
	failRate := flag.Float64("fail-rate", 0, "fraction of requests to fail with a 500")
	chunks := flag.Int("chunks", 24, "number of SSE chunks for streaming responses")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}

		if *latency > 0 {
			time.Sleep(*latency)
		}
		if *failRate > 0 && rand.Float64() < *failRate {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"synthetic upstream failure","type":"server_error"}}`)
			return
		}

		if req.Stream {
			streamResponse(w, req.Model, *chunks)
			return
		}
		blockingResponse(w, req.Model)
	})

	// Enough of the models endpoint for the gateway's readiness probe.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "mock-model", "object": "model", "owned_by": "mockprovider"},
			},
		})
	})

	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		var inputs []string
		var one string
		if json.Unmarshal(req.Input, &one) == nil {
			inputs = []string{one}
		} else if json.Unmarshal(req.Input, &inputs) != nil {
			http.Error(w, `{"error":{"message":"input must be a string or string array"}}`, http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, 0, len(inputs))
		for i, input := range inputs {
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": mockEmbedding(input)})
		}
		writeJSON(w, map[string]any{"object": "list", "model": req.Model, "data": data, "usage": map[string]int{"prompt_tokens": len(strings.Fields(strings.Join(inputs, " "))), "total_tokens": len(strings.Fields(strings.Join(inputs, " ")))}})
	})

	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"models": []map[string]any{{"name": "mock-model", "model": "mock-model"}},
		})
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"requests": requests.Load()})
	})

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// No WriteTimeout, for the same reason the gateway has none: it is an
		// absolute deadline on the whole response and would truncate streams.
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("mockprovider listening on %s (latency=%s fail-rate=%.2f)", *addr, *latency, *failRate)
	log.Fatal(srv.ListenAndServe())
}

func blockingResponse(w http.ResponseWriter, model string) {
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": mockAnswer},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 18, "completion_tokens": 24, "total_tokens": 42,
		},
	})
}

func streamResponse(w http.ResponseWriter, model string, chunks int) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	words := strings.Fields(mockAnswer)

	for i := 0; i < chunks; i++ {
		word := words[i%len(words)] + " "
		frame := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": word},
			}},
		}
		b, _ := json.Marshal(frame)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return // client hung up
		}
		// Flush per chunk. Without this the whole stream arrives as one buffer
		// at the end, and the gateway's time-to-first-token would be measuring
		// this program's buffering rather than anything real.
		if err := rc.Flush(); err != nil {
			return
		}
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const mockAnswer = "The gateway routes the request to a provider and relays the response back to the caller unchanged."

func mockEmbedding(text string) []float32 {
	const dimensions = 768
	vector := make([]float32, dimensions)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		sum := sha256.Sum256([]byte(strings.Trim(word, ".,!?;:\"'()")))
		index := binary.BigEndian.Uint16(sum[:2]) % dimensions
		sign := float32(1)
		if sum[2]&1 == 1 {
			sign = -1
		}
		vector[index] += sign
	}
	var norm float64
	for _, value := range vector {
		norm += float64(value * value)
	}
	if norm == 0 {
		return vector
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range vector {
		vector[i] *= scale
	}
	return vector
}
