package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// While the local administrator sign-in is off, the only group with the
// Administrator role can neither lose the role nor be deleted: nobody could
// administer the status page any more.
func TestLastAdminGroupKept(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAs("boss@example.com", store.RoleAdmin) // member of the group "test-group-admin"
	if r := e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), false)); r.Status != http.StatusSeeOther {
		t.Fatalf("turn the local sign-in off: %d", r.Status)
	}
	group := func(externalID string) *store.Group {
		t.Helper()
		groups, err := e.Store.ListGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			if g.ExternalID == externalID {
				return &g
			}
		}
		return nil
	}
	path := func(g *store.Group) string { return "/admin/groups/" + strconv.FormatInt(g.ID, 10) }
	const refused = "This is the only group with the Administrator role and the local administrator sign-in is turned off"

	admins := group("test-group-admin")
	r := e.post(path(admins), url.Values{"name": {"Admins"}, "role": {"editor"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("demote the only administrator group: %d", r.Status)
	}
	mustContain(t, r.Body, refused)
	r = e.post(path(admins)+"/delete", nil)
	if r.Status != http.StatusSeeOther || r.Location != path(admins) {
		t.Fatalf("delete the only administrator group: %d %q", r.Status, r.Location)
	}
	mustContain(t, e.get(r.Location).Body, refused)
	if g := group("test-group-admin"); g == nil || g.Role != store.RoleAdmin {
		t.Fatalf("the only administrator group was changed: %+v", g)
	}
	if r := e.get("/admin/settings"); r.Status != http.StatusOK {
		t.Fatalf("the administrator lost access: %d", r.Status)
	}

	// An administrator group that is not the only one can lose the role, and
	// a group without it can be deleted.
	other, err := e.Store.CreateGroup(ctx, "it-admins", "IT admins")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.post(path(other), url.Values{"role": {"admin"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("make another group an administrator group: %d", r.Status)
	}
	if r := e.post(path(other), url.Values{"role": {"viewer"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("demote an administrator group that is not the only one: %d", r.Status)
	}
	if r := e.post(path(other)+"/delete", nil); r.Status != http.StatusSeeOther || r.Location != "/admin/groups" {
		t.Fatalf("delete a group without a role: %d %q", r.Status, r.Location)
	}

	// With the local sign-in on, the last administrator group can lose its
	// role: the local administrator can still sign in.
	if r := e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), true)); r.Status != http.StatusSeeOther {
		t.Fatalf("turn the local sign-in on: %d", r.Status)
	}
	if r := e.post(path(admins), url.Values{"role": {"editor"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("demote the only administrator group with the local sign-in on: %d", r.Status)
	}
}
