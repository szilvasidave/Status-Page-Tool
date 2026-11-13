package scim

// PATCH requests (RFC 7644 section 3.5.2). Microsoft Entra ID sends the
// operation names capitalised ("Replace") and, depending on the
// aadOptscim062020 flag, either one operation per attribute
//
//	{"op": "Replace", "path": "active", "value": "False"}
//
// or operations without path whose value maps attribute paths to values
//
//	{"op": "replace", "value": {"displayName": "Jane", "name.givenName": "Jane", "active": false}}
//
// Both are supported. Group members are removed either with
// {"op": "Remove", "path": "members", "value": [{"value": "42"}]} or with
// {"op": "remove", "path": "members[value eq \"42\"]"}.

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

// operation is one PATCH operation.
type operation struct {
	op    string // add, replace or remove
	path  string // "" = the value is an object of attribute paths and values
	value any
}

// operations returns the operations of a PATCH request body.
func operations(body map[string]any) ([]operation, error) {
	raw, ok := lookup(body, "Operations")
	if !ok {
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "The request has no Operations (schema %s).", schemaPatchOp)
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "Operations must be a list.")
	}
	ops := make([]operation, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "Every operation must be an object with op, path and value.")
		}
		var o operation
		name, _ := lookup(m, "op")
		opName, _ := name.(string)
		o.op = strings.ToLower(strings.TrimSpace(opName))
		if o.op != "add" && o.op != "replace" && o.op != "remove" {
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "The operation %q is not supported; use add, replace or remove.", opName)
		}
		if p, ok := lookup(m, "path"); ok && p != nil {
			s, ok := p.(string)
			if !ok {
				return nil, errorf(http.StatusBadRequest, "invalidPath", "path must be a string.")
			}
			o.path = strings.TrimSpace(s)
		}
		o.value, _ = lookup(m, "value")
		if o.path == "" && o.op == "remove" {
			return nil, errorf(http.StatusBadRequest, "noTarget", "A remove operation needs a path.")
		}
		ops = append(ops, o)
	}
	return ops, nil
}

// apply calls fn for the attributes the operation changes.
func (o operation) apply(fn func(op string, p path, value any) error) error {
	if o.path != "" {
		p, err := parsePath(o.path)
		if err != nil {
			return errorf(http.StatusBadRequest, "invalidPath", "The path %q is not supported: %v.", o.path, err)
		}
		return fn(o.op, p, o.value)
	}
	values, ok := o.value.(map[string]any)
	if !ok {
		return errorf(http.StatusBadRequest, "invalidValue", "An operation without path needs an object of attributes as value.")
	}
	// Sorted, so that e.g. "name" is applied before "name.givenName".
	for _, k := range slices.Sorted(maps.Keys(values)) {
		p, err := parsePath(k)
		if err != nil {
			return errorf(http.StatusBadRequest, "invalidPath", "The attribute %q is not supported: %v.", k, err)
		}
		if err := fn(o.op, p, values[k]); err != nil {
			return err
		}
	}
	return nil
}

// lookup returns the value of key in m, ignoring case (SCIM attribute names
// are case-insensitive).
func lookup(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}
