package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// The group page shows the last provisioning change only for provisioned
// groups: after an upgrade, last_synced_at may still hold the time of the
// directory sync of earlier versions (removed).
func TestGroupPageLastProvisioningChange(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAdmin()
	page := func(g *store.Group) string {
		t.Helper()
		r := e.get("/admin/groups/" + strconv.FormatInt(g.ID, 10))
		if r.Status != http.StatusOK {
			t.Fatalf("group page: %d", r.Status)
		}
		return r.Body
	}

	synced, err := e.Store.CreateGroup(ctx, "synced-before", "Synced before")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB().ExecContext(ctx, `UPDATE idp_groups SET last_synced_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).Unix(), synced.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page(synced), "Last provisioning change") {
		t.Error("a group that is not provisioned shows the last provisioning change")
	}

	provisioned, _, err := e.Store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "Provisioned", ExternalID: "provisioned"})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, page(provisioned), "Last provisioning change")
}
