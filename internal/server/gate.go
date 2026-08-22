package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/ratelimit"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// authenticate resolves the caller and attaches them to the request context.
//
// Health and readiness probes are exempt: an orchestrator has no credential to
// present, and requiring one would make every pod look dead.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isUnauthenticatedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if !s.auth.Enabled() {
			// Auth off: identify by IP so rate limits still have something to
			// key on. An unauthenticated gateway with no limiter at all is a
			// free proxy for anyone who finds it.
			p := s.auth.AnonymousPrincipal(clientIP(r), s.cfg.RateLimit.AnonymousRequestsPerMinute)
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
			return
		}

		principal, err := s.auth.AuthenticateRequest(r.Context(), r.Header.Get(s.auth.Header()), clientIP(r))
		if err != nil {
			// WWW-Authenticate tells a well-behaved client how to fix this.
			w.Header().Set("WWW-Authenticate", `Bearer realm="conduit"`)
			telemetry.LoggerFrom(r.Context()).Warn("request rejected: authentication failed",
				"path", r.URL.Path, "remote", clientIP(r))
			// Recorded without the presented credential, and with no principal:
			// a failed auth attempt has no trustworthy identity to attribute it
			// to, and logging the rejected key would put credentials — possibly
			// a valid one sent to the wrong host — into the analytics store.
			s.recordRejection(r, nil, "", http.StatusUnauthorized, "authentication", err.Error())
			writeError(w, r, http.StatusUnauthorized, "authentication_error",
				"Incorrect API key provided. Send it as `Authorization: Bearer <key>`.")
			return
		}

		if isAdminPath(r.URL.Path) && !principal.Admin {
			telemetry.LoggerFrom(r.Context()).Warn("request rejected: admin access required",
				"path", r.URL.Path, "principal", principal.ID)
			s.recordRejection(r, principal, "", http.StatusForbidden,
				"authorization", "admin access required")
			writeError(w, r, http.StatusForbidden, "permission_error",
				"This API key does not have access to administrative endpoints.")
			return
		}

		ctx := auth.WithPrincipal(r.Context(), principal)
		if principal.ProjectID != "" && s.controlRuntime != nil {
			rawUser := strings.TrimSpace(r.Header.Get("X-Conduit-End-User"))
			r.Header.Del("X-Conduit-End-User")
			if rawUser != "" {
				principal.EndUserPseudonym = s.controlRuntime.Pseudonym(principal.ProjectID, rawUser)
			}
		}
		ctx = telemetry.WithLogger(ctx,
			telemetry.LoggerFrom(ctx).With("api_key_id", principal.ID, "project_id", principal.ProjectID))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rateLimit enforces the per-principal request budget.
//
// Only request-rate is enforced here. Token limits cannot be: the token cost of
// a completion is unknown until it has been generated, so those are checked as
// a budget gate before the call and debited after (see debitTokens).
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isUnauthenticatedPath(r.URL.Path) || !s.cfg.RateLimit.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		principal, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		limit := principal.RequestsPerMinute
		if limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}

		key := ratelimit.Key("req", principal.RateLimitKey())
		decision, err := s.limiter.Allow(r.Context(), key, limit, 1)
		if err != nil && !decision.Allowed {
			writeError(w, r, http.StatusServiceUnavailable, "rate_limit_unavailable",
				"the rate limiter is unavailable and the gateway is configured to fail closed")
			return
		}

		setRateLimitHeaders(w, decision)

		if decision.Degraded {
			telemetry.LoggerFrom(r.Context()).Warn("rate limit not enforced: limiter degraded",
				"principal", principal.ID)
		}

		if !decision.Allowed {
			// Retry-After is what makes a 429 actionable. Without it a client
			// has to guess, and most guess "immediately".
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(decision.RetryAfter)))
			telemetry.LoggerFrom(r.Context()).Warn("request rejected: rate limit exceeded",
				"principal", principal.ID, "limit", limit,
				"retry_after_ms", decision.RetryAfter.Milliseconds())
			if s.metrics != nil {
				s.metrics.RateLimitRejections.WithLabelValues("requests").Inc()
			}
			s.recordRejection(r, principal, "", http.StatusTooManyRequests,
				"rate_limit", "request rate limit exceeded")
			writeError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded",
				"Rate limit reached for this API key. Limit: "+strconv.Itoa(limit)+
					" requests per "+s.cfg.RateLimit.Window.String()+".")
			return
		}

		if principal.ProjectID != "" && principal.ProjectRequestsPerMinute > 0 {
			projectDecision, err := s.limiter.Allow(r.Context(), ratelimit.Key("req", "project:"+principal.ProjectID), principal.ProjectRequestsPerMinute, 1)
			if err != nil && !projectDecision.Allowed {
				writeError(w, r, http.StatusServiceUnavailable, "rate_limit_unavailable", "the project limiter is unavailable")
				return
			}
			if !projectDecision.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(projectDecision.RetryAfter)))
				writeError(w, r, http.StatusTooManyRequests, "project_rate_limit_exceeded", "Project request rate limit reached.")
				return
			}
		}
		if principal.EndUserPseudonym != "" && principal.EndUserRequestsPerMinute != nil {
			userDecision, err := s.limiter.Allow(r.Context(), ratelimit.Key("req", "project:"+principal.ProjectID+":user:"+principal.EndUserPseudonym), *principal.EndUserRequestsPerMinute, 1)
			if err != nil && !userDecision.Allowed {
				writeError(w, r, http.StatusServiceUnavailable, "rate_limit_unavailable", "the end-user limiter is unavailable")
				return
			}
			if !userDecision.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(userDecision.RetryAfter)))
				writeError(w, r, http.StatusTooManyRequests, "end_user_rate_limit_exceeded", "End-user request rate limit reached.")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// checkTokenBudget rejects a request when the principal has already exhausted
