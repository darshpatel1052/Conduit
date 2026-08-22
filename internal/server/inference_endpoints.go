package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/control"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
)

func readInferenceBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err == nil {
		return body, true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "request body exceeds the configured limit of "+strconv.FormatInt(maxErr.Limit, 10)+" bytes")
	} else {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "could not read request body: "+err.Error())
	}
	return nil, false
}

func (s *Server) projectInference(w http.ResponseWriter, r *http.Request, profile, kind string) (*auth.Principal, *proxy.Proxy, func(string, string, int, int) float64, bool) {
	principal, _ := auth.PrincipalFrom(r.Context())
	pxy := s.proxy
	var cost func(string, string, int, int) float64
	if principal != nil && principal.ProjectID != "" && s.controlRuntime != nil {
		runtime, err := s.controlRuntime.Resolve(r.Context(), principal.ProjectID)
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "project configuration is temporarily unavailable")
			return principal, nil, nil, false
		}
		if !runtime.Supports(profile, kind) {
			writeError(w, r, http.StatusBadRequest, "profile_kind_mismatch", "Profile "+profile+" is not a "+kind+" profile.")
			return principal, nil, nil, false
		}
		pxy = runtime.Proxy
		cost = func(providerName, upstreamModel string, promptTokens, completionTokens int) float64 {
			return runtime.Cost(profile, providerName, upstreamModel, promptTokens, completionTokens)
		}
		w.Header().Set("X-Conduit-Project", runtime.Project.Slug)
		w.Header().Set("X-Conduit-Profile", profile)
		if principal.RequireEndUser && principal.EndUserPseudonym == "" {
			writeError(w, r, http.StatusBadRequest, "end_user_required", "X-Conduit-End-User is required by this project's policy")
			return principal, nil, nil, false
		}
	}
	if principal != nil && !principal.CanUseModel(profile) {
		writeError(w, r, http.StatusForbidden, "permission_error", "This API key does not have access to model "+profile+".")
		return principal, nil, nil, false
	}
	if !s.checkMonthlyBudget(w, r, principal) || !s.checkTokenBudget(w, r, principal, profile) {
		return principal, nil, nil, false
	}
	return principal, pxy, cost, true
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, ok := readInferenceBody(w, r)
	if !ok {
		return
	}
	req, err := provider.ParseEmbeddingRequest(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err = req.Validate(); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	principal, pxy, projectCost, ok := s.projectInference(w, r, req.Model, control.ProfileKindEmbedding)
	if !ok {
		return
	}
	obs := s.beginObservation(r, req.Model, false, "embeddings", principal, projectCost)
	defer s.finish(r, obs)

	resp, result, err := pxy.Embed(r.Context(), req)
	obs.result = result
	if err != nil {
		obs.err = err
		obs.status = provider.StatusCodeFor(err)
		writeProviderError(w, r, err)
		return
	}
	setResultHeaders(w, result)
	if result.Usage != nil {
		s.debitTokens(r, principal, result.Usage.TotalTokens)
	}
	payload := resp.Raw
	if len(payload) == 0 {
		payload, _ = json.Marshal(resp)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := readInferenceBody(w, r)
	if !ok {
		return
	}
	req, err := provider.ParseResponseRequest(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err = req.Validate(); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chat, err := req.ChatRequest()
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	principal, pxy, projectCost, ok := s.projectInference(w, r, req.Model, control.ProfileKindGeneration)
	if !ok {
		return
	}
	obs := s.beginObservation(r, req.Model, req.Stream, "responses", principal, projectCost)
	defer s.finish(r, obs)
	if req.Stream {
		s.streamResponse(w, r, req, chat, principal, pxy, obs)
		return
	}

	chatResp, result, err := pxy.Complete(r.Context(), chat)
	obs.result = result
	if err != nil {
		obs.err = err
		obs.status = provider.StatusCodeFor(err)
		writeProviderError(w, r, err)
		return
	}
	setResultHeaders(w, result)
	if result.Usage != nil {
		s.debitTokens(r, principal, result.Usage.TotalTokens)
	}
	text := ""
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
		text = chatResp.Choices[0].Message.Text()
	}
	writeJSON(w, http.StatusOK, responseObject(responseID(), req, text, result.Usage, "completed"))
}

func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, req *provider.ResponseRequest, chat *provider.ChatRequest, principal *auth.Principal, pxy *proxy.Proxy, obs *observation) {
	handle, result, err := pxy.OpenStream(r.Context(), chat)
	obs.result = result
	if err != nil {
		obs.err = err
		obs.status = provider.StatusCodeFor(err)
		writeProviderError(w, r, err)
		return
	}
	defer handle.Stream.Close()
	setResultHeaders(w, result)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	id := responseID()
	sequence := 0
	emit := func(eventType string, payload map[string]any) error {
		payload["type"] = eventType
		payload["sequence_number"] = sequence
		sequence++
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded); writeErr != nil {
			return writeErr
		}
		return rc.Flush()
	}
	created := responseObject(id, req, "", nil, "in_progress")
	if err = emit("response.created", map[string]any{"response": created}); err != nil {
		return
	}
	itemID := "msg_" + strings.TrimPrefix(id, "resp_")
	_ = emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
	_ = emit("response.content_part.added", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})

	var text strings.Builder
	started := time.Now()
	for {
		chunk, recvErr := handle.Stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			obs.err = recvErr
			_ = emit("response.failed", map[string]any{"response": responseObject(id, req, text.String(), result.Usage, "failed")})
			return
		}
		if chunk == nil {
			continue
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			delta := choice.Delta.Text()
			if delta == "" {
				continue
			}
			text.WriteString(delta)
			if err = emit("response.output_text.delta", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta}); err != nil {
				return
			}
		}
	}
	result.Duration = result.TTFB + time.Since(started)
	_ = emit("response.output_text.done", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "text": text.String()})
	_ = emit("response.content_part.done", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}})
	_ = emit("response.output_item.done", map[string]any{"output_index": 0, "item": responseMessage(itemID, text.String(), "completed")})
	_ = emit("response.completed", map[string]any{"response": responseObject(id, req, text.String(), result.Usage, "completed")})
	if result.Usage != nil {
		s.debitTokens(r, principal, result.Usage.TotalTokens)
	}
}

func responseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "resp_" + hex.EncodeToString(b)
}

func responseMessage(id, text, status string) map[string]any {
	return map[string]any{"id": id, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responseObject(id string, req *provider.ResponseRequest, text string, usage *provider.Usage, status string) map[string]any {
	var responseUsage any
	if usage != nil {
		responseUsage = map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens}
	}
	output := []any{}
	if status == "completed" || text != "" {
		output = append(output, responseMessage("msg_"+strings.TrimPrefix(id, "resp_"), text, status))
	}
	return map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": req.Model, "output": output, "usage": responseUsage, "error": nil, "instructions": req.Instructions, "store": false}
}
