package scim

// Discovery endpoints (RFC 7644 section 4): ServiceProviderConfig,
// ResourceTypes and Schemas. They describe what the endpoint supports and
// the attributes it stores.

import (
	"net/http"
	"strings"
)

func serviceProviderConfig(req *request) error {
	type supported struct {
		Supported bool `json:"supported"`
	}
	writeJSON(req.w, http.StatusOK, map[string]any{
		"schemas":        []string{schemaSPConfig},
		"patch":          supported{true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": maxCount},
		"changePassword": supported{false},
		"sort":           supported{false},
		"etag":           supported{false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "The token generated in Admin → Settings of the status page, sent as Authorization: Bearer <token>.",
			"primary":     true,
		}},
		"meta": meta{ResourceType: "ServiceProviderConfig", Location: req.base + "/ServiceProviderConfig"},
	})
	return nil
}

// resourceType describes /Users or /Groups.
type resourceType struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Endpoint    string   `json:"endpoint"`
	Description string   `json:"description"`
	Schema      string   `json:"schema"`
	Meta        meta     `json:"meta"`
}

func resourceTypes(req *request, args []string) error {
	all := []resourceType{
		{Schemas: []string{schemaResourceType}, ID: "User", Name: "User", Endpoint: "/Users", Description: "User account", Schema: schemaUser,
			Meta: meta{ResourceType: "ResourceType", Location: req.base + "/ResourceTypes/User"}},
		{Schemas: []string{schemaResourceType}, ID: "Group", Name: "Group", Endpoint: "/Groups", Description: "Group", Schema: schemaGroup,
			Meta: meta{ResourceType: "ResourceType", Location: req.base + "/ResourceTypes/Group"}},
	}
	return writeDiscovery(req, args, len(all), func(i int) (string, any) { return all[i].ID, all[i] })
}

// attribute describes an attribute in a schema (RFC 7643 section 7).
type attribute struct {
	Name           string      `json:"name"`
	Type           string      `json:"type"`
	MultiValued    bool        `json:"multiValued"`
	Description    string      `json:"description,omitempty"`
	Required       bool        `json:"required"`
	CaseExact      bool        `json:"caseExact"`
	Mutability     string      `json:"mutability"`
	Returned       string      `json:"returned"`
	Uniqueness     string      `json:"uniqueness"`
	ReferenceTypes []string    `json:"referenceTypes,omitempty"`
	SubAttributes  []attribute `json:"subAttributes,omitempty"`
}

// str returns a single-valued, optional, read-write string attribute.
func str(name, description string) attribute {
	return attribute{Name: name, Type: "string", Description: description, Mutability: "readWrite", Returned: "default", Uniqueness: "none"}
}

type schema struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Attributes  []attribute `json:"attributes"`
	Meta        meta        `json:"meta"`
}

func schemas(req *request, args []string) error {
	userName := str("userName", "User name at the identity provider (Microsoft Entra ID: user principal name). Unique.")
	userName.Required, userName.Uniqueness = true, "server"
	emails := str("emails", "E-mail addresses; notifications go to the work address (else the primary or the first one).")
	emails.Type, emails.MultiValued = "complex", true
	emails.SubAttributes = []attribute{str("value", "E-mail address."), str("type", "work, home or other."),
		{Name: "primary", Type: "boolean", Mutability: "readWrite", Returned: "default", Uniqueness: "none"}}
	name := str("name", "Name of the user.")
	name.Type = "complex"
	name.SubAttributes = []attribute{str("formatted", "Full name."), str("familyName", "Family name."), str("givenName", "Given name.")}
	active := attribute{Name: "active", Type: "boolean", Description: "False: the user is deactivated, cannot sign in and gets no e-mails.",
		Mutability: "readWrite", Returned: "default", Uniqueness: "none"}

	displayName := str("displayName", "Name of the group. Unique.")
	displayName.Required = true
	members := attribute{Name: "members", Type: "complex", MultiValued: true, Description: "Users in the group.",
		Mutability: "readWrite", Returned: "default", Uniqueness: "none",
		SubAttributes: []attribute{
			{Name: "value", Type: "string", Description: "id of the user.", Mutability: "immutable", Returned: "default", Uniqueness: "none"},
			{Name: "$ref", Type: "reference", ReferenceTypes: []string{"User"}, Description: "URI of the user.", Mutability: "immutable", Returned: "default", Uniqueness: "none"},
			{Name: "display", Type: "string", Description: "Name of the user.", Mutability: "readOnly", Returned: "default", Uniqueness: "none"},
		}}

	all := []schema{
		{Schemas: []string{schemaSchema}, ID: schemaUser, Name: "User", Description: "User account",
			Attributes: []attribute{userName, name, str("displayName", "Name shown in the status page."), emails, active},
			Meta:       meta{ResourceType: "Schema", Location: req.base + "/Schemas/" + schemaUser}},
		{Schemas: []string{schemaSchema}, ID: schemaGroup, Name: "Group", Description: "Group",
			Attributes: []attribute{displayName, members},
			Meta:       meta{ResourceType: "Schema", Location: req.base + "/Schemas/" + schemaGroup}},
	}
	return writeDiscovery(req, args, len(all), func(i int) (string, any) { return all[i].ID, all[i] })
}

// writeDiscovery responds with all n items as a list, or with the one whose
// id is args[0].
func writeDiscovery(req *request, args []string, n int, item func(int) (id string, v any)) error {
	if len(args) == 0 {
		resources := make([]any, n)
		for i := range n {
			_, resources[i] = item(i)
		}
		writeJSON(req.w, http.StatusOK, newListResponse(n, 1, resources))
		return nil
	}
	for i := range n {
		if id, v := item(i); strings.EqualFold(id, args[0]) {
			writeJSON(req.w, http.StatusOK, v)
			return nil
		}
	}
	return errorf(http.StatusNotFound, "", "%s not found.", args[0])
}
