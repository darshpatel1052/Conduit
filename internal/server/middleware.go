package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// responseRecorder captures the status and byte count for access logging.
//
// It implements Unwrap so http.ResponseController can still reach the
// underlying flusher. Without that method, wrapping the ResponseWriter here
// would silently disable SSE flushing for every streamed response — the
// classic way middleware breaks streaming.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// requestLogger emits one structured line per request and attaches a
// request-scoped logger to the context.
//
// One line per request, with fields rather than a formatted string, is what
// makes the log queryable: "p99 latency for model X on provider Y" is a filter,
// not a regex.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health checks run every couple of seconds forever; logging them
			// at info would bury the traffic that matters.
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			reqID := middleware.GetReqID(r.Context())

			reqLog := log.With("request_id", reqID)
			ctx := telemetry.WithLogger(r.Context(), reqLog)

			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r.WithContext(ctx))

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", rec.written,
				"remote", r.RemoteAddr,
			}
			// Surface the routing decision the proxy recorded in headers, so
			// the access log alone answers "who served this request".
			for _, h := range []string{"X-Conduit-Provider", "X-Conduit-Route", "X-Conduit-Model", "X-Conduit-Attempts"} {
				if v := rec.Header().Get(h); v != "" {
					attrs = append(attrs, strings.ToLower(strings.TrimPrefix(h, "X-Conduit-")), v)
				}
			}

			switch {
			case status >= 500:
				reqLog.Error("request failed", attrs...)
			case status >= 400:
				reqLog.Warn("request rejected", attrs...)
			default:
				reqLog.Info("request completed", attrs...)
			}
		})
	}
}

// recoverer turns a panic in a handler into a 500 for that one request instead
// of a process crash that drops every other in-flight connection with it.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is the documented way to abandon a
					// response; re-panicking preserves that contract.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					telemetry.LoggerFrom(r.Context()).Error("panic recovered in handler",
						"panic", rec,
						"path", r.URL.Path,
						"stack", string(debug.Stack()))
					writeError(w, r, http.StatusInternalServerError, "internal_error",
						"the gateway encountered an internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// limitBody caps the inbound request body. Prompts are attacker-controlled
// input, and without a cap a single request can make the gateway buffer
// arbitrary memory.
func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/livez"
}
