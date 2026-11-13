package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// TestServiceFormProvisionedGroups: like the Groups page, the service form
// lists provisioned groups among the groups to notify even before they have
// a role or services, with the Provisioned badge; groups recorded from
// sign-ins without a role or services stay under "Other groups".
func TestServiceFormProvisionedGroups(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	prov, _, err := e.Store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "SAP Key Users", ExternalID: "3f2a9c1e-7b4d-4e8a-9c61-2d5f8e0b7a14"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s1", Email: "sam@example.com", Name: "Sam"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "from-sign-in"}}); err != nil {
		t.Fatal(err)
	}

	r := e.get("/admin/services/new")
	other := strings.Index(r.Body, "Other groups (1)")
	if other < 0 {
		t.Fatalf("no list of other groups:\n%s", r.Body)
	}
	box := `<input type="checkbox" name="groups" value="` + strconv.FormatInt(prov.ID, 10) + `">`
	if i := strings.Index(r.Body, box); i < 0 || i > other {
		t.Error("the provisioned group is not among the groups to notify")
	}
	if i := strings.Index(r.Body, "from-sign-in"); i < other {
		t.Error("the group recorded from a sign-in is not under Other groups")
	}
	mustContain(t, r.Body, `SAP Key Users <span class="badge">Provisioned</span>`,
		"after their members sign in or when Microsoft Entra ID provisions them")
}

// TestProvisionedGroupTexts: an empty provisioned group does not suggest
// that sign-ins add members (they never change provisioned groups), and a
// provisioned name as long as Microsoft Entra ID allows (256 characters) can
// be saved with the group's role.
func TestProvisionedGroupTexts(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	long := strings.Repeat("Ä", 256)
	g, _, err := e.Store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: long, ExternalID: "long-group"})
	if err != nil {
		t.Fatal(err)
	}
	page := "/admin/groups/" + strconv.FormatInt(g.ID, 10)
	r := e.get(page)
	mustContain(t, r.Body, "No members yet. Microsoft Entra ID provisions the direct members of this group",
		`maxlength="256"`)
	if strings.Contains(r.Body, "Members are recorded when they sign in") {
		t.Error("the empty provisioned group says that sign-ins record its members")
	}

	r = e.post(page, url.Values{"name": {long}, "role": {"editor"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("saving the role of a group with a 256-character name: %d", r.Status)
	}
	if got, _ := e.Store.GetGroup(ctx, g.ID); got == nil || got.Role != store.RoleEditor || got.Name != long {
		t.Errorf("group after saving: %+v", got)
	}
	r = e.post(page, url.Values{"name": {long + "x"}, "role": {"editor"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("257 characters: %d", r.Status)
	}
	mustContain(t, r.Body, "The display name is too long (at most 256 characters).")
	mustContain(t, e.get("/admin/groups").Body, `id="name" name="name" type="text" value="" maxlength="256"`)

	// A group that is not provisioned keeps the sign-in hint.
	other, err := e.Store.CreateGroup(ctx, "it-ops", "IT Ops")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.get("/admin/groups/"+strconv.FormatInt(other.ID, 10)).Body, "Members are recorded when they sign in")
}
