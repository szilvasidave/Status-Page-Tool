package web

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// TestSSOAppRegistrationFieldsShown: the issuer URL, client ID and client
// secret are shown while sign-in with OpenID Connect is off, because e-mail
// with Microsoft Graph uses them, too; only the sign-in fields are hidden.
// They are still checked only while sign-in is enabled.
func TestSSOAppRegistrationFieldsShown(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	r := e.get("/admin/settings")
	hidden := strings.Index(r.Body, `<div id="oidc-fields" hidden>`)
	if hidden < 0 {
		t.Fatal("the sign-in fields are not hidden while sign-in is off")
	}
	for _, id := range []string{`id="oidc_issuer"`, `id="oidc_client_id"`, `id="oidc_client_secret"`} {
		if i := strings.Index(r.Body, id); i < 0 || i > hidden {
			t.Errorf("%s is not shown while sign-in is off", id)
		}
	}
	for _, id := range []string{`id="oidc_scopes"`, `id="oidc_groups_claim"`, `id="oidc_button_label"`} {
		if i := strings.Index(r.Body, id); i < hidden {
			t.Errorf("%s is shown while sign-in is off", id)
		}
	}
	mustContain(t, r.Body, "E-mail with Microsoft Graph (see <a href=\"#email\">E-mail</a>) uses this app registration")

	// Sign-in off: the app registration is saved without sign-in, and empty
	// fields are no error.
	f := adminSettingsForm(adminSettings(t, e))
	f.Set("oidc_issuer", "https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0")
	f.Set("oidc_client_id", "app-id")
	f.Set("oidc_client_secret", "app-secret")
	mustContain(t, adminFollow(t, e, e.post("/admin/settings", f)).Body, "Settings saved.")
	st := adminSettings(t, e)
	if st.OIDCEnabled || st.OIDCClientID != "app-id" || st.OIDCClientSecret != "app-secret" {
		t.Errorf("app registration without sign-in: enabled %v, client %q, secret stored %v", st.OIDCEnabled, st.OIDCClientID, st.OIDCClientSecret != "")
	}
	f.Set("oidc_issuer", "")
	f.Set("oidc_client_id", "")
	mustContain(t, adminFollow(t, e, e.post("/admin/settings", f)).Body, "Settings saved.")

	// Sign-in on: they are required.
	f.Set("oidc_enabled", "on")
	if r := e.post("/admin/settings", f); r.Status != http.StatusUnprocessableEntity {
		t.Errorf("sign-in without issuer and client ID: %d", r.Status)
	} else {
		mustContain(t, r.Body, "Enter the issuer URL of your identity provider.", "Enter the client ID of the application registration.",
			`<div id="oidc-fields">`)
	}
}

// TestProvisioningTokenHelp: the token buttons submit their own forms and
// reload the page with the saved settings, so the section says to save first
// and the settings form asks before unsaved changes are lost (static/app.js).
// While provisioning is off, it says how to turn it on again: with Restart
// provisioning in Microsoft Entra ID.
func TestProvisioningTokenHelp(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	const unsaved = `data-unsaved="Your changes to the settings have not been saved and will be lost. Continue?"`
	r := e.get("/admin/settings")
	mustContain(t, r.Body, `<form method="post" action="/admin/settings" class="form" `+unsaved+`>`,
		`form="scim-token-form" aria-describedby="scim_token_help"`,
		"Save your changes first: generating a token reloads the page with the saved settings.",
		"If provisioning was on", "use <em>Restart provisioning</em> there")

	_, _, r = provGenerateToken(t, e)
	mustContain(t, r.Body, "Save your changes first: these buttons reload the page with the saved",
		`form="scim-off-form" class="btn-danger" aria-describedby="scim_token_help"`,
		"To turn it on again later, generate a new token and use Restart provisioning in Microsoft Entra ID.")
	if strings.Contains(r.Body, "If provisioning was on") {
		t.Error("the hint for turning provisioning on again is shown while it is on")
	}

	js, err := fs.ReadFile(assets, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`f.hasAttribute("data-unsaved")`, `e.submitter.closest("form[data-changed]")`, `outer !== e.target`} {
		if !strings.Contains(string(js), s) {
			t.Errorf("app.js does not contain %s", s)
		}
	}
}
