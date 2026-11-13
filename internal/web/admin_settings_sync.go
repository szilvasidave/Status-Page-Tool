package web

// Settings page section "Users and groups from Microsoft Entra ID (SCIM
// provisioning)": the Tenant URL and the token for Microsoft Entra ID (see
// internal/scim). The token is generated here and shown once; only its
// SHA-256 is stored. Post/Redirect/Get: the POST keeps the new token in
// memory (newTokens) and redirects to the settings page with the id of that
// slot, which shows the token and forgets it, so that reloading the page
// neither shows the token again nor generates another one. The token itself
// is never put into a URL, a cookie or the log.
// Template: templates/partials/settings_sync.html.

import (
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/scim"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminSyncData is the view data of the provisioning section.
type adminSyncData struct {
	TenantURL    string    // <Base URL>/scim/v2
	Enabled      bool      // a token exists: provisioning is on
	TokenCreated time.Time // when the token was generated
	NewToken     string    // a token generated just now (shown once)
	Replaced     bool      // NewToken replaced a previous token
}

func (s *Server) adminSyncView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.Sync.TenantURL = scim.EndpointURL(r, d.St.BaseURL)
	d.Sync.Enabled = cur.SCIMEnabled()
	d.Sync.TokenCreated = cur.SCIMTokenCreatedAt
	if id := r.URL.Query().Get(newTokenParam); id != "" {
		if t, ok := newTokens.take(id, tokenOwner(r)); ok {
			d.Sync.NewToken, d.Sync.Replaced = t.token, t.replaced
		}
	}
}

// adminSyncSave: the section has no fields in the settings form; the token
// has its own buttons (handleSCIMTokenNew, handleSCIMTokenDelete).
func (s *Server) adminSyncSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {}

// handleSCIMTokenNew generates a provisioning token (replacing the previous
// one) and redirects to the settings page, which shows it once.
func (s *Server) handleSCIMTokenNew(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	before, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	token, hash := scim.NewToken()
	if err := s.Store.SetSCIMToken(ctx, hash, time.Now()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("SCIM provisioning token generated", "replaced", before.SCIMEnabled(), "user", adminActor(r))
	id := newTokens.put(tokenSlot{token: token, replaced: before.SCIMEnabled(), userID: tokenOwner(r)})
	http.Redirect(w, r, "/admin/settings?"+newTokenParam+"="+id+"#provisioning", http.StatusSeeOther)
}

// handleSCIMTokenDelete turns provisioning off: the token is deleted and
// sign-ins manage the groups again.
func (s *Server) handleSCIMTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetSCIMToken(r.Context(), "", time.Time{}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("SCIM provisioning turned off", "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/settings", "success",
		"Provisioning has been turned off. Microsoft Entra ID can no longer change users and groups; sign-ins update the group memberships again.")
}

// newTokenParam is the query parameter of the settings page with the id of a
// slot in newTokens (never the token itself).
const newTokenParam = "new-token"

// newTokenTTL is how long a generated token waits for the settings page.
const newTokenTTL = 5 * time.Minute

// newTokens holds generated tokens until the settings page shows them: each
// one is shown once, only to the administrator who generated it, and only
// within newTokenTTL.
var newTokens = &tokenSlots{slots: map[string]tokenSlot{}}

type tokenSlots struct {
	mu    sync.Mutex
	slots map[string]tokenSlot // by random id
}

type tokenSlot struct {
	token    string
	replaced bool  // the token replaced a previous one
	userID   int64 // the administrator who generated it
	expires  time.Time
}

// put keeps t and returns the id of its slot; expired slots are dropped.
func (s *tokenSlots) put(t tokenSlot) string {
	id := secret.RandomToken(16)
	now := time.Now()
	t.expires = now.Add(newTokenTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.DeleteFunc(s.slots, func(_ string, old tokenSlot) bool { return now.After(old.expires) })
	s.slots[id] = t
	return id
}

// take returns the token of the slot id and forgets it; ok is false if the
// slot is unknown (shown already), expired or belongs to another user.
func (s *tokenSlots) take(id string, userID int64) (t tokenSlot, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok = s.slots[id]
	if !ok || t.userID != userID {
		return tokenSlot{}, false
	}
	delete(s.slots, id)
	if time.Now().After(t.expires) {
		return tokenSlot{}, false
	}
	return t, true
}

// tokenOwner returns the id of the signed-in user (0 if none).
func tokenOwner(r *http.Request) int64 {
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		return id.User.ID
	}
	return 0
}
