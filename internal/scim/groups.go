package scim

// /Groups. Attribute mapping (Microsoft Entra ID defaults in brackets):
//
//	id            idp_groups.id (decimal)
//	displayName   name [displayName]
//	externalId    external_id, GUIDs in lower case [objectId], the value the
//	              groups claim contains, so a group known from sign-ins is taken over
//	members       user_groups: the ids of users created by /Users [members]
//
// Only provisioned groups are visible. DELETE keeps the group with its role
// and services (see store.DeleteSCIMGroup).

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// groupJSON is a group resource as returned.
type groupJSON struct {
	Schemas     []string      `json:"schemas"`
	ID          string        `json:"id"`
	ExternalID  string        `json:"externalId,omitempty"`
	DisplayName string        `json:"displayName"`
	Members     *[]memberJSON `json:"members,omitempty"` // nil when excluded
	Meta        meta          `json:"meta"`
}

type memberJSON struct {
	Value   string `json:"value"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

// toGroupJSON converts a group; members is nil to leave them out. A
// generated external id is not shown: the client did not set one.
func toGroupJSON(g *store.Group, members []store.SCIMMember, withMembers bool, base string) groupJSON {
	id := strconv.FormatInt(g.ID, 10)
	out := groupJSON{
		Schemas: []string{schemaGroup}, ID: id, DisplayName: g.DisplayName(),
		Meta: meta{ResourceType: "Group", Created: timestamp(g.CreatedAt), LastModified: timestamp(g.CreatedAt), Location: base + "/Groups/" + id},
	}
	if !strings.HasPrefix(g.ExternalID, store.SCIMGeneratedIDPrefix) {
		out.ExternalID = g.ExternalID
	}
	if g.LastSyncedAt != nil {
		out.Meta.LastModified = timestamp(*g.LastSyncedAt)
	}
	if withMembers {
		list := make([]memberJSON, 0, len(members))
		for _, m := range members {
			uid := strconv.FormatInt(m.UserID, 10)
			list = append(list, memberJSON{Value: uid, Ref: base + "/Users/" + uid, Display: m.Display})
		}
		out.Members = &list
	}
	return out
}

// groups handles /Groups and /Groups/{id}.
func (h *Handler) groups(req *request, args []string) error {
	if len(args) == 0 {
		switch req.r.Method {
		case http.MethodGet:
			return h.listGroups(req)
		case http.MethodPost:
			return h.createGroup(req)
		}
		return methodNotAllowed(req, http.MethodGet, http.MethodPost)
	}
	if len(args) > 1 {
		return errorf(http.StatusNotFound, "", "Not found: %s.", req.r.URL.Path)
	}
	if err := resourceMethod(req); err != nil {
		return err
	}
	id, ok := parseID(args[0])
	if !ok {
		return notFoundError("Group", args[0])
	}
	switch req.r.Method {
	case http.MethodGet:
		g, err := h.store.SCIMGroup(req.r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return notFoundError("Group", args[0])
		} else if err != nil {
			return err
		}
		return h.writeGroup(req, http.StatusOK, g)
	case http.MethodPut:
		return h.replaceGroup(req, id)
	case http.MethodPatch:
		return h.patchGroup(req, id)
	}
	err := h.store.DeleteSCIMGroup(req.r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return notFoundError("Group", args[0])
	} else if err != nil {
		return err
	}
	h.log.Info("SCIM: group deleted; its members were removed, the group keeps its role and services", "group_id", id)
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

// groupResources converts groups, with their members unless the request
// excludes them (excludedAttributes=members, as Microsoft Entra ID asks).
func (h *Handler) groupResources(req *request, groups []store.Group) ([]any, error) {
	withMembers := !excluded(req, "members")
	var members map[int64][]store.SCIMMember
	if withMembers && len(groups) > 0 {
		ids := make([]int64, len(groups))
		for i, g := range groups {
			ids[i] = g.ID
		}
		var err error
		if members, err = h.store.SCIMGroupMembers(req.r.Context(), ids); err != nil {
			return nil, err
		}
	}
	resources := make([]any, 0, len(groups))
	for i := range groups {
		resources = append(resources, toGroupJSON(&groups[i], members[groups[i].ID], withMembers, req.base))
	}
	return resources, nil
}

// writeGroup responds with one group; a created group (201) gets a
// Location header.
func (h *Handler) writeGroup(req *request, status int, g *store.Group) error {
	resources, err := h.groupResources(req, []store.Group{*g})
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		req.w.Header().Set("Location", req.base+"/Groups/"+strconv.FormatInt(g.ID, 10))
	}
	writeJSON(req.w, status, resources[0])
	return nil
}

func (h *Handler) listGroups(req *request) error {
	startIndex, count, err := paging(req)
	if err != nil {
		return err
	}
	var q store.SCIMGroupQuery
	none := false
	if f := req.r.URL.Query().Get("filter"); strings.TrimSpace(f) != "" {
		conds, err := parseFilter(f)
		if err != nil {
			return err
		}
		if q, none, err = groupQuery(conds); err != nil {
			return err
		}
	}
	var (
		groups []store.Group
		total  int
	)
	if !none {
		if groups, total, err = h.store.SCIMGroups(req.r.Context(), q, startIndex-1, count); err != nil {
			return err
		}
	}
	resources, err := h.groupResources(req, groups)
	if err != nil {
		return err
	}
	writeJSON(req.w, http.StatusOK, newListResponse(total, startIndex, resources))
	return nil
}

// groupQuery turns filter conditions into a query (see userQuery).
func groupQuery(conds []cond) (q store.SCIMGroupQuery, none bool, err error) {
	for _, c := range conds {
		if c.value == "" || strings.EqualFold(c.value, "null") && !c.str {
			none = true
			continue
		}
		p := c.path
		switch {
		case p.is("id", ""):
			id, ok := parseID(c.value)
			if !ok || (q.ID != 0 && q.ID != id) {
				none = true
			}
			q.ID = id
		case p.is("displayname", ""):
			if q.DisplayName != "" && !strings.EqualFold(q.DisplayName, c.value) {
				none = true
			}
			q.DisplayName = c.value
		case p.is("externalid", ""):
			if q.ExternalID != "" && store.NormExternalID(q.ExternalID) != store.NormExternalID(c.value) {
				none = true
			}
			q.ExternalID = c.value
		default:
			return q, false, errFilter("groups can be filtered by displayName, externalId and id, not by %s.", pathString(p))
		}
	}
	return q, none, nil
}

func (h *Handler) createGroup(req *request) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	g := &groupState{members: map[int64]bool{}}
	if err := g.applyResource(body); err != nil {
		return err
	}
	in, err := g.data()
	if err != nil {
		return err
	}
	created, adopted, err := h.store.CreateSCIMGroup(req.r.Context(), in)
	if err != nil {
		return err
	}
	if adopted {
		h.log.Info("SCIM: existing group taken over by provisioning (role and services kept, members replaced)",
			"group_id", created.ID, "group", created.Name, "members", created.MemberCount)
	} else {
		h.log.Info("SCIM: group created", "group_id", created.ID, "group", created.Name, "members", created.MemberCount)
	}
	return h.writeGroup(req, http.StatusCreated, created)
}

func (h *Handler) replaceGroup(req *request, id int64) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	g := &groupState{members: map[int64]bool{}}
	if err := g.applyResource(body); err != nil {
		return err
	}
	in, err := g.data()
	if err != nil {
		return err
	}
	updated, err := h.updateGroup(req, id, func(d *store.SCIMGroupData) error {
		d.DisplayName, d.Members = in.DisplayName, in.Members
		if in.ExternalID != "" {
			d.ExternalID = in.ExternalID
		}
		return nil
	})
	if err != nil {
		return err
	}
	return h.writeGroup(req, http.StatusOK, updated)
}

// patchGroup applies a PATCH request and responds with 204, as Microsoft
// recommends for groups.
func (h *Handler) patchGroup(req *request, id int64) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	ops, err := operations(body)
	if err != nil {
		return err
	}
	_, err = h.updateGroup(req, id, func(d *store.SCIMGroupData) error {
		g := &groupState{displayName: d.DisplayName, externalID: d.ExternalID, members: map[int64]bool{}}
		for _, uid := range d.Members {
			g.members[uid] = true
		}
		for _, op := range ops {
			if err := op.apply(g.apply); err != nil {
				return err
			}
		}
		in, err := g.data()
		if err != nil {
			return err
		}
		*d = in
		return nil
	})
	if err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

// updateGroup stores a changed group and logs the member changes.
func (h *Handler) updateGroup(req *request, id int64, change func(*store.SCIMGroupData) error) (*store.Group, error) {
	var before []int64
	var after []int64
	g, err := h.store.UpdateSCIMGroup(req.r.Context(), id, func(d *store.SCIMGroupData) error {
		before = slices.Clone(d.Members)
		if err := change(d); err != nil {
			return err
		}
		after = d.Members
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundError("Group", strconv.FormatInt(id, 10))
	} else if err != nil {
		return nil, err
	}
	was := make(map[int64]bool, len(before))
	for _, uid := range before {
		was[uid] = true
	}
	added, removed := 0, len(was)
	for _, uid := range after {
		if was[uid] {
			removed--
		} else {
			added++
		}
	}
	h.log.Info("SCIM: group updated", "group_id", g.ID, "group", g.Name, "members", g.MemberCount,
		"requested_additions", added, "requested_removals", removed)
	return g, nil
}

// ------------------------------------------------------------------ state

// groupState holds the attributes of a group while a request is applied.
type groupState struct {
	displayName, externalID string
	members                 map[int64]bool
}

func (g *groupState) data() (store.SCIMGroupData, error) {
	d := store.SCIMGroupData{
		DisplayName: strings.TrimSpace(g.displayName),
		ExternalID:  strings.TrimSpace(g.externalID),
		Members:     slices.Sorted(maps.Keys(g.members)),
	}
	if d.DisplayName == "" {
		return d, invalidValue("displayName is required.")
	}
	for _, v := range []struct{ name, value string }{{"displayName", d.DisplayName}, {"externalId", d.ExternalID}} {
		if err := checkValue(v.name, v.value); err != nil {
			return d, err
		}
	}
	return d, nil
}

// applyResource sets the attributes of a POST or PUT body.
func (g *groupState) applyResource(body map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(body)) {
		p, err := parsePath(k)
		if err != nil {
			continue // not an attribute name: ignored
		}
		if err := g.apply("replace", p, body[k]); err != nil {
			return err
		}
	}
	return nil
}

// apply applies one operation (add, replace or remove) to the attribute at
// path p. Attributes the status page does not store are ignored.
func (g *groupState) apply(op string, p path, v any) error {
	if p.ext {
		return nil
	}
	switch p.attr {
	case "displayname":
		if p.sub != "" || p.filter != nil {
			return nil
		}
		if op == "remove" {
			return invalidValue("displayName cannot be removed.")
		}
		return setString(&g.displayName, "displayName", v)
	case "externalid":
		if op == "remove" || p.sub != "" || p.filter != nil {
			return nil // the external id identifies the group in the groups claim: kept
		}
		return setString(&g.externalID, "externalId", v)
	case "members":
		return g.applyMembers(op, p, v)
	}
	return nil // id, meta, schemas, description, …
}

func (g *groupState) applyMembers(op string, p path, v any) error {
	if p.sub != "" {
		return nil
	}
	if p.filter != nil { // members[value eq "42"]: only remove uses it
		if op != "remove" {
			return nil
		}
		for _, f := range p.filter {
			if !f.path.is("value", "") {
				return errorf(http.StatusBadRequest, "invalidPath", "Members can only be selected by value.")
			}
			if id, ok := parseID(f.value); ok {
				delete(g.members, id)
			}
		}
		return nil
	}
	ids, err := memberIDs(v)
	if err != nil {
		return err
	}
	switch op {
	case "replace":
		clear(g.members)
		fallthrough
	case "add":
		for _, id := range ids {
			g.members[id] = true
		}
	case "remove":
		if v == nil {
			clear(g.members)
		}
		for _, id := range ids {
			delete(g.members, id)
		}
	}
	return nil
}

// memberIDs returns the user ids of a members value: a list of objects with
// "value" (or one object). Values that cannot be user ids are skipped: they
// cannot be members.
func memberIDs(v any) ([]int64, error) {
	var items []any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		items = x
	case map[string]any:
		items = []any{x}
	default:
		return nil, invalidValue(`members must be a list of objects with "value".`)
	}
	var ids []int64
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, invalidValue(`members must be a list of objects with "value".`)
		}
		var s string
		raw, _ := lookup(m, "value")
		switch x := raw.(type) {
		case string:
			s = x
		case json.Number:
			s = x.String()
		}
		if id, ok := parseID(strings.TrimSpace(s)); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
