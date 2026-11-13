package web

// "Generate token" uses Post/Redirect/Get: reloading the page that shows the
// new token neither shows it again nor generates another token.

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/scim"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// provSlotRe matches the redirect after "Generate token": the id of the
// in-memory slot, never the token.
var provSlotRe = regexp.MustCompile(`^/admin/settings\?new-token=([A-Za-z0-9_-]{20,})#provisioning$`)

// provGenerateToken clicks "Generate token" and follows the redirect to the
// settings page, which shows the new token. Returns the token, the response
// to the POST and the page.
func provGenerateToken(t *testing.T, e *testEnv) (token string, redirect, page Response) {
	t.Helper()
	redirect = provPostGenerate(t, e)
	page = e.get(redirect.Location)
	m := provTokenRe.FindStringSubmatch(page.Body)
	if page.Status != http.StatusOK || m == nil {
		t.Fatalf("no token on the page (%d):\n%s", page.Status, page.Body)
	}
	return m[1], redirect, page
}

func TestProvisioningTokenReload(t *testing.T) {
	e, logs := provEnv(t)
	e.loginAdmin()

	token, gen, _ := provGenerateToken(t, e)
	if strings.Contains(gen.Location, token) {
		t.Error("the token is in the URL")
	}
	hash := adminSettings(t, e).SCIMTokenHash
	if hash != scim.HashToken(token) {
		t.Fatal("the shown token is not the stored one")
	}

	// Reloading shows the page without the token and generates none.
	again := e.get(gen.Location)
	if again.Status != http.StatusOK || strings.Contains(again.Body, token) || strings.Contains(again.Body, `id="scim-token"`) {
		t.Fatalf("reload: %d, token shown again: %v", again.Status, strings.Contains(again.Body, token))
	}
	mustContain(t, again.Body, "Provisioning is on.")
	if adminSettings(t, e).SCIMTokenHash != hash {
		t.Error("reloading generated another token")
	}
	if strings.Contains(logs.String(), token) {
		t.Error("the token is in the log")
	}

	// Unknown slots show the normal page.
	if r := e.get("/admin/settings?new-token=unknown-slot-id-0123456789#provisioning"); r.Status != http.StatusOK || strings.Contains(r.Body, `id="scim-token"`) {
		t.Errorf("unknown slot: %d", r.Status)
	}

	// Only the administrator who generated a token sees it.
	gen2 := provPostGenerate(t, e)
	e.logout()
	e.loginAs("other-admin@example.com", store.RoleAdmin)
	if r := e.get(gen2.Location); r.Status != http.StatusOK || strings.Contains(r.Body, `id="scim-token"`) {
		t.Fatalf("another administrator sees the token (%d)", r.Status)
	}
	e.logout()
	e.loginAdmin()
	if r := e.get(gen2.Location); !provTokenRe.MatchString(r.Body) {
		t.Error("the token is gone for the administrator who generated it")
	}

	// A token that waited too long is not shown.
	gen3 := provPostGenerate(t, e)
	id := provSlotRe.FindStringSubmatch(gen3.Location)[1]
	newTokens.mu.Lock()
	slot := newTokens.slots[id]
	slot.expires = time.Now().Add(-time.Second)
	newTokens.slots[id] = slot
	newTokens.mu.Unlock()
	if r := e.get(gen3.Location); strings.Contains(r.Body, `id="scim-token"`) {
		t.Error("an expired token is shown")
	}
	newTokens.mu.Lock()
	_, kept := newTokens.slots[id]
	newTokens.mu.Unlock()
	if kept {
		t.Error("the expired slot was kept")
	}
}

// provPostGenerate clicks "Generate token" without opening the page it
// redirects to.
func provPostGenerate(t *testing.T, e *testEnv) Response {
	t.Helper()
	r := e.post("/admin/settings/scim-token", nil)
	if r.Status != http.StatusSeeOther || !provSlotRe.MatchString(r.Location) {
		t.Fatalf("generate token: %d %q, want a redirect to the settings page", r.Status, r.Location)
	}
	return r
}
