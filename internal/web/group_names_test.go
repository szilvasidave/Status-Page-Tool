package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

const (
	opsID   = "3f2b8c1e-5d4a-4e6b-9c7d-1a2b3c4d5e6f"
	staffID = "8a7b6c5d-4e3f-4a1b-8c9d-0e1f2a3b4c5d"
	goneID  = "d4c3b2a1-0f9e-4d8c-b7a6-958473625140"
)

// groupNamesEnv is a test environment whose single sign-on settings use the
// app registration of the fake Microsoft Graph, with the lookup of group
// names on, and an administrator signed in.
func groupNamesEnv(t *testing.T) (*testEnv, *fakegraph.Server) {
	t.Helper()
	e := newTestEnv(t)
	fg := fakegraph.Start(t)
	saveSettings(t, e, func(s *store.Settings) {
		s.OIDCEnabled = true
		s.OIDCIssuer = fg.Issuer()
		s.OIDCClientID = fg.ClientID
		s.OIDCClientSecret = fg.ClientSecret
		s.EntraGroupNames = true
	})
	e.loginAdmin()
	return e, fg
}

func saveSettings(t *testing.T, e *testEnv, f func(*store.Settings)) {
	t.Helper()
	st, err := e.Store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f(st)
	if err := e.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func groupByExternalID(t *testing.T, e *testEnv, ext string) *store.Group {
	t.Helper()
	groups, err := e.Store.ListGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range groups {
		if groups[i].ExternalID == ext {
			return &groups[i]
		}
	}
	t.Fatalf("no group %s", ext)
	return nil
}

func TestGroupNamesLookUpNow(t *testing.T) {
	e, fg := groupNamesEnv(t)
	ctx := context.Background()
	u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-1", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: opsID}, {ExternalID: staffID}, {ExternalID: goneID}, {ExternalID: "keycloak-name"}}); err != nil {
		t.Fatal(err)
	}
	fg.SetGroup(opsID, "IT-Operations")
	fg.SetGroup(staffID, "All Staff")

	r := e.get("/admin/groups")
	mustContain(t, r.Body, "Names are looked up in Microsoft Entra ID", `action="/admin/groups/names"`, "Look up names now", opsID)
	if strings.Contains(r.Body, "latest lookup") || strings.Contains(r.Body, "IT-Operations") {
		t.Error("the page shows a lookup that did not happen")
	}

	r = adminFollow(t, e, e.post("/admin/groups/names", nil))
	mustContain(t, r.Body, "The names of 3 groups were looked up in Microsoft Entra ID. 1 group was not found there",
		"IT-Operations", "All Staff", "latest lookup")
	if g := groupByExternalID(t, e, opsID); g.DisplayName() != "IT-Operations" || g.Name != "" {
		t.Errorf("group = %+v", g)
	}
	// Elsewhere: the users list and the service form.
	mustContain(t, e.get("/admin/users").Body, "All Staff, "+goneID+", IT-Operations, keycloak-name")
	mustContain(t, e.get("/admin/services/new").Body, "IT-Operations")

	// A failure is shown on the page until a lookup succeeds.
	fg.FailGroupsNext(fakegraph.GroupsDenied())
	r = adminFollow(t, e, e.post("/admin/groups/names", nil))
	mustContain(t, r.Body, "The names of the groups could not be looked up in Microsoft Entra ID", "Authorization_RequestDenied",
		"GroupMember.Read.All")
	mustContain(t, e.get("/admin/groups").Body, `class="flash flash-warning"`, "Insufficient privileges")
	r = adminFollow(t, e, e.post("/admin/groups/names", nil))
	if strings.Contains(r.Body, "Insufficient privileges") {
		t.Error("the error is still shown after a successful lookup")
	}

	// Turned off: a hint instead of the button; the button's request is refused.
	saveSettings(t, e, func(s *store.Settings) { s.EntraGroupNames = false })
	r = adminFollow(t, e, e.post("/admin/groups/names", nil))
	mustContain(t, r.Body, "Looking up group names in Microsoft Entra ID is turned off", "To show their names, turn on")
	if strings.Contains(r.Body, "Look up names now") {
		t.Error("the button is shown while the lookup is off")
	}
	// No hint with another identity provider (its claim may hold names).
	saveSettings(t, e, func(s *store.Settings) { s.OIDCIssuer = "https://keycloak.example.com/realms/it" })
	if r := e.get("/admin/groups"); strings.Contains(r.Body, "To show their names") {
		t.Error("hint shown for a provider other than Microsoft Entra ID")
	}
}

