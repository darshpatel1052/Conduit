package controlapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/darshpatel1052/conduit/internal/analytics"
	"github.com/darshpatel1052/conduit/internal/control"
)

type API struct {
	store        *control.Store
	sessions     *control.Sessions
	secureCookie bool
	usage        *analytics.Querier
	runtime      *control.RuntimeManager
}

type identity struct {
	User    control.AdminUser
	Session control.Session
	Token   string
}

type identityKey struct{}

func New(store *control.Store, sessions *control.Sessions, secureCookie bool, usage *analytics.Querier, runtime *control.RuntimeManager) http.Handler {
	a := &API{store: store, sessions: sessions, secureCookie: secureCookie, usage: usage, runtime: runtime}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Post("/session/login", a.login)
	r.Post("/invitations/activate", a.activateInvitation)
	r.Group(func(protected chi.Router) {
		protected.Use(a.authenticate)
		protected.Get("/session/me", a.me)
		protected.Post("/session/logout", a.logout)
		protected.Post("/session/password", a.changePassword)
		protected.Post("/administrators/invitations", a.createInvitation)
		protected.Get("/projects", a.listProjects)
		protected.Post("/projects", a.createProject)
		protected.Get("/projects/{projectID}", a.getProject)
		protected.Put("/projects/{projectID}", a.updateProject)
		protected.Post("/projects/{projectID}/archive", a.archiveProject)
		protected.Get("/projects/{projectID}/members", a.listMembers)
		protected.Post("/projects/{projectID}/members", a.addMember)
		protected.Get("/projects/{projectID}/policy", a.getPolicy)
		protected.Put("/projects/{projectID}/policy", a.updatePolicy)
		protected.Get("/projects/{projectID}/providers", a.listProviders)
		protected.Post("/projects/{projectID}/providers", a.createProvider)
		protected.Post("/projects/{projectID}/providers/{providerID}/test", a.testProvider)
		protected.Put("/projects/{projectID}/providers/{providerID}/credential", a.rotateProviderCredential)
		protected.Get("/projects/{projectID}/profiles", a.listProfiles)
		protected.Post("/projects/{projectID}/profiles", a.createProfile)
		protected.Put("/projects/{projectID}/profiles/{profileID}", a.updateProfile)
		protected.Get("/projects/{projectID}/keys", a.listKeys)
		protected.Post("/projects/{projectID}/keys", a.createKey)
		protected.Post("/projects/{projectID}/keys/{keyID}/revoke", a.revokeKey)
		protected.Get("/projects/{projectID}/audit", a.listAudit)
		protected.Get("/projects/{projectID}/usage/overview", a.usageOverview)
		protected.Get("/projects/{projectID}/usage/requests", a.usageRequests)
		protected.Post("/projects/{projectID}/usage/end-users/search", a.searchEndUserUsage)
	})
	return r
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !decodeJSON(w, r, &in) {
		return
	}
	allowed, err := a.sessions.AllowLogin(r.Context(), in.Email, clientIP(r))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "login is temporarily unavailable")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "login_throttled", "too many login attempts; try again later")
		return
	}
	user, err := a.store.AuthenticateAdmin(r.Context(), in.Email, in.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	token, session, err := a.sessions.Create(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "login is temporarily unavailable")
		return
	}
	a.sessions.ClearLoginAttempts(r.Context(), in.Email, clientIP(r))
	http.SetCookie(w, &http.Cookie{Name: control.SessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: a.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int((8 * time.Hour).Seconds())})
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "csrf_token": session.CSRF})
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(control.SessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication_error", "administrator session required")
			return
		}
		session, err := a.sessions.Get(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication_error", "administrator session is invalid or expired")
			return
		}
		user, err := a.store.AdminByID(r.Context(), session.AdminID)
		if err != nil || user.Status != "active" {
			writeError(w, http.StatusUnauthorized, "authentication_error", "administrator account is unavailable")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			provided := r.Header.Get("X-CSRF-Token")
			if len(provided) != len(session.CSRF) || subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRF)) != 1 {
				writeError(w, http.StatusForbidden, "csrf_error", "valid X-CSRF-Token header required")
				return
			}
		}
		id := &identity{User: *user, Session: *session, Token: cookie.Value}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	writeJSON(w, http.StatusOK, map[string]any{"user": id.User, "csrf_token": id.Session.CSRF})
}
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	_ = a.sessions.Revoke(r.Context(), id.Token)
	http.SetCookie(w, &http.Cookie{Name: control.SessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secureCookie, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) createInvitation(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in struct {
		Email        string `json:"email"`
		ExpiresHours int    `json:"expires_hours"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.CreateInvitation(r.Context(), actor(r), id.User.PlatformOwner, in.Email, time.Duration(in.ExpiresHours)*time.Hour)
	respondStatus(w, http.StatusCreated, out, err)
}

func (a *API) activateInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.ActivateInvitation(r.Context(), in.Token, in.DisplayName, in.Password)
	respondStatus(w, http.StatusCreated, out, err)
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	err := a.store.ChangePassword(r.Context(), actor(r), in.CurrentPassword, in.NewPassword)
	respond(w, map[string]any{"ok": err == nil}, err)
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ListProjects(r.Context(), id.User.ID, id.User.PlatformOwner)
	respond(w, out, err)
}
func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct{ Slug, Name, Description string }
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.CreateProject(r.Context(), actor(r), in.Slug, in.Name, in.Description)
	respondStatus(w, http.StatusCreated, out, err)
}
func (a *API) getProject(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.GetProject(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) updateProject(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.UpdateProject(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), in.Name, in.Description)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respond(w, out, err)
}
func (a *API) archiveProject(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ArchiveProject(r.Context(), actor(r), id.User.PlatformOwner, projectID(r))
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respond(w, out, err)
}
func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ListMemberships(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) addMember(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in struct{ Email, Role string }
	if !decodeJSON(w, r, &in) {
		return
	}
	err := a.store.AddMembership(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), in.Email, in.Role)
	respondStatus(w, http.StatusCreated, map[string]any{"ok": err == nil}, err)
}
func (a *API) getPolicy(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.GetPolicy(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) updatePolicy(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in control.Policy
	if !decodeJSON(w, r, &in) {
		return
	}
	in.ProjectID = projectID(r)
	out, err := a.store.UpdatePolicy(r.Context(), actor(r), id.User.PlatformOwner, in)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respond(w, out, err)
}
func (a *API) listProviders(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ListProviders(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) createProvider(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in control.CreateProviderInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.CreateProvider(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), in)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respondStatus(w, http.StatusCreated, out, err)
}
func (a *API) testProvider(w http.ResponseWriter, r *http.Request) {
	if a.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "provider testing is unavailable")
		return
	}
	id := current(r)
	providerID := chi.URLParam(r, "providerID")
	p, err := a.store.ProviderForManagement(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), providerID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	result, err := a.runtime.TestProvider(r.Context(), *p)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err = a.store.RecordProviderTest(r.Context(), actor(r), projectID(r), providerID, result); err != nil {
		respond(w, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *API) rotateProviderCredential(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in struct {
		Credential string `json:"credential"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Credential) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "credential is required")
		return
	}
	out, err := a.store.RotateProviderCredential(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), chi.URLParam(r, "providerID"), in.Credential)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respond(w, out, err)
}
func (a *API) listProfiles(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ListProfiles(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) createProfile(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in control.CreateProfileInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.CreateProfile(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), in)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respondStatus(w, http.StatusCreated, out, err)
}
func (a *API) updateProfile(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in control.CreateProfileInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.UpdateProfile(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), chi.URLParam(r, "profileID"), in)
	if err == nil && a.runtime != nil {
		a.runtime.Invalidate(projectID(r))
	}
	respond(w, out, err)
}
func (a *API) listKeys(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	out, err := a.store.ListApplicationKeys(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r))
	respond(w, out, err)
}
func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	var in control.CreateKeyInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := a.store.CreateApplicationKey(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), in)
	respondStatus(w, http.StatusCreated, out, err)
}
func (a *API) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	err := a.store.RevokeApplicationKey(r.Context(), actor(r), id.User.PlatformOwner, projectID(r), chi.URLParam(r, "keyID"))
	respond(w, map[string]any{"ok": err == nil}, err)
}
func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	id := current(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.store.ListAudit(r.Context(), id.User.ID, id.User.PlatformOwner, projectID(r), limit)
	respond(w, out, err)
}
func (a *API) usageOverview(w http.ResponseWriter, r *http.Request) {
	if a.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics_unavailable", "analytics is unavailable")
		return
	}
	id := current(r)
	if _, err := a.store.Role(r.Context(), id.User.ID, projectID(r), id.User.PlatformOwner); err != nil {
		respond(w, nil, err)
		return
	}
	window, err := analytics.ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	out, err := a.usage.ProjectOverview(r.Context(), projectID(r), window)
	respond(w, out, err)
}
func (a *API) usageRequests(w http.ResponseWriter, r *http.Request) {
	a.projectUsageRequests(w, r, "")
}
func (a *API) searchEndUserUsage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EndUser string `json:"end_user"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.EndUser) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "end_user is required")
		return
	}
	pseudonym := a.store.EndUserPseudonym(projectID(r), in.EndUser)
	a.projectUsageRequests(w, r, pseudonym)
}
func (a *API) projectUsageRequests(w http.ResponseWriter, r *http.Request, endUserPseudonym string) {
	if a.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics_unavailable", "analytics is unavailable")
		return
	}
	id := current(r)
	if _, err := a.store.Role(r.Context(), id.User.ID, projectID(r), id.User.PlatformOwner); err != nil {
		respond(w, nil, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.usage.ProjectRecent(r.Context(), projectID(r), endUserPseudonym, limit)
	respond(w, out, err)
}
func current(r *http.Request) *identity { return r.Context().Value(identityKey{}).(*identity) }
func projectID(r *http.Request) string  { return chi.URLParam(r, "projectID") }
func actor(r *http.Request) control.Actor {
	return control.Actor{AdminID: current(r).User.ID, RequestID: middleware.GetReqID(r.Context()), SourceIP: clientIP(r)}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
func respond(w http.ResponseWriter, out any, err error) { respondStatus(w, http.StatusOK, out, err) }
func respondStatus(w http.ResponseWriter, status int, out any, err error) {
	if err == nil {
		writeJSON(w, status, out)
		return
	}
	switch {
	case errors.Is(err, control.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, control.ErrForbidden):
		writeError(w, http.StatusForbidden, "permission_error", err.Error())
	case errors.Is(err, control.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "type": code, "message": message}})
}
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return strings.Trim(host[:i], "[]")
	}
	return host
}
