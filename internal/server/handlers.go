package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/control"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// errorBody mirrors OpenAI's error envelope. A drop-in gateway has to fail in
// the shape clients already parse, or every SDK's error handling breaks the
// moment it points at Conduit.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// writeJSON marshals into a buffer before touching the response.
//
// Encoding straight into the ResponseWriter would commit the status line first,
// so anything unmarshalable — a NaN float being the realistic case, since
// encoding/json rejects those and ClickHouse returns one for a quantile over
// zero rows — produces a 200 with an empty body. That is the worst available
// failure: the client sees success and parses nothing. Buffering costs one
// allocation on responses that are already small, and turns the same case into
// an honest 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		// Hand-rolled rather than routed back through this function, so a
		// failure to encode the error cannot recurse.
		_, _ = w.Write([]byte(`{"error":{"message":"response could not be encoded",` +
			`"type":"internal_error","code":"internal_error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

func writeError(w http.ResponseWriter, r *http.Request, status int, kind, message string) {
	// 499 is nginx's "client closed request": there is no client left to
	// receive a body, so writing one just wastes work.
	if status == 499 {
		return
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Message: message, Type: kind, Code: kind}})
}

func writeProviderError(w http.ResponseWriter, r *http.Request, err error) {
	status := provider.StatusCodeFor(err)
	kind := string(provider.KindInternal)
	message := err.Error()

	if pe, ok := provider.AsError(err); ok {
		kind = string(pe.Kind)
		message = pe.Message
		if message == "" {
			message = pe.Error()
		}
	}
	writeError(w, r, status, kind, message)
}

// handleHealthz is liveness: the process is up and serving.
//
// Deliberately dependency-free. A liveness probe that checks downstreams turns
// a provider outage into a restart loop — the kernel kills a gateway that was
// working perfectly because something on the far side of the internet was not.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "conduit",
		"uptime":  time.Since(s.started).Round(time.Second).String(),
	})
}

// handleReadyz is readiness: this instance can usefully serve traffic.
//
// Unlike liveness, this does consider providers — but only whether *any*
// provider can be reached. Requiring all of them would take a healthy replica
// out of the load balancer over one degraded upstream, which is the opposite of
// what a gateway with failover is for.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.controlReady != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.controlReady(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "unavailable", "reason": "control-plane dependencies are unavailable",
			})
			return
		}
	}
	if !s.router.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"reason": "no provider is currently accepting traffic",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// handleModels implements GET /v1/models, which OpenAI clients call to
// enumerate what is available.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rtr := s.router
	if principal, ok := auth.PrincipalFrom(r.Context()); ok && principal.ProjectID != "" && s.controlRuntime != nil {
		runtime, err := s.controlRuntime.Resolve(r.Context(), principal.ProjectID)
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "project configuration is temporarily unavailable")
			return
		}
		rtr = runtime.Router
	}
	models := rtr.Models()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  s.started.Unix(),
			"owned_by": "conduit",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleAdminRoutes exposes the live routing table and breaker states.
func (s *Server) handleAdminRoutes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.router.Snapshot())
}

// handleChatCompletions is the gateway's reason to exist: POST
// /v1/chat/completions, OpenAI-compatible, streaming or not.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := telemetry.LoggerFrom(ctx)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "invalid_request",
				"request body exceeds the configured limit of "+strconv.FormatInt(maxErr.Limit, 10)+" bytes")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not read request body: "+err.Error())
		return
	}

	req, err := provider.ParseChatRequest(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		// Rejected before any upstream is contacted: a malformed request would
		// fail identically at every provider, so spending failover attempts on
		// it is pure waste.
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	principal, _ := auth.PrincipalFrom(ctx)
	pxy := s.proxy
	var projectCost func(string, string, int, int) float64
	if principal != nil && principal.ProjectID != "" && s.controlRuntime != nil {
		runtime, err := s.controlRuntime.Resolve(ctx, principal.ProjectID)
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "project configuration is temporarily unavailable")
			return
		}
		pxy = runtime.Proxy
		if !runtime.Supports(req.Model, control.ProfileKindGeneration) {
			writeError(w, r, http.StatusBadRequest, "profile_kind_mismatch", "Profile "+req.Model+" is not a generation profile.")
			return
		}
		projectCost = func(providerName, upstreamModel string, promptTokens, completionTokens int) float64 {
			return runtime.Cost(req.Model, providerName, upstreamModel, promptTokens, completionTokens)
		}
		w.Header().Set("X-Conduit-Project", runtime.Project.Slug)
		w.Header().Set("X-Conduit-Profile", req.Model)
		if principal.RequireEndUser && principal.EndUserPseudonym == "" {
			writeError(w, r, http.StatusBadRequest, "end_user_required", "X-Conduit-End-User is required by this project's policy")
			return
		}
	}
	if !s.checkMonthlyBudget(w, r, principal) {
		return
	}

	// A key restricted to certain models must not reach a provider it is not
	// entitled to. Checked here rather than in middleware because the model is
	// only known once the body is parsed.
	if principal != nil && !principal.CanUseModel(req.Model) {
		log.Warn("request rejected: model not permitted for this key",
			"principal", principal.ID, "model", req.Model)
		writeError(w, r, http.StatusForbidden, "permission_error",
			"This API key does not have access to model "+req.Model+".")
		return
	}

	if !s.checkTokenBudget(w, r, principal, req.Model) {
		return
	}

	// One observation per request, published to metrics, the trace and the
	// analytics queue on every exit path.
	obs := s.beginObservation(r, req.Model, req.Stream, "chat.completions", principal, projectCost)
	defer s.finish(r, obs)

	// The cache is consulted before routing: a hit skips provider selection,
	// the breaker, and the upstream call entirely, which is where the latency
	// and cost savings come from.
	if s.lookupCache(w, r, req, req.Model, principal, obs) {
		return
	}

	if req.Stream {
		s.streamCompletion(w, r, req, log, principal, obs, pxy)
		return
	}
	s.blockingCompletion(w, r, req, log, principal, obs, pxy)
}

func (s *Server) blockingCompletion(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest,
	log *slog.Logger, principal *auth.Principal, obs *observation, pxy *proxy.Proxy) {

	resp, result, err := pxy.Complete(r.Context(), req)
	obs.result = result
	if err != nil {
		obs.err = err
		obs.status = provider.StatusCodeFor(err)
		logAttempts(log, req, result, err)
		writeProviderError(w, r, err)
		return
	}

	setResultHeaders(w, result)
	logAttempts(log, req, result, nil)

	if result.Usage != nil {
		s.debitTokens(r, principal, result.Usage.TotalTokens)
	}

	// Write-through on miss, so the next equivalent prompt hits.
	s.storeInCache(r, req, req.Model, resp.Raw, result.Usage)

	// Relay the provider's own bytes rather than a re-marshalled struct, so
	// fields Conduit does not model (logprobs, provider extensions) survive
	// the hop intact.
	payload := resp.Raw
	if len(payload) == 0 {
		b, err := json.Marshal(resp)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "could not encode response")
			return
		}
		payload = b
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest,
	log *slog.Logger, principal *auth.Principal, obs *observation, pxy *proxy.Proxy) {

	// OpenStream blocks until the first chunk arrives, which is what keeps
	// failover possible: nothing has been written to the client yet, so a
	// failure here can still be answered with a normal JSON error and a
	// different provider.
	handle, result, err := pxy.OpenStream(r.Context(), req)
	obs.result = result
	if err != nil {
		obs.err = err
		obs.status = provider.StatusCodeFor(err)
		logAttempts(log, req, result, err)
		writeProviderError(w, r, err)
		return
	}

	if s.metrics != nil {
		s.metrics.StreamsActive.Inc()
		defer s.metrics.StreamsActive.Dec()
	}

	logAttempts(log, req, result, nil)

	if err := pxy.Relay(r.Context(), w, handle, req); err != nil {
		// Headers are already sent; the relay reports what it can in-stream.
		log.Debug("stream relay ended with error", "error", err)
	}

	// Debited after the stream finishes, when usage is finally known. Uses a
	// detached context so a client that hung up mid-stream is still charged for
	// the tokens the provider actually generated.
	if result.Usage != nil {
		s.debitTokens(r, principal, result.Usage.TotalTokens)
	}

	log.Info("stream completed",
		"provider", result.Provider,
		"route", result.Route,
		"upstream_model", result.UpstreamModel,
		"ttfb_ms", result.TTFB.Milliseconds(),
		"duration_ms", result.Duration.Milliseconds(),
		"prompt_tokens", usageField(result, func(u *provider.Usage) int { return u.PromptTokens }),
		"completion_tokens", usageField(result, func(u *provider.Usage) int { return u.CompletionTokens }),
	)
}