func TestGroupAddLooksUpName(t *testing.T) {
	e, fg := groupNamesEnv(t)
	fg.SetGroup(opsID, "IT-Operations")

	// The object ID in upper case, without a name: the name comes from Entra ID.
	r := e.post("/admin/groups", url.Values{"external_id": {strings.ToUpper(opsID)}})
	r = adminFollow(t, e, r)
	mustContain(t, r.Body, "Group “IT-Operations” added. Choose its role", "Name in Microsoft Entra ID", `placeholder="IT-Operations"`)

	// Unknown to Entra ID: added, with a warning.
	r = adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {goneID}}))
	mustContain(t, r.Body, `flash-warning`, "Microsoft Entra ID has no group with this object ID – check the ID.",
		"Not found (deleted, or not a group's ID)")

	// A name given here takes precedence; the directory name is looked up all the same.
	fg.SetGroup(staffID, "All Staff")
	r = adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {staffID}, "name": {"Everybody"}}))
	mustContain(t, r.Body, "Group “Everybody” added.", "All Staff")

	// The lookup fails: added, with the reason.
	fg.FailGroupsNext(fakegraph.GroupsDenied())
	r = adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {"aaaaaaaa-0f9e-4d8c-b7a6-958473625140"}}))
	mustContain(t, r.Body, "flash-warning", "Its name could not be looked up in Microsoft Entra ID", "GroupMember.Read.All")

	// Names that are not object IDs are not looked up.
	before, _ := fg.GroupRequests()
	r = adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {"it-admins"}}))
	mustContain(t, r.Body, "Group “it-admins” added.")
	if after, _ := fg.GroupRequests(); after != before {
		t.Error("a group name was looked up in Microsoft Entra ID")
	}
}

// The display name of a group: a name equal to the directory name or the
// group ID is not stored, so that the group follows renames in Entra ID.
func TestGroupNameFollowsDirectory(t *testing.T) {
	e, fg := groupNamesEnv(t)
	fg.SetGroup(opsID, "IT-Operations")
	adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {opsID}}))
	g := groupByExternalID(t, e, opsID)
	path := "/admin/groups/" + strconv.FormatInt(g.ID, 10)
	page := e.get(path)
	mustContain(t, page.Body, "Empty shows the name in Microsoft Entra ID, which follows renames there.")

	for _, name := range []string{"IT-Operations", opsID, ""} {
		if r := e.post(path, url.Values{"name": {name}, "role": {"editor"}}); r.Status != http.StatusSeeOther {
			t.Fatalf("save with name %q: %d", name, r.Status)
		}
		if g := groupByExternalID(t, e, opsID); g.Name != "" || g.DisplayName() != "IT-Operations" || g.Role != store.RoleEditor {
			t.Errorf("after saving the name %q: %+v", name, g)
		}
	}
	fg.SetGroup(opsID, "IT Operations (renamed)")
	adminFollow(t, e, e.post("/admin/groups/names", nil))
	if g := groupByExternalID(t, e, opsID); g.DisplayName() != "IT Operations (renamed)" {
		t.Errorf("after the rename: %q", g.DisplayName())
	}
	e.post(path, url.Values{"name": {"Ops"}, "role": {"editor"}})
	if g := groupByExternalID(t, e, opsID); g.Name != "Ops" || g.DisplayName() != "Ops" {
		t.Errorf("own name: %+v", g)
	}
}

// The groups page and the add form without the lookup.
func TestGroupNamesOff(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	saveSettings(t, e, func(s *store.Settings) {
		s.OIDCEnabled = true
		s.OIDCIssuer = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
		s.OIDCClientID = "c"
		s.OIDCClientSecret = "s"
	})
	r := e.get("/admin/groups")
	mustContain(t, r.Body, "Empty: the group ID is shown.")
	if strings.Contains(r.Body, "To show their names") {
		t.Error("hint without groups")
	}
	adminFollow(t, e, e.post("/admin/groups", url.Values{"external_id": {opsID}}))
	mustContain(t, e.get("/admin/groups").Body, "Groups from Microsoft Entra ID are shown by their object IDs", `href="/admin/settings#sso"`)
	// A named group needs no hint.
	g := groupByExternalID(t, e, opsID)
	e.post("/admin/groups/"+strconv.FormatInt(g.ID, 10), url.Values{"name": {"Ops"}, "role": {"viewer"}})
	if strings.Contains(e.get("/admin/groups").Body, "To show their names") {
		t.Error("hint although every group has a name")
	}
}