// its token allowance. Called from the completions handler rather than as
// middleware, because it needs the parsed model and only applies to inference.
func (s *Server) checkTokenBudget(w http.ResponseWriter, r *http.Request, p *auth.Principal, model string) bool {
	if !s.cfg.RateLimit.Enabled || p == nil || p.TokensPerMinute <= 0 {
		return true
	}

	key := ratelimit.Key("tok", p.RateLimitKey())
	decision, err := s.limiter.Peek(r.Context(), key, p.TokensPerMinute)
	if err == nil && decision.Allowed && p.ProjectID != "" && p.ProjectTokensPerMinute > 0 {
		decision, err = s.limiter.Peek(r.Context(), ratelimit.Key("tok", "project:"+p.ProjectID), p.ProjectTokensPerMinute)
	}
	if err != nil || decision.Allowed {
		return true
	}

	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(decision.RetryAfter)))
	w.Header().Set("X-RateLimit-Limit-Tokens", strconv.Itoa(p.TokensPerMinute))
	w.Header().Set("X-RateLimit-Remaining-Tokens", "0")
	if s.metrics != nil {
		// Separate dimension from request-rate rejections: the two have
		// different causes and different fixes, and an alert that cannot tell
		// "too many calls" from "too many tokens" sends people to the wrong one.
		s.metrics.RateLimitRejections.WithLabelValues("tokens").Inc()
	}
	s.recordRejection(r, p, model, http.StatusTooManyRequests,
		"rate_limit", "token rate limit exceeded")
	writeError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded",
		"Token rate limit reached for this API key. Limit: "+
			strconv.Itoa(p.TokensPerMinute)+" tokens per "+s.cfg.RateLimit.Window.String()+".")
	return false
}

func (s *Server) checkMonthlyBudget(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	if s.accounting == nil || p == nil || p.ProjectID == "" || p.MonthlyBudgetUSD == nil {
		return true
	}
	spent, err := s.accounting.Current(r.Context(), p.ProjectID)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "budget_unavailable", "project budget cannot be verified")
		return false
	}
	if spent >= *p.MonthlyBudgetUSD {
		writeError(w, r, http.StatusTooManyRequests, "budget_exceeded", "Project monthly budget has been reached.")
		return false
	}
	if p.BudgetWarningPercent > 0 && spent/(*p.MonthlyBudgetUSD)*100 >= float64(p.BudgetWarningPercent) {
		w.Header().Set("X-Conduit-Budget-Warning", "true")
	}
	return true
}

// debitTokens charges actual usage after a completion.
//
// This is the half of token limiting that request limiting does not need: the
// cost is only knowable afterwards, so a request that pushes a principal over
// the line is served, and the overage is carried into the next window.
func (s *Server) debitTokens(r *http.Request, p *auth.Principal, tokens int) {
	if !s.cfg.RateLimit.Enabled || p == nil || p.TokensPerMinute <= 0 || tokens <= 0 {
		return
	}
	// Deliberately not r.Context(): for a streaming request that context is
	// already cancelled by the time usage is known, and the tokens were still
	// spent. Billing must not depend on the client sticking around.
	ctx, cancel := detachedContext(r, 2*time.Second)
	defer cancel()

	key := ratelimit.Key("tok", p.RateLimitKey())
	if err := s.limiter.Debit(ctx, key, p.TokensPerMinute, tokens); err != nil {
		telemetry.LoggerFrom(r.Context()).Warn("could not debit token usage",
			"principal", p.ID, "tokens", tokens, "error", err)
	}
	if p.ProjectID != "" && p.ProjectTokensPerMinute > 0 {
		if err := s.limiter.Debit(ctx, ratelimit.Key("tok", "project:"+p.ProjectID), p.ProjectTokensPerMinute, tokens); err != nil {
			telemetry.LoggerFrom(r.Context()).Warn("could not debit project token usage", "project_id", p.ProjectID, "tokens", tokens, "error", err)
		}
	}
}

func setRateLimitHeaders(w http.ResponseWriter, d ratelimit.Decision) {
	if d.Limit <= 0 {
		return
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	if d.Remaining >= 0 {
		h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	}
	if !d.ResetAt.IsZero() {
		h.Set("X-RateLimit-Reset", strconv.FormatInt(d.ResetAt.Unix(), 10))
	}
}

func retryAfterSeconds(d time.Duration) int {
	// Round up: advertising 0 invites an immediate retry, which is how a rate
	// limit becomes a busy loop.
	secs := int(d.Seconds())
	if time.Duration(secs)*time.Second < d {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return secs
}

// isUnauthenticatedPath lists the endpoints that must answer without a
// credential.
//
// Probes: an orchestrator has none, and demanding one makes every pod look
// dead. Metrics: a Prometheus scrape target that needs a credential is a scrape
// target nobody configures, and the endpoint exposes aggregate counters rather
// than prompt content. Both belong on the pod network, not the public internet
// — that is a deployment boundary, not an application one.
func (s *Server) isUnauthenticatedPath(path string) bool {
	if path == "/control/v1" || strings.HasPrefix(path, "/control/v1/") {
		return true
	}
	if isProbe(path) {
		return true
	}
	return s.cfg.Metrics.Enabled && path == s.cfg.Metrics.Path
}

func isAdminPath(requestPath string) bool {
	return requestPath == "/admin" || strings.HasPrefix(requestPath, "/admin/")
}

func clientIP(r *http.Request) string {
	// chi's RealIP middleware has already normalised X-Forwarded-For into
	// RemoteAddr, so this is the client address as far as the gateway can tell.
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
