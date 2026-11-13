package scim

// /Users. Attribute mapping (Microsoft Entra ID defaults in brackets):
//
//	id                         users.id (decimal)
//	userName                   principal_name, as sent [userPrincipalName]
//	externalId                 scim_external_id [mailNickname]
//	emails                     e-mail: the work address, else the primary one, else the first;
//	                           without one a user name that is an e-mail address [mail]
//	displayName / name         name: displayName, else name.formatted, else givenName familyName
//	active                     active [Not IsSoftDeleted]
//
// Other attributes (title, phoneNumbers, addresses, enterprise extension, …)
// are accepted and ignored.

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// userJSON is a user resource as returned.
type userJSON struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id"`
	ExternalID  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	Name        *nameJSON   `json:"name,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
	Emails      []emailJSON `json:"emails,omitempty"`
	Active      bool        `json:"active"`
	Meta        meta        `json:"meta"`
}

type nameJSON struct {
	Formatted string `json:"formatted,omitempty"`
}

type emailJSON struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// toUserJSON converts a user. Users have no modification time in the
// database, so lastModified is the creation time.
func toUserJSON(u *store.User, base string) userJSON {
	id := strconv.FormatInt(u.ID, 10)
	out := userJSON{
		Schemas: []string{schemaUser}, ID: id, ExternalID: u.SCIMExternalID, UserName: u.PrincipalName,
		DisplayName: u.Name, Active: u.Active,
		Meta: meta{ResourceType: "User", Created: timestamp(u.CreatedAt), LastModified: timestamp(u.CreatedAt), Location: base + "/Users/" + id},
	}
	if u.Name != "" {
		out.Name = &nameJSON{Formatted: u.Name}
	}
	if u.Email != "" {
		out.Emails = []emailJSON{{Value: u.Email, Type: "work", Primary: true}}
	}
	return out
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// users handles /Users and /Users/{id}.
func (h *Handler) users(req *request, args []string) error {
	if len(args) == 0 {
		switch req.r.Method {
		case http.MethodGet:
			return h.listUsers(req)
		case http.MethodPost:
			return h.createUser(req)
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
		return notFoundError("User", args[0])
	}
	switch req.r.Method {
	case http.MethodGet:
		u, err := h.store.SCIMUser(req.r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return notFoundError("User", args[0])
		} else if err != nil {
			return err
		}
		writeJSON(req.w, http.StatusOK, toUserJSON(u, req.base))
		return nil
	case http.MethodPut:
		return h.replaceUser(req, id)
	case http.MethodPatch:
		return h.patchUser(req, id)
	}
	return h.deleteUser(req, id)
}

func (h *Handler) listUsers(req *request) error {
	startIndex, count, err := paging(req)
	if err != nil {
		return err
	}
	var q store.SCIMUserQuery
	none := false
	if f := req.r.URL.Query().Get("filter"); strings.TrimSpace(f) != "" {
		conds, err := parseFilter(f)
		if err != nil {
			return err
		}
		if q, none, err = userQuery(conds); err != nil {
			return err
		}
	}
	var (
		users []store.User
		total int
	)
	if !none {
		if users, total, err = h.store.SCIMUsers(req.r.Context(), q, startIndex-1, count); err != nil {
			return err
		}
	}
	resources := make([]any, 0, len(users))
	for i := range users {
		resources = append(resources, toUserJSON(&users[i], req.base))
	}
	writeJSON(req.w, http.StatusOK, newListResponse(total, startIndex, resources))
	return nil
}

// userQuery turns filter conditions into a query. none is true if no user
// can match (e.g. an id that is not a number, or an address type other than
// "work").
func userQuery(conds []cond) (q store.SCIMUserQuery, none bool, err error) {
	set := func(field *string, value string) {
		if *field != "" && !strings.EqualFold(*field, value) {
			none = true // two different values for one attribute
		}
		*field = value
	}
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
		case p.is("username", ""):
			set(&q.UserName, c.value)
		case p.is("externalid", ""):
			if q.ExternalID != "" && q.ExternalID != c.value {
				none = true
			}
			q.ExternalID = c.value
		case p.attr == "emails" && !p.ext && (p.sub == "value" || p.sub == "" && p.filter == nil):
			// Users have one address, the work address and primary.
			for _, f := range p.filter {
				switch {
				case f.path.is("type", ""):
					none = none || !strings.EqualFold(f.value, "work")
				case f.path.is("primary", ""):
					none = none || !f.valueIsTrue()
				default:
					return q, false, errFilter("emails can only be filtered by type and primary.")
				}
			}
			set(&q.Email, c.value)
		default:
			return q, false, errFilter("users can be filtered by userName, externalId, id and emails, not by %s.", pathString(p))
		}
	}
	return q, none, nil
}

// pathString shows a parsed path in error messages.
func pathString(p path) string {
	s := p.attr
	if p.filter != nil {
		s += "[…]"
	}
	if p.sub != "" {
		s += "." + p.sub
	}
	return s
}

func (h *Handler) createUser(req *request) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	u := &userState{active: true}
	if err := u.applyResource(body); err != nil {
		return err
	}
	in, err := u.data()
	if err != nil {
		return err
	}
	created, adopted, err := h.store.CreateSCIMUser(req.r.Context(), in)
	if err != nil {
		return err
	}
	if adopted {
		h.log.Info("SCIM: existing user taken over by provisioning", "user_id", created.ID, "user", created.PrincipalName)
	} else {
		h.log.Info("SCIM: user created", "user_id", created.ID, "user", created.PrincipalName)
	}
	res := toUserJSON(created, req.base)
	req.w.Header().Set("Location", res.Meta.Location)
	writeJSON(req.w, http.StatusCreated, res)
	return nil
}

func (h *Handler) replaceUser(req *request, id int64) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	u := &userState{active: true}
	if err := u.applyResource(body); err != nil {
		return err
	}
	in, err := u.data()
	if err != nil {
		return err
	}
	return h.updateUser(req, id, func(*store.User) (store.SCIMUserData, error) { return in, nil })
}

func (h *Handler) patchUser(req *request, id int64) error {
	body, err := readBody(req)
	if err != nil {
		return err
	}
	ops, err := operations(body)
	if err != nil {
		return err
	}
	return h.updateUser(req, id, func(cur *store.User) (store.SCIMUserData, error) {
		u := userStateOf(cur)
		for _, op := range ops {
			if err := op.apply(u.apply); err != nil {
				return store.SCIMUserData{}, err
			}
		}
		return u.data()
	})
}

// updateUser stores a changed user and responds with it (200).
func (h *Handler) updateUser(req *request, id int64, change func(*store.User) (store.SCIMUserData, error)) error {
	var before store.User
	u, err := h.store.UpdateSCIMUser(req.r.Context(), id, func(cur *store.User) (store.SCIMUserData, error) {
		before = *cur
		return change(cur)
	})
	if errors.Is(err, store.ErrNotFound) {
		return notFoundError("User", strconv.FormatInt(id, 10))
	} else if err != nil {
		return err
	}
	switch {
	case before.Active && !u.Active:
		h.log.Info("SCIM: user deactivated", "user_id", u.ID, "user", u.PrincipalName)
	case !before.Active && u.Active:
		h.log.Info("SCIM: user activated", "user_id", u.ID, "user", u.PrincipalName)
	case !before.Provisioned:
		h.log.Info("SCIM: existing user taken over by provisioning", "user_id", u.ID, "user", u.PrincipalName)
	default:
		h.log.Debug("SCIM: user updated", "user_id", u.ID, "user", u.PrincipalName)
	}
	writeJSON(req.w, http.StatusOK, toUserJSON(u, req.base))
	return nil
}

func (h *Handler) deleteUser(req *request, id int64) error {
	err := h.store.DeleteUser(req.r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrLocalAdmin) {
		return notFoundError("User", strconv.FormatInt(id, 10))
	} else if err != nil {
		return err
	}
	h.log.Info("SCIM: user deleted", "user_id", id)
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

// ------------------------------------------------------------------ state

// userState holds the attributes of a user while a request is applied. Only
// what the status page stores is kept; the name parts only serve to derive
// the name.
type userState struct {
	userName, externalID, displayName string
	formatted, givenName, familyName  string
	emails                            []email
	active                            bool
}

type email struct {
	value, typ string
	primary    bool
}

// userStateOf returns the state of a stored user (its address as the work
// address).
func userStateOf(u *store.User) *userState {
	s := &userState{userName: u.PrincipalName, externalID: u.SCIMExternalID, displayName: u.Name, active: u.Active}
	if u.Email != "" {
		s.emails = []email{{value: u.Email, typ: "work", primary: true}}
	}
	return s
}

// data returns the attributes to store and checks them.
func (s *userState) data() (store.SCIMUserData, error) {
	d := store.SCIMUserData{
		UserName:   strings.TrimSpace(s.userName),
		ExternalID: strings.TrimSpace(s.externalID),
		Name:       strings.TrimSpace(s.displayName),
		Active:     s.active,
	}
	if d.Name == "" {
		d.Name = strings.TrimSpace(s.formatted)
	}
	if d.Name == "" {
		d.Name = strings.TrimSpace(strings.TrimSpace(s.givenName) + " " + strings.TrimSpace(s.familyName))
	}
	if d.UserName == "" {
		return d, invalidValue("userName is required.")
	}
	d.Email = s.address()
	if d.Email != "" && !looksLikeEmail(d.Email) {
		return d, invalidValue("The e-mail address %q is not valid.", d.Email)
	}
	for _, v := range []struct{ name, value string }{
		{"userName", d.UserName}, {"externalId", d.ExternalID}, {"displayName", d.Name}, {"emails", d.Email},
	} {
		if err := checkValue(v.name, v.value); err != nil {
			return d, err
		}
	}
	return d, nil
}

// address returns the e-mail address to use: the work address, else the
// primary one, else the first; without any the user name if it is an
// address (not for guests, whose user names contain #EXT#).
func (s *userState) address() string {
	pick := func(match func(email) bool) string {
		for _, e := range s.emails {
			if v := strings.TrimSpace(e.value); v != "" && match(e) {
				return strings.ToLower(v)
			}
		}
		return ""
	}
	for _, match := range []func(email) bool{
		func(e email) bool { return strings.EqualFold(e.typ, "work") },
		func(e email) bool { return e.primary },
		func(email) bool { return true },
	} {
		if v := pick(match); v != "" {
			return v
		}
	}
	if u := strings.TrimSpace(s.userName); looksLikeEmail(u) && !strings.Contains(strings.ToUpper(u), "#EXT#") {
		return strings.ToLower(u)
	}
	return ""
}

// applyResource sets the attributes of a POST or PUT body.
func (s *userState) applyResource(body map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(body)) {
		p, err := parsePath(k)
		if err != nil {
			continue // not an attribute name: ignored
		}
		if err := s.apply("replace", p, body[k]); err != nil {
			return err
		}
	}
	return nil
}

// apply applies one operation (add, replace or remove) to the attribute at
// path p. Attributes the status page does not store are ignored.
func (s *userState) apply(op string, p path, v any) error {
	if p.ext {
		return nil
	}
	switch p.attr {
	case "username":
		if p.sub != "" || p.filter != nil {
			return nil
		}
		if op == "remove" {
			return invalidValue("userName cannot be removed.")
		}
		return setString(&s.userName, "userName", v)
	case "externalid", "displayname":
		field := map[string]*string{"externalid": &s.externalID, "displayname": &s.displayName}[p.attr]
		if p.sub != "" || p.filter != nil {
			return nil
		}
		if op == "remove" {
			*field = ""
			return nil
		}
		return setString(field, p.attr, v)
	case "active":
		if op == "remove" || p.sub != "" || p.filter != nil {
			return nil
		}
		b, err := boolValue(v)
		if err != nil {
			return err
		}
		s.active = b
		return nil
	case "name":
		return s.applyName(op, p, v)
	case "emails":
		return s.applyEmails(op, p, v)
	}
	return nil // id, meta, schemas, title, phoneNumbers, addresses, …
}

func (s *userState) applyName(op string, p path, v any) error {
	parts := map[string]*string{"formatted": &s.formatted, "givenname": &s.givenName, "familyname": &s.familyName}
	if p.filter != nil {
		return nil
	}
	if p.sub != "" {
		field, ok := parts[p.sub]
		if !ok {
			return nil // middleName, honorificPrefix, …
		}
		if op == "remove" {
			*field = ""
			return nil
		}
		return setString(field, "name."+p.sub, v)
	}
	if op == "remove" || v == nil {
		s.formatted, s.givenName, s.familyName = "", "", ""
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return invalidValue("name must be an object.")
	}
	if op == "replace" {
		s.formatted, s.givenName, s.familyName = "", "", ""
	}
	for k, val := range m {
		if field, ok := parts[strings.ToLower(k)]; ok {
			if err := setString(field, "name."+k, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *userState) applyEmails(op string, p path, v any) error {
	if p.sub != "" && p.sub != "value" {
		return nil // type, primary, display
	}
	matches := func(e email) bool { return emailMatches(p.filter, e) }
	if op == "remove" {
		kept := s.emails[:0]
		for _, e := range s.emails {
			if !matches(e) {
				kept = append(kept, e)
			}
		}
		s.emails = kept
		return nil
	}
	if p.sub == "value" { // emails[type eq "work"].value or emails.value
		var value string
		if err := setString(&value, "emails.value", v); err != nil {
			return err
		}
		found := false
		for i := range s.emails {
			if matches(s.emails[i]) {
				s.emails[i].value, found = value, true
			}
		}
		if !found {
			e := email{value: value, typ: "work", primary: len(s.emails) == 0}
			for _, f := range p.filter {
				switch {
				case f.path.is("type", ""):
					e.typ = f.value
				case f.path.is("primary", ""):
					e.primary = f.valueIsTrue()
				}
			}
			s.emails = append(s.emails, e)
		}
		return nil
	}
	list, err := emailList(v)
	if err != nil {
		return err
	}
	if p.filter != nil { // emails[type eq "work"] with an address object
		kept := s.emails[:0]
		for _, e := range s.emails {
			if !matches(e) {
				kept = append(kept, e)
			}
		}
		s.emails = append(kept, list...)
		return nil
	}
	if op == "replace" {
		s.emails = list
		return nil
	}
	for _, e := range list { // add: an address of the same type is replaced
		kept := s.emails[:0]
		for _, old := range s.emails {
			if e.typ == "" || !strings.EqualFold(old.typ, e.typ) {
				kept = append(kept, old)
			}
		}
		s.emails = append(kept, e)
	}
	return nil
}

// emailMatches reports whether e matches the value filter of a path (no
// filter matches every address).
func emailMatches(filter []cond, e email) bool {
	for _, f := range filter {
		switch {
		case f.path.is("type", ""):
			if !strings.EqualFold(e.typ, f.value) {
				return false
			}
		case f.path.is("primary", ""):
			if e.primary != f.valueIsTrue() {
				return false
			}
		case f.path.is("value", ""):
			if !strings.EqualFold(e.value, f.value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// emailList converts an emails value: a list of address objects, or one.
func emailList(v any) ([]email, error) {
	var items []any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		items = x
	case map[string]any:
		items = []any{x}
	default:
		return nil, invalidValue("emails must be a list of objects with value, type and primary.")
	}
	var out []email
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, invalidValue("emails must be a list of objects with value, type and primary.")
		}
		var e email
		for k, val := range m {
			var err error
			switch strings.ToLower(k) {
			case "value":
				err = setString(&e.value, "emails.value", val)
			case "type":
				err = setString(&e.typ, "emails.type", val)
			case "primary":
				e.primary, err = boolValue(val)
			}
			if err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(e.value) != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ values

// setString stores a string attribute value; null clears it. Numbers are
// accepted as written.
func setString(field *string, name string, v any) error {
	switch x := v.(type) {
	case nil:
		*field = ""
	case string:
		*field = x
	case json.Number:
		*field = x.String()
	case bool:
		*field = strconv.FormatBool(x)
	default:
		return invalidValue("%s must be a string.", name)
	}
	return nil
}

// boolValue converts a boolean attribute value: true/false, or the strings
// "True"/"False" that Microsoft Entra ID sends without aadOptscim062020.
func boolValue(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
			return b, nil
		}
	}
	return false, invalidValue("active must be true or false, not %v.", v)
}

// checkValue rejects values that cannot be stored or shown.
func checkValue(name, v string) error {
	if len([]rune(v)) > maxValueLength {
		return invalidValue("%s is too long (at most %d characters).", name, maxValueLength)
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return invalidValue("%s must not contain control characters.", name)
	}
	return nil
}

// looksLikeEmail is a deliberately loose check for "local@domain.tld".
func looksLikeEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && strings.Contains(domain, ".") && !strings.Contains(domain, "@") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") &&
		!strings.ContainsFunc(s, unicode.IsSpace)
}
