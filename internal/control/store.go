package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/config"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound     = errors.New("resource not found")
	ErrForbidden    = errors.New("project access denied")
	ErrConflict     = errors.New("resource already exists")
	ErrInvalidLogin = errors.New("invalid email or password")
)

type Store struct {
	pool    *pgxpool.Pool
	secrets *Secrets
	log     *slog.Logger
}

func Open(ctx context.Context, dsn, masterKey string, log *slog.Logger) (*Store, error) {
	secrets, err := ParseMasterKey(masterKey)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse control postgres DSN: %w", err)
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open control database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping control database: %w", err)
	}
	store := &Store{pool: pool, secrets: secrets, log: log.With("component", "control_store")}
	if err := store.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply control-plane migrations: %w", err)
	}
	return nil
}

func (s *Store) BootstrapOwner(ctx context.Context, email, displayName, password string) (*AdminUser, error) {
	email = normalizeEmail(email)
	if email == "" || strings.TrimSpace(displayName) == "" {
		return nil, errors.New("email and display name are required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM admin_users WHERE platform_owner`).Scan(&count); err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, errors.New("a platform owner already exists")
	}
	var out AdminUser
	err = s.pool.QueryRow(ctx, `
		INSERT INTO admin_users(email, display_name, password_hash, platform_owner, status)
		VALUES ($1, $2, $3, TRUE, 'active')
		RETURNING id, email, display_name, platform_owner, status, password_change_required, last_login_at, created_at`,
		email, strings.TrimSpace(displayName), hash).Scan(&out.ID, &out.Email, &out.DisplayName,
		&out.PlatformOwner, &out.Status, &out.PasswordChangeRequired, &out.LastLoginAt, &out.CreatedAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	_, _ = s.pool.Exec(ctx, `INSERT INTO audit_events(actor_admin_id, action, resource_type, resource_id, after_json)
		VALUES ($1, 'admin.bootstrap', 'admin_user', $1::text, jsonb_build_object('email',$2,'display_name',$3,'platform_owner',true))`,
		out.ID, out.Email, out.DisplayName)
	return &out, nil
}

func (s *Store) AuthenticateAdmin(ctx context.Context, email, password string) (*AdminUser, error) {
	var out AdminUser
	var passwordHash string
	err := s.pool.QueryRow(ctx, `SELECT id, email, display_name, password_hash, platform_owner, status,
		password_change_required, last_login_at, created_at FROM admin_users WHERE email=$1`, normalizeEmail(email)).Scan(
		&out.ID, &out.Email, &out.DisplayName, &passwordHash, &out.PlatformOwner, &out.Status,
		&out.PasswordChangeRequired, &out.LastLoginAt, &out.CreatedAt)
	if err != nil || out.Status != "active" || !VerifyPassword(passwordHash, password) {
		return nil, ErrInvalidLogin
	}
	now := time.Now().UTC()
	_, _ = s.pool.Exec(ctx, `UPDATE admin_users SET last_login_at=$2 WHERE id=$1`, out.ID, now)
	out.LastLoginAt = &now
	return &out, nil
}

func (s *Store) AdminByID(ctx context.Context, id string) (*AdminUser, error) {
	var out AdminUser
	err := s.pool.QueryRow(ctx, `SELECT id, email, display_name, platform_owner, status,
		password_change_required, last_login_at, created_at FROM admin_users WHERE id=$1`, id).Scan(
		&out.ID, &out.Email, &out.DisplayName, &out.PlatformOwner, &out.Status,
		&out.PasswordChangeRequired, &out.LastLoginAt, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &out, err
}

func (s *Store) CreateInvitation(ctx context.Context, actor Actor, platformOwner bool, email string, ttl time.Duration) (*CreatedInvitation, error) {
	if !platformOwner {
		return nil, ErrForbidden
	}
	email = normalizeEmail(email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = 24 * time.Hour
	}
	token := randomToken(32)
	var out CreatedInvitation
	err := s.pool.QueryRow(ctx, `INSERT INTO admin_invitations(email,token_hash,invited_by,expires_at)
		VALUES($1,$2,$3,NOW()+$4::interval) RETURNING id,email,expires_at`, email, auth.HashKeyBytes(token), actor.AdminID, ttl.String()).Scan(&out.ID, &out.Email, &out.ExpiresAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	out.Token = token
	_ = s.appendAudit(ctx, actor, nil, "administrator.invite", "admin_invitation", out.ID, nil, map[string]any{"email": email, "expires_at": out.ExpiresAt})
	return &out, nil
}

func (s *Store) ActivateInvitation(ctx context.Context, token, displayName, password string) (*AdminUser, error) {
	if token == "" || strings.TrimSpace(displayName) == "" {
		return nil, errors.New("token, display name, and password are required")
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var invitationID, email string
	var stored []byte
	var expires time.Time
	var used *time.Time
	err = tx.QueryRow(ctx, `SELECT id,email,token_hash,expires_at,used_at FROM admin_invitations WHERE token_hash=$1 FOR UPDATE`, auth.HashKeyBytes(token)).Scan(&invitationID, &email, &stored, &expires, &used)
	if err != nil || used != nil || expires.Before(time.Now()) || subtle.ConstantTimeCompare(stored, auth.HashKeyBytes(token)) != 1 {
		return nil, errors.New("invitation is invalid, expired, or already used")
	}
	var out AdminUser
	err = tx.QueryRow(ctx, `INSERT INTO admin_users(email,display_name,password_hash,status) VALUES($1,$2,$3,'active')
		ON CONFLICT(email) DO UPDATE SET display_name=EXCLUDED.display_name,password_hash=EXCLUDED.password_hash,status='active',updated_at=NOW()
		RETURNING id,email,display_name,platform_owner,status,password_change_required,last_login_at,created_at`, email, strings.TrimSpace(displayName), passwordHash).Scan(&out.ID, &out.Email, &out.DisplayName, &out.PlatformOwner, &out.Status, &out.PasswordChangeRequired, &out.LastLoginAt, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_invitations SET used_at=NOW() WHERE id=$1`, invitationID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, Actor{AdminID: out.ID}, nil, "administrator.activate", "admin_user", out.ID, nil, map[string]any{"email": out.Email, "display_name": out.DisplayName}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ChangePassword(ctx context.Context, actor Actor, currentPassword, newPassword string) error {
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM admin_users WHERE id=$1 AND status='active'`, actor.AdminID).Scan(&stored); err != nil {
		return ErrNotFound
	}
	if !VerifyPassword(stored, currentPassword) {
		return ErrInvalidLogin
	}
	next, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `UPDATE admin_users SET password_hash=$2,password_change_required=FALSE,updated_at=NOW() WHERE id=$1`, actor.AdminID, next); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, actor, nil, "administrator.password_change", "admin_user", actor.AdminID, nil, map[string]any{"password_changed": true}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateProject(ctx context.Context, actor Actor, slug, name, description string) (*Project, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	name = strings.TrimSpace(name)
	if !validSlug(slug) || name == "" {
		return nil, errors.New("slug must contain lowercase letters, digits, or hyphens, and name is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out Project
	err = tx.QueryRow(ctx, `INSERT INTO projects(slug,name,description,created_by) VALUES($1,$2,$3,$4)
		RETURNING id,slug,name,description,status,configuration_version,created_at,updated_at`,
		slug, name, strings.TrimSpace(description), actor.AdminID).Scan(&out.ID, &out.Slug, &out.Name,
		&out.Description, &out.Status, &out.ConfigurationVersion, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_memberships(project_id,admin_id,role,created_by)
		VALUES($1,$2,'project_owner',$2)`, out.ID, actor.AdminID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_policies(project_id,updated_by) VALUES($1,$2)`, out.ID, actor.AdminID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &out.ID, "project.create", "project", out.ID, nil, map[string]any{
		"slug": out.Slug, "name": out.Name, "description": out.Description,
	}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	out.Role = RoleProjectOwner
	return &out, nil
}

func (s *Store) ListProjects(ctx context.Context, adminID string, platformOwner bool) ([]Project, error) {
	query := `SELECT p.id,p.slug,p.name,p.description,p.status,p.configuration_version,p.created_at,p.updated_at,
		COALESCE(pm.role,'platform_owner') FROM projects p LEFT JOIN project_memberships pm ON pm.project_id=p.id AND pm.admin_id=$1`
	if !platformOwner {
		query += ` WHERE pm.admin_id IS NOT NULL`
	}
	query += ` ORDER BY p.name`
	rows, err := s.pool.Query(ctx, query, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.Status, &p.ConfigurationVersion, &p.CreatedAt, &p.UpdatedAt, &p.Role); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, adminID string, platformOwner bool, projectID string) (*Project, error) {
	role, err := s.Role(ctx, adminID, projectID, platformOwner)
	if err != nil {
		return nil, err
	}
	var out Project
	err = s.pool.QueryRow(ctx, `SELECT id,slug,name,description,status,configuration_version,created_at,updated_at
		FROM projects WHERE id=$1`, projectID).Scan(&out.ID, &out.Slug, &out.Name, &out.Description,
		&out.Status, &out.ConfigurationVersion, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	out.Role = role
	return &out, err
}

func (s *Store) UpdateProject(ctx context.Context, actor Actor, platformOwner bool, projectID, name, description string) (*Project, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanOwn(role)) {
		return nil, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("project name is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before, out Project
	err = tx.QueryRow(ctx, `SELECT id,slug,name,description,status,configuration_version,created_at,updated_at
		FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&before.ID, &before.Slug, &before.Name, &before.Description,
		&before.Status, &before.ConfigurationVersion, &before.CreatedAt, &before.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `UPDATE projects SET name=$2,description=$3,configuration_version=configuration_version+1,
		updated_at=NOW() WHERE id=$1 RETURNING id,slug,name,description,status,configuration_version,created_at,updated_at`,
		projectID, name, strings.TrimSpace(description)).Scan(&out.ID, &out.Slug, &out.Name, &out.Description,
		&out.Status, &out.ConfigurationVersion, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "project.update", "project", projectID,
		map[string]any{"name": before.Name, "description": before.Description}, map[string]any{"name": out.Name, "description": out.Description}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	out.Role = role
	return &out, nil
}

func (s *Store) ArchiveProject(ctx context.Context, actor Actor, platformOwner bool, projectID string) (*Project, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanOwn(role)) {
		return nil, ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out Project
	err = tx.QueryRow(ctx, `UPDATE projects SET status='archived',configuration_version=configuration_version+1,
		updated_at=NOW() WHERE id=$1 AND status='active'
		RETURNING id,slug,name,description,status,configuration_version,created_at,updated_at`, projectID).Scan(
		&out.ID, &out.Slug, &out.Name, &out.Description, &out.Status, &out.ConfigurationVersion, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: project is already archived", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "project.archive", "project", projectID,
		map[string]any{"status": "active"}, map[string]any{"status": "archived"}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	out.Role = role
	return &out, nil
}

func (s *Store) Role(ctx context.Context, adminID, projectID string, platformOwner bool) (string, error) {
	if platformOwner {
		return "platform_owner", nil
	}
	var role string
	err := s.pool.QueryRow(ctx, `SELECT role FROM project_memberships WHERE project_id=$1 AND admin_id=$2`, projectID, adminID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrForbidden
	}
	return role, err
}

func (s *Store) AddMembership(ctx context.Context, actor Actor, platformOwner bool, projectID, adminEmail, role string) error {
	actorRole, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanOwn(actorRole)) {
		return ErrForbidden
	}
	if role != RoleProjectOwner && role != RoleProjectAdmin && role != RoleProjectViewer {
		return errors.New("invalid project role")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var adminID string
	if err = tx.QueryRow(ctx, `SELECT id FROM admin_users WHERE email=$1 AND status='active'`, normalizeEmail(adminEmail)).Scan(&adminID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO project_memberships(project_id,admin_id,role,created_by) VALUES($1,$2,$3,$4)
		ON CONFLICT(project_id,admin_id) DO UPDATE SET role=EXCLUDED.role`, projectID, adminID, role, actor.AdminID)
	if err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "membership.upsert", "project_membership", adminID, nil, map[string]any{"email": normalizeEmail(adminEmail), "role": role}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListMemberships(ctx context.Context, adminID string, platformOwner bool, projectID string) ([]Membership, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT pm.project_id,pm.admin_id,a.email,a.display_name,pm.role,pm.created_at
		FROM project_memberships pm JOIN admin_users a ON a.id=pm.admin_id WHERE pm.project_id=$1 ORDER BY a.email`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.AdminID, &m.Email, &m.DisplayName, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetPolicy(ctx context.Context, adminID string, platformOwner bool, projectID string) (*Policy, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	return s.loadPolicy(ctx, projectID)
}

func (s *Store) loadPolicy(ctx context.Context, projectID string) (*Policy, error) {
	var p Policy
	err := s.pool.QueryRow(ctx, `SELECT project_id,version,requests_per_minute,tokens_per_minute,end_user_requests_per_minute,
		monthly_budget_usd::float8,budget_warning_percent,require_end_user,cache_mode,cache_ttl_seconds,semantic_threshold,
		maximum_cacheable_temperature,analytics_retention_days FROM project_policies WHERE project_id=$1`, projectID).Scan(
		&p.ProjectID, &p.Version, &p.RequestsPerMinute, &p.TokensPerMinute, &p.EndUserRequestsPerMinute, &p.MonthlyBudgetUSD,
		&p.BudgetWarningPercent, &p.RequireEndUser, &p.CacheMode, &p.CacheTTLSeconds, &p.SemanticThreshold, &p.MaximumCacheableTemperature, &p.AnalyticsRetentionDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

func (s *Store) UpdatePolicy(ctx context.Context, actor Actor, platformOwner bool, p Policy) (*Policy, error) {
	role, err := s.Role(ctx, actor.AdminID, p.ProjectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	if p.RequestsPerMinute <= 0 || p.TokensPerMinute <= 0 || p.BudgetWarningPercent < 1 || p.BudgetWarningPercent > 100 ||
		(p.CacheMode != "disabled" && p.CacheMode != "exact" && p.CacheMode != "semantic") || (p.AnalyticsRetentionDays != 7 && p.AnalyticsRetentionDays != 30 && p.AnalyticsRetentionDays != 90) {
		return nil, errors.New("invalid project policy")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := s.loadPolicy(ctx, p.ProjectID)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE project_policies SET version=version+1,requests_per_minute=$2,tokens_per_minute=$3,
		end_user_requests_per_minute=$4,monthly_budget_usd=$5,budget_warning_percent=$6,require_end_user=$7,cache_mode=$8,
		cache_ttl_seconds=$9,semantic_threshold=$10,maximum_cacheable_temperature=$11,analytics_retention_days=$12,updated_by=$13,updated_at=NOW()
		WHERE project_id=$1`, p.ProjectID, p.RequestsPerMinute, p.TokensPerMinute, p.EndUserRequestsPerMinute, p.MonthlyBudgetUSD, p.BudgetWarningPercent,
		p.RequireEndUser, p.CacheMode, p.CacheTTLSeconds, p.SemanticThreshold, p.MaximumCacheableTemperature, p.AnalyticsRetentionDays, actor.AdminID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET configuration_version=configuration_version+1,updated_at=NOW() WHERE id=$1`, p.ProjectID); err != nil {
		return nil, err
	}
	p.Version = before.Version + 1
	if err = insertAudit(ctx, tx, actor, &p.ProjectID, "policy.update", "project_policy", p.ProjectID, before, p); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &p, nil
}

type CreateProviderInput struct {
	Name           string         `json:"name"`
	Type           string         `json:"type"`
	BaseURL        string         `json:"base_url"`
	Credential     string         `json:"credential"`
	TimeoutSeconds int            `json:"timeout_seconds"`
	BreakerPolicy  map[string]any `json:"breaker_policy"`
}

func (s *Store) CreateProvider(ctx context.Context, actor Actor, platformOwner bool, projectID string, in CreateProviderInput) (*ProviderConnection, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	if err := ValidateProviderURL(in.Type, in.BaseURL); err != nil {
		return nil, err
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 120
	}
	id := uuid.NewString()
	ciphertext, err := s.secrets.Encrypt(projectID, id, in.Credential)
	if err != nil {
		return nil, err
	}
	breaker, _ := json.Marshal(in.BreakerPolicy)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out ProviderConnection
	var hasCredential bool
	err = tx.QueryRow(ctx, `INSERT INTO provider_connections(id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,
		encrypted_credential,credential_fingerprint,credential_rotated_at,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,CASE WHEN $8::bytea IS NULL THEN NULL ELSE NOW() END,$10)
		RETURNING id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,credential_fingerprint,
		(encrypted_credential IS NOT NULL),created_at,updated_at`, id, projectID, strings.TrimSpace(in.Name), in.Type, strings.TrimRight(in.BaseURL, "/"),
		in.TimeoutSeconds, breaker, ciphertext, CredentialFingerprint(in.Credential), actor.AdminID).Scan(&out.ID, &out.ProjectID, &out.Name, &out.Type, &out.BaseURL,
		&out.TimeoutSeconds, &out.BreakerPolicy, &out.CredentialFingerprint, &hasCredential, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	out.HasCredential = hasCredential
	if _, err = tx.Exec(ctx, `UPDATE projects SET configuration_version=configuration_version+1,updated_at=NOW() WHERE id=$1`, projectID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "provider.create", "provider_connection", out.ID, nil, map[string]any{"name": out.Name, "type": out.Type, "base_url": out.BaseURL, "credential_fingerprint": out.CredentialFingerprint}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ListProviders(ctx context.Context, adminID string, platformOwner bool, projectID string) ([]ProviderConnection, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,credential_fingerprint,
		(encrypted_credential IS NOT NULL),created_at,updated_at FROM provider_connections WHERE project_id=$1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderConnection{}
	for rows.Next() {
		var p ProviderConnection
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Name, &p.Type, &p.BaseURL, &p.TimeoutSeconds, &p.BreakerPolicy, &p.CredentialFingerprint, &p.HasCredential, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProviderForManagement(ctx context.Context, actor Actor, platformOwner bool, projectID, providerID string) (*RuntimeProvider, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	var p RuntimeProvider
	err = s.pool.QueryRow(ctx, `SELECT id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,
		credential_fingerprint,(encrypted_credential IS NOT NULL),encrypted_credential,created_at,updated_at
		FROM provider_connections WHERE id=$1 AND project_id=$2`, providerID, projectID).Scan(
		&p.ID, &p.ProjectID, &p.Name, &p.Type, &p.BaseURL, &p.TimeoutSeconds, &p.BreakerPolicy,
		&p.CredentialFingerprint, &p.HasCredential, &p.EncryptedCredential, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

func (s *Store) RotateProviderCredential(ctx context.Context, actor Actor, platformOwner bool, projectID, providerID, credential string) (*ProviderConnection, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	ciphertext, err := s.secrets.Encrypt(projectID, providerID, credential)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out ProviderConnection
	var previousFingerprint string
	err = tx.QueryRow(ctx, `SELECT credential_fingerprint FROM provider_connections WHERE id=$1 AND project_id=$2 FOR UPDATE`, providerID, projectID).Scan(&previousFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `UPDATE provider_connections SET encrypted_credential=$3,credential_fingerprint=$4,
		credential_rotated_at=NOW(),updated_at=NOW() WHERE id=$1 AND project_id=$2
		RETURNING id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,
		credential_fingerprint,(encrypted_credential IS NOT NULL),created_at,updated_at`,
		providerID, projectID, ciphertext, CredentialFingerprint(credential)).Scan(&out.ID, &out.ProjectID, &out.Name,
		&out.Type, &out.BaseURL, &out.TimeoutSeconds, &out.BreakerPolicy, &out.CredentialFingerprint,
		&out.HasCredential, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET configuration_version=configuration_version+1,updated_at=NOW() WHERE id=$1`, projectID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "provider.credential_rotate", "provider_connection", providerID,
		map[string]any{"credential_fingerprint": previousFingerprint}, map[string]any{"credential_fingerprint": out.CredentialFingerprint}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) RecordProviderTest(ctx context.Context, actor Actor, projectID, providerID string, result ProviderTestResult) error {
	return s.appendAudit(ctx, actor, &projectID, "provider.test", "provider_connection", providerID, nil,
		map[string]any{"ok": result.OK, "status_code": result.StatusCode, "latency_ms": result.LatencyMS, "message": result.Message})
}

type CreateProfileInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Strategy    string          `json:"strategy"`
	Enabled     bool            `json:"enabled"`
	MaxAttempts int             `json:"max_attempts"`
	Targets     []ProfileTarget `json:"targets"`
}

func (s *Store) CreateProfile(ctx context.Context, actor Actor, platformOwner bool, projectID string, in CreateProfileInput) (*ModelProfile, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Targets) == 0 {
		return nil, errors.New("profile name and at least one target are required")
	}
	if in.Strategy == "" {
		in.Strategy = "priority"
	}
	if in.Kind == "" {
		in.Kind = ProfileKindGeneration
	}
	if in.Kind != ProfileKindGeneration && in.Kind != ProfileKindEmbedding {
		return nil, errors.New("profile kind must be generation or embedding")
	}
	if err := s.validateProfileTargets(ctx, projectID, in.Kind, in.Targets); err != nil {
		return nil, err
	}
	if in.Strategy != "priority" && in.Strategy != "round_robin" && in.Strategy != "weighted" {
		return nil, errors.New("invalid routing strategy")
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = len(in.Targets)
	}
	if !in.Enabled {
		in.Enabled = true
	}
	sort.Slice(in.Targets, func(i, j int) bool { return in.Targets[i].Priority < in.Targets[j].Priority })
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out ModelProfile
	err = tx.QueryRow(ctx, `INSERT INTO model_profiles(project_id,name,description,kind,enabled,strategy,max_attempts,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id,project_id,name,description,kind,enabled,strategy,max_attempts,created_at,updated_at`, projectID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), in.Kind, in.Enabled, in.Strategy, in.MaxAttempts, actor.AdminID).Scan(
		&out.ID, &out.ProjectID, &out.Name, &out.Description, &out.Kind, &out.Enabled, &out.Strategy, &out.MaxAttempts, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	for i, t := range in.Targets {
		if t.Priority == 0 {
			t.Priority = i + 1
		}
		if t.Weight == 0 {
			t.Weight = 1
		}
		var target ProfileTarget
		err = tx.QueryRow(ctx, `INSERT INTO model_profile_targets(project_id,profile_id,provider_connection_id,upstream_model_id,priority,weight,input_price_per_million,output_price_per_million)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,provider_connection_id,upstream_model_id,priority,weight,input_price_per_million::float8,output_price_per_million::float8,pricing_updated_at`,
			projectID, out.ID, t.ProviderConnectionID, t.UpstreamModelID, t.Priority, t.Weight, t.InputPricePerMillion, t.OutputPricePerMillion).Scan(&target.ID, &target.ProviderConnectionID, &target.UpstreamModelID, &target.Priority, &target.Weight, &target.InputPricePerMillion, &target.OutputPricePerMillion, &target.PricingUpdatedAt)
		if err != nil {
			return nil, mapPGError(err)
		}
		out.Targets = append(out.Targets, target)
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET configuration_version=configuration_version+1,updated_at=NOW() WHERE id=$1`, projectID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "profile.create", "model_profile", out.ID, nil, out); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) UpdateProfile(ctx context.Context, actor Actor, platformOwner bool, projectID, profileID string, in CreateProfileInput) (*ModelProfile, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Targets) == 0 {
		return nil, errors.New("profile name and at least one target are required")
	}
	if in.Strategy == "" {
		in.Strategy = "priority"
	}
	if in.Kind == "" {
		in.Kind = ProfileKindGeneration
	}
	if in.Kind != ProfileKindGeneration && in.Kind != ProfileKindEmbedding {
		return nil, errors.New("profile kind must be generation or embedding")
	}
	if err := s.validateProfileTargets(ctx, projectID, in.Kind, in.Targets); err != nil {
		return nil, err
	}
	if in.Strategy != "priority" && in.Strategy != "round_robin" && in.Strategy != "weighted" {
		return nil, errors.New("invalid routing strategy")
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = len(in.Targets)
	}
	profiles, err := s.loadProfiles(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	var before *ModelProfile
	for i := range profiles {
		if profiles[i].ID == profileID {
			copy := profiles[i]
			before = &copy
			break
		}
	}
	if before == nil {
		return nil, ErrNotFound
	}
	sort.Slice(in.Targets, func(i, j int) bool { return in.Targets[i].Priority < in.Targets[j].Priority })
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out ModelProfile
	err = tx.QueryRow(ctx, `UPDATE model_profiles SET name=$3,description=$4,kind=$5,enabled=$6,strategy=$7,max_attempts=$8,updated_at=NOW() WHERE id=$1 AND project_id=$2
		RETURNING id,project_id,name,description,kind,enabled,strategy,max_attempts,created_at,updated_at`, profileID, projectID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), in.Kind, in.Enabled, in.Strategy, in.MaxAttempts).Scan(&out.ID, &out.ProjectID, &out.Name, &out.Description, &out.Kind, &out.Enabled, &out.Strategy, &out.MaxAttempts, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, mapPGError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM model_profile_targets WHERE profile_id=$1`, profileID); err != nil {
		return nil, err
	}
	for i, t := range in.Targets {
		if t.Priority == 0 {
			t.Priority = i + 1
		}
		if t.Weight == 0 {
			t.Weight = 1
		}
		var target ProfileTarget
		err = tx.QueryRow(ctx, `INSERT INTO model_profile_targets(project_id,profile_id,provider_connection_id,upstream_model_id,priority,weight,input_price_per_million,output_price_per_million)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,provider_connection_id,upstream_model_id,priority,weight,input_price_per_million::float8,output_price_per_million::float8,pricing_updated_at`, projectID, profileID, t.ProviderConnectionID, t.UpstreamModelID, t.Priority, t.Weight, t.InputPricePerMillion, t.OutputPricePerMillion).Scan(&target.ID, &target.ProviderConnectionID, &target.UpstreamModelID, &target.Priority, &target.Weight, &target.InputPricePerMillion, &target.OutputPricePerMillion, &target.PricingUpdatedAt)
		if err != nil {
			return nil, mapPGError(err)
		}
		out.Targets = append(out.Targets, target)
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET configuration_version=configuration_version+1,updated_at=NOW() WHERE id=$1`, projectID); err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "profile.update", "model_profile", profileID, before, out); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ListProfiles(ctx context.Context, adminID string, platformOwner bool, projectID string) ([]ModelProfile, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	return s.loadProfiles(ctx, projectID, false)
}

func (s *Store) loadProfiles(ctx context.Context, projectID string, enabledOnly bool) ([]ModelProfile, error) {
	q := `SELECT id,project_id,name,description,kind,enabled,strategy,max_attempts,created_at,updated_at FROM model_profiles WHERE project_id=$1`
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY name`
	rows, err := s.pool.Query(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelProfile{}
	for rows.Next() {
		var p ModelProfile
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Name, &p.Description, &p.Kind, &p.Enabled, &p.Strategy, &p.MaxAttempts, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		tr, err := s.pool.Query(ctx, `SELECT t.id,t.provider_connection_id,pc.name,t.upstream_model_id,t.priority,t.weight,t.input_price_per_million::float8,t.output_price_per_million::float8,t.pricing_updated_at
			FROM model_profile_targets t JOIN provider_connections pc ON pc.id=t.provider_connection_id WHERE t.profile_id=$1 ORDER BY t.priority`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for tr.Next() {
			var t ProfileTarget
			if err := tr.Scan(&t.ID, &t.ProviderConnectionID, &t.ProviderName, &t.UpstreamModelID, &t.Priority, &t.Weight, &t.InputPricePerMillion, &t.OutputPricePerMillion, &t.PricingUpdatedAt); err != nil {
				tr.Close()
				return nil, err
			}
			out[i].Targets = append(out[i].Targets, t)
		}
		tr.Close()
	}
	return out, nil
}

func (s *Store) validateProfileTargets(ctx context.Context, projectID, kind string, targets []ProfileTarget) error {
	if kind != ProfileKindEmbedding {
		return nil
	}
	for _, target := range targets {
		var providerType string
		err := s.pool.QueryRow(ctx, `SELECT provider_type FROM provider_connections WHERE id=$1 AND project_id=$2`, target.ProviderConnectionID, projectID).Scan(&providerType)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("embedding profile target does not belong to this project")
		}
		if err != nil {
			return err
		}
		if providerType == config.TypeAnthropic {
			return errors.New("Anthropic connections cannot be used in embedding profiles")
		}
	}
	return nil
}

type CreateKeyInput struct {
	Label             string     `json:"label"`
	Environment       string     `json:"environment"`
	AllowedProfiles   []string   `json:"allowed_profiles"`
	RequestsPerMinute *int       `json:"requests_per_minute"`
	TokensPerMinute   *int       `json:"tokens_per_minute"`
	ExpiresAt         *time.Time `json:"expires_at"`
}

func (s *Store) CreateApplicationKey(ctx context.Context, actor Actor, platformOwner bool, projectID string, in CreateKeyInput) (*CreatedKey, error) {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return nil, ErrForbidden
	}
	if strings.TrimSpace(in.Label) == "" {
		return nil, errors.New("key label is required")
	}
	if in.Environment == "" {
		in.Environment = "development"
	}
	publicID := strings.ToLower(randomHex(6))
	secret := randomHex(32)
	plaintext := "ck_" + publicID + "_" + secret
	hash := auth.HashKeyBytes(plaintext)
	prefix := "ck_" + publicID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out CreatedKey
	err = tx.QueryRow(ctx, `INSERT INTO application_keys(public_id,project_id,label,environment,key_hash,prefix,allowed_profiles,requests_per_minute,tokens_per_minute,expires_at,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id,public_id,project_id,label,environment,prefix,allowed_profiles,requests_per_minute,tokens_per_minute,expires_at,revoked_at,created_at,last_used_at`,
		publicID, projectID, strings.TrimSpace(in.Label), in.Environment, hash, prefix, in.AllowedProfiles, in.RequestsPerMinute, in.TokensPerMinute, in.ExpiresAt, actor.AdminID).Scan(
		&out.ID, &out.PublicID, &out.ProjectID, &out.Label, &out.Environment, &out.Prefix, &out.AllowedProfiles, &out.RequestsPerMinute, &out.TokensPerMinute, &out.ExpiresAt, &out.RevokedAt, &out.CreatedAt, &out.LastUsedAt)
	if err != nil {
		return nil, mapPGError(err)
	}
	out.Plaintext = plaintext
	if err = insertAudit(ctx, tx, actor, &projectID, "api_key.create", "application_key", out.ID, nil, map[string]any{"label": out.Label, "environment": out.Environment, "prefix": out.Prefix, "allowed_profiles": out.AllowedProfiles}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ListApplicationKeys(ctx context.Context, adminID string, platformOwner bool, projectID string) ([]ApplicationKey, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,public_id,project_id,label,environment,prefix,allowed_profiles,requests_per_minute,tokens_per_minute,expires_at,revoked_at,created_at,last_used_at
		FROM application_keys WHERE project_id=$1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApplicationKey{}
	for rows.Next() {
		var k ApplicationKey
		if err := rows.Scan(&k.ID, &k.PublicID, &k.ProjectID, &k.Label, &k.Environment, &k.Prefix, &k.AllowedProfiles, &k.RequestsPerMinute, &k.TokensPerMinute, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeApplicationKey(ctx context.Context, actor Actor, platformOwner bool, projectID, keyID string) error {
	role, err := s.Role(ctx, actor.AdminID, projectID, platformOwner)
	if err != nil || (!platformOwner && !CanManage(role)) {
		return ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cmd, err := tx.Exec(ctx, `UPDATE application_keys SET revoked_at=NOW() WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL`, keyID, projectID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = insertAudit(ctx, tx, actor, &projectID, "api_key.revoke", "application_key", keyID, nil, map[string]any{"revoked": true}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ResolveAPIKey(ctx context.Context, token, sourceIP string) (*auth.Principal, error) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != "ck" || len(parts[1]) != 12 {
		return nil, auth.ErrUnauthorized
	}
	var keyID, projectID, label, environment string
	var stored []byte
	var profiles []string
	var keyRPM, keyTPM *int
	var expires, revoked *time.Time
	err := s.pool.QueryRow(ctx, `SELECT k.id,k.project_id,k.label,k.environment,k.key_hash,k.allowed_profiles,k.requests_per_minute,k.tokens_per_minute,k.expires_at,k.revoked_at
		FROM application_keys k JOIN projects p ON p.id=k.project_id WHERE k.public_id=$1 AND p.status='active'`, parts[1]).Scan(&keyID, &projectID, &label, &environment, &stored, &profiles, &keyRPM, &keyTPM, &expires, &revoked)
	if err != nil || revoked != nil || (expires != nil && expires.Before(time.Now())) || !auth.CompareKeyHash(token, stored) {
		return nil, auth.ErrUnauthorized
	}
	policy, err := s.loadPolicy(ctx, projectID)
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	rpm, tpm := policy.RequestsPerMinute, policy.TokensPerMinute
	if keyRPM != nil {
		rpm = *keyRPM
	}
	if keyTPM != nil {
		tpm = *keyTPM
	}
	p := &auth.Principal{ID: keyID, KeyID: keyID, ProjectID: projectID, Name: label, Environment: environment, Models: profiles, RequestsPerMinute: rpm, TokensPerMinute: tpm,
		ProjectRequestsPerMinute: policy.RequestsPerMinute, ProjectTokensPerMinute: policy.TokensPerMinute, EndUserRequestsPerMinute: policy.EndUserRequestsPerMinute,
		RequireEndUser: policy.RequireEndUser, CacheMode: policy.CacheMode, PolicyVersion: policy.Version, MonthlyBudgetUSD: policy.MonthlyBudgetUSD, BudgetWarningPercent: policy.BudgetWarningPercent,
		CacheSemanticThreshold: policy.SemanticThreshold, CacheMaxTemperature: policy.MaximumCacheableTemperature, CacheTTLSeconds: policy.CacheTTLSeconds}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(ctx, `UPDATE application_keys SET last_used_at=NOW(),last_used_ip=NULLIF($2,'')::inet WHERE id=$1`, keyID, sourceIP)
	}()
	return p, nil
}

type RuntimeProvider struct {
	ProviderConnection
	EncryptedCredential []byte
}
type RuntimeConfig struct {
	Project   Project
	Policy    Policy
	Providers []RuntimeProvider
	Profiles  []ModelProfile
}

func (s *Store) RuntimeConfig(ctx context.Context, projectID string) (*RuntimeConfig, error) {
	var out RuntimeConfig
	err := s.pool.QueryRow(ctx, `SELECT id,slug,name,description,status,configuration_version,created_at,updated_at FROM projects WHERE id=$1 AND status='active'`, projectID).Scan(&out.Project.ID, &out.Project.Slug, &out.Project.Name, &out.Project.Description, &out.Project.Status, &out.Project.ConfigurationVersion, &out.Project.CreatedAt, &out.Project.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p, err := s.loadPolicy(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out.Policy = *p
	rows, err := s.pool.Query(ctx, `SELECT id,project_id,name,provider_type,base_url,timeout_seconds,breaker_policy,credential_fingerprint,(encrypted_credential IS NOT NULL),encrypted_credential,created_at,updated_at FROM provider_connections WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p RuntimeProvider
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Name, &p.Type, &p.BaseURL, &p.TimeoutSeconds, &p.BreakerPolicy, &p.CredentialFingerprint, &p.HasCredential, &p.EncryptedCredential, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out.Providers = append(out.Providers, p)
	}
	out.Profiles, err = s.loadProfiles(ctx, projectID, true)
	return &out, err
}

func (s *Store) DecryptProviderCredential(p RuntimeProvider) (string, error) {
	return s.secrets.Decrypt(p.ProjectID, p.ID, p.EncryptedCredential)
}
func (s *Store) EndUserPseudonym(projectID, raw string) string {
	return s.secrets.EndUserPseudonym(projectID, raw)
}

func (s *Store) ListAudit(ctx context.Context, adminID string, platformOwner bool, projectID string, limit int) ([]AuditEvent, error) {
	if _, err := s.Role(ctx, adminID, projectID, platformOwner); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.actor_admin_id,COALESCE(a.display_name,''),e.project_id,e.action,e.resource_type,e.resource_id,e.before_json,e.after_json,e.request_id,COALESCE(host(e.source_ip),''),e.created_at
		FROM audit_events e LEFT JOIN admin_users a ON a.id=e.actor_admin_id WHERE e.project_id=$1 ORDER BY e.created_at DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.ActorAdminID, &e.ActorName, &e.ProjectID, &e.Action, &e.ResourceType, &e.ResourceID, &e.Before, &e.After, &e.RequestID, &e.SourceIP, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) appendAudit(ctx context.Context, actor Actor, projectID *string, action, resourceType, resourceID string, before, after any) error {
	return insertAudit(ctx, s.pool, actor, projectID, action, resourceType, resourceID, before, after)
}

func insertAudit(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, actor Actor, projectID *string, action, resourceType, resourceID string, before, after any) error {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	_, err := q.Exec(ctx, `INSERT INTO audit_events(actor_admin_id,project_id,action,resource_type,resource_id,before_json,after_json,request_id,source_ip)
		VALUES(NULLIF($1,'')::uuid,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::inet)`, actor.AdminID, projectID, action, resourceType, resourceID, b, a, actor.RequestID, actor.SourceIP)
	return err
}

func ValidateProviderURL(providerType, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return errors.New("provider base_url must be an absolute URL")
	}
	if u.User != nil {
		return errors.New("provider URL must not contain credentials")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("provider URL scheme must be http or https")
	}
	if u.Scheme == "http" && !isLocalHost(u.Hostname()) {
		return errors.New("unencrypted provider URLs are allowed only for local or private-network hosts")
	}
	return nil
}
func isLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return true
	}
	if !strings.Contains(host, ".") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func validSlug(s string) bool {
	if len(s) < 2 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
func mapPGError(err error) error {
	if strings.Contains(err.Error(), "duplicate key") {
		return ErrConflict
	}
	return err
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
