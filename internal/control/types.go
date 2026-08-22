package control

import "time"

const (
	RoleProjectOwner      = "project_owner"
	RoleProjectAdmin      = "project_admin"
	RoleProjectViewer     = "project_viewer"
	ProfileKindGeneration = "generation"
	ProfileKindEmbedding  = "embedding"
)

type AdminUser struct {
	ID                     string     `json:"id"`
	Email                  string     `json:"email"`
	DisplayName            string     `json:"display_name"`
	PlatformOwner          bool       `json:"platform_owner"`
	Status                 string     `json:"status"`
	PasswordChangeRequired bool       `json:"password_change_required"`
	LastLoginAt            *time.Time `json:"last_login_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
}

type Project struct {
	ID                   string    `json:"id"`
	Slug                 string    `json:"slug"`
	Name                 string    `json:"name"`
	Description          string    `json:"description"`
	Status               string    `json:"status"`
	ConfigurationVersion int64     `json:"configuration_version"`
	Role                 string    `json:"role,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type Membership struct {
	ProjectID   string    `json:"project_id"`
	AdminID     string    `json:"admin_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

type Policy struct {
	ProjectID                   string   `json:"project_id"`
	Version                     int64    `json:"version"`
	RequestsPerMinute           int      `json:"requests_per_minute"`
	TokensPerMinute             int      `json:"tokens_per_minute"`
	EndUserRequestsPerMinute    *int     `json:"end_user_requests_per_minute"`
	MonthlyBudgetUSD            *float64 `json:"monthly_budget_usd"`
	BudgetWarningPercent        int      `json:"budget_warning_percent"`
	RequireEndUser              bool     `json:"require_end_user"`
	CacheMode                   string   `json:"cache_mode"`
	CacheTTLSeconds             int      `json:"cache_ttl_seconds"`
	SemanticThreshold           float64  `json:"semantic_threshold"`
	MaximumCacheableTemperature float64  `json:"maximum_cacheable_temperature"`
	AnalyticsRetentionDays      int      `json:"analytics_retention_days"`
}

type ProviderConnection struct {
	ID                    string         `json:"id"`
	ProjectID             string         `json:"project_id"`
	Name                  string         `json:"name"`
	Type                  string         `json:"type"`
	BaseURL               string         `json:"base_url"`
	TimeoutSeconds        int            `json:"timeout_seconds"`
	BreakerPolicy         map[string]any `json:"breaker_policy"`
	CredentialFingerprint string         `json:"credential_fingerprint"`
	HasCredential         bool           `json:"has_credential"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

type ProviderTestResult struct {
	ProviderID string `json:"provider_id"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Message    string `json:"message"`
}

type ProfileTarget struct {
	ID                    string    `json:"id,omitempty"`
	ProviderConnectionID  string    `json:"provider_connection_id"`
	ProviderName          string    `json:"provider_name,omitempty"`
	UpstreamModelID       string    `json:"upstream_model_id"`
	Priority              int       `json:"priority"`
	Weight                int       `json:"weight"`
	InputPricePerMillion  float64   `json:"input_price_per_million"`
	OutputPricePerMillion float64   `json:"output_price_per_million"`
	PricingUpdatedAt      time.Time `json:"pricing_updated_at,omitempty"`
}

type ModelProfile struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"project_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Enabled     bool            `json:"enabled"`
	Strategy    string          `json:"strategy"`
	MaxAttempts int             `json:"max_attempts"`
	Targets     []ProfileTarget `json:"targets"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type ApplicationKey struct {
	ID                string     `json:"id"`
	PublicID          string     `json:"public_id"`
	ProjectID         string     `json:"project_id"`
	Label             string     `json:"label"`
	Environment       string     `json:"environment"`
	Prefix            string     `json:"prefix"`
	AllowedProfiles   []string   `json:"allowed_profiles"`
	RequestsPerMinute *int       `json:"requests_per_minute"`
	TokensPerMinute   *int       `json:"tokens_per_minute"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
}

type CreatedKey struct {
	ApplicationKey
	Plaintext string `json:"plaintext"`
}

type CreatedInvitation struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuditEvent struct {
	ID           string         `json:"id"`
	ActorAdminID *string        `json:"actor_admin_id,omitempty"`
	ActorName    string         `json:"actor_name,omitempty"`
	ProjectID    *string        `json:"project_id,omitempty"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	Before       map[string]any `json:"before,omitempty"`
	After        map[string]any `json:"after,omitempty"`
	RequestID    string         `json:"request_id"`
	SourceIP     string         `json:"source_ip,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type Actor struct {
	AdminID   string
	RequestID string
	SourceIP  string
}

func CanManage(role string) bool { return role == RoleProjectOwner || role == RoleProjectAdmin }

func CanOwn(role string) bool { return role == RoleProjectOwner }
