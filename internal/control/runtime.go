package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/router"
	"github.com/redis/go-redis/v9"
)

type Runtime struct {
	Project      Project
	Policy       Policy
	Router       *router.Router
	Proxy        *proxy.Proxy
	prices       map[string][2]float64
	profileKinds map[string]string
}

func (r *Runtime) Supports(profile, kind string) bool {
	return r.profileKinds[profile] == kind
}

func (r *Runtime) Cost(profile, providerName, upstreamModel string, promptTokens, completionTokens int) float64 {
	price, ok := r.prices[profile+"\x00"+providerName+"\x00"+upstreamModel]
	if !ok {
		return 0
	}
	return float64(promptTokens)/1_000_000*price[0] + float64(completionTokens)/1_000_000*price[1]
}

type cachedRuntime struct {
	runtime *Runtime
	loaded  time.Time
}

type RuntimeManager struct {
	store    *Store
	client   *http.Client
	upstream config.Upstream
	log      *slog.Logger
	mu       sync.Mutex
	cache    map[string]cachedRuntime
	redis    *redis.Client
}

func NewRuntimeManager(store *Store, client *http.Client, upstream config.Upstream, log *slog.Logger) *RuntimeManager {
	return &RuntimeManager{store: store, client: client, upstream: upstream, log: log, cache: make(map[string]cachedRuntime)}
}

func (m *RuntimeManager) Resolve(ctx context.Context, projectID string) (*Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.cache[projectID]; ok && time.Since(cached.loaded) < 5*time.Second {
		return cached.runtime, nil
	}
	cfg, err := m.store.RuntimeConfig(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if cached, ok := m.cache[projectID]; ok && cached.runtime.Project.ConfigurationVersion == cfg.Project.ConfigurationVersion {
		cached.loaded = time.Now()
		cached.runtime.Policy = cfg.Policy
		m.cache[projectID] = cached
		return cached.runtime, nil
	}
	runtime, err := m.build(cfg)
	if err != nil {
		return nil, err
	}
	m.cache[projectID] = cachedRuntime{runtime: runtime, loaded: time.Now()}
	return runtime, nil
}

func (m *RuntimeManager) Invalidate(projectID string) {
	m.mu.Lock()
	delete(m.cache, projectID)
	m.mu.Unlock()
	if m.redis != nil {
		_ = m.redis.Publish(context.Background(), "conduit:configuration:invalidate", projectID).Err()
	}
}

func (m *RuntimeManager) EnableInvalidation(ctx context.Context, redisURL string) error {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return err
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return err
	}
	m.redis = client
	subscription := client.Subscribe(ctx, "conduit:configuration:invalidate")
	if _, err := subscription.Receive(ctx); err != nil {
		_ = client.Close()
		m.redis = nil
		return err
	}
	go func() {
		defer subscription.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-subscription.Channel():
				if !ok {
					return
				}
				m.mu.Lock()
				delete(m.cache, message.Payload)
				m.mu.Unlock()
			}
		}
	}()
	return nil
}
func (m *RuntimeManager) Close() error {
	if m.redis != nil {
		return m.redis.Close()
	}
	return nil
}

func (m *RuntimeManager) Pseudonym(projectID, raw string) string {
	return m.store.EndUserPseudonym(projectID, raw)
}

func (m *RuntimeManager) TestProvider(ctx context.Context, p RuntimeProvider) (ProviderTestResult, error) {
	result := ProviderTestResult{ProviderID: p.ID}
	credential, err := m.store.DecryptProviderCredential(p)
	if err != nil {
		return result, fmt.Errorf("decrypt provider credential: %w", err)
	}
	timeout := 10 * time.Second
	if p.TimeoutSeconds > 0 && time.Duration(p.TimeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(p.BaseURL, "/")
	endpoint := base + "/v1/models"
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/models"
	}
	if p.Type == config.TypeOllama {
		base = strings.TrimSuffix(base, "/v1")
		endpoint = base + "/api/tags"
	}
	req, err := http.NewRequestWithContext(testCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Accept", "application/json")
	if credential != "" {
		if p.Type == config.TypeAnthropic {
			req.Header.Set("x-api-key", credential)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
	}
	started := time.Now()
	resp, requestErr := m.client.Do(req)
	result.LatencyMS = time.Since(started).Milliseconds()
	if requestErr != nil {
		result.Message = "connection failed: " + requestErr.Error()
		return result, nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	result.StatusCode = resp.StatusCode
	result.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if result.OK {
		result.Message = "provider accepted the authenticated model-list request"
	} else {
		result.Message = fmt.Sprintf("provider returned HTTP %d", resp.StatusCode)
	}
	return result, nil
}

func (m *RuntimeManager) build(runtimeConfig *RuntimeConfig) (*Runtime, error) {
	routes := &config.Routes{}
	providerNames := make(map[string]string, len(runtimeConfig.Providers))
	prices := make(map[string][2]float64)
	profileKinds := make(map[string]string)
	for _, p := range runtimeConfig.Providers {
		providerNames[p.ID] = p.Name
		credential, err := m.store.DecryptProviderCredential(p)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %q: %w", p.Name, err)
		}
		providerType := p.Type
		if providerType == "openai_compatible" {
			providerType = config.TypeOpenAI
		}
		pc := config.ProviderConfig{
			Name: p.Name, Type: providerType, BaseURL: p.BaseURL, APIKey: credential,
			Timeout: config.Duration(time.Duration(p.TimeoutSeconds) * time.Second),
		}
		if len(p.BreakerPolicy) > 0 {
			body, _ := json.Marshal(p.BreakerPolicy)
			_ = json.Unmarshal(body, &pc.Breaker)
		}
		routes.Providers = append(routes.Providers, pc)
	}
	for _, profile := range runtimeConfig.Profiles {
		profileKinds[profile.Name] = profile.Kind
		route := config.Route{Model: profile.Name, Strategy: profile.Strategy, MaxAttempts: profile.MaxAttempts}
		for _, target := range profile.Targets {
			prices[profile.Name+"\x00"+target.ProviderName+"\x00"+target.UpstreamModelID] = [2]float64{target.InputPricePerMillion, target.OutputPricePerMillion}
			route.Targets = append(route.Targets, config.Target{
				Provider: providerNames[target.ProviderConnectionID], Model: target.UpstreamModelID, Weight: target.Weight,
			})
		}
		routes.Routes = append(routes.Routes, route)
	}
	routes.ApplyDefaultsForRuntime()
	if err := routes.Validate(); err != nil {
		return nil, fmt.Errorf("project %s routing is invalid: %w", runtimeConfig.Project.Slug, err)
	}
	rtr, err := router.New(routes, m.client, m.log.With("project_id", runtimeConfig.Project.ID))
	if err != nil {
		return nil, err
	}
	return &Runtime{
		Project: runtimeConfig.Project, Policy: runtimeConfig.Policy, Router: rtr,
		Proxy:        proxy.New(rtr, m.upstream, m.log),
		prices:       prices,
		profileKinds: profileKinds,
	}, nil
}
