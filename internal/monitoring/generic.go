package monitoring

// The generic format: what tools whose request can be configured send, and
// scripts. One alert is a JSON object
//
//	{"service": "VPN", "status": "down", "key": "vpn-gw", "title": "…", "message": "…",
//	 "severity": "critical", "impact": "major_outage"}
//
// (keys in any case; "service" may be a list of names, tried in order),
// several alerts a JSON array of such objects or {"alerts": [ … ]}. Instead
// of JSON the fields can be sent as form fields (application/x-www-form-
// urlencoded, as PRTG and curl --data-urlencode send them) or as query
// parameters of the URL; a field can then be repeated (service).
//
// Required: service and status. The key defaults to the first service
// value, so that a script that reports one state per service needs none.
// Tools may send synonyms (genericNames: id, summary, description,
// priority, state, services); a field's own name wins over a synonym.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// genericNames are the fields of the generic format with the names they
// are read from, in order of preference (the first name is the field's
// own; the others are synonyms that tools use).
var genericNames = map[string][]string{
	"service":  {"service", "services"},
	"status":   {"status", "state"},
	"key":      {"key", "id"},
	"title":    {"title", "summary"},
	"message":  {"message", "description"},
	"severity": {"severity", "priority"},
	"impact":   {"impact"},
}

// knownName reports whether name (lower case) is a name of a field.
func knownName(name string) bool {
	for _, names := range genericNames {
		for _, n := range names {
			if n == name {
				return true
			}
		}
	}
	return false
}

// genericAlert is one alert of the generic format: its values by the names
// it was sent with (lower case; only names of genericNames).
type genericAlert map[string][]string

// values returns the values of a field, those of its own name first.
func (g genericAlert) values(field string) []string {
	var out []string
	for _, name := range genericNames[field] {
		out = append(out, g[name]...)
	}
	return out
}

// get returns the first value of a field that is not empty.
func (g genericAlert) get(field string) string {
	for _, v := range g.values(field) {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// ParseGeneric reads the generic format.
func ParseGeneric(r *Request) (*Batch, error) {
	var alerts []genericAlert
	body := bytes.TrimSpace(r.Body)
	switch {
	case r.ContentType == "multipart/form-data":
		return nil, parseErrorf("multipart requests are not supported: send JSON, form fields (application/x-www-form-urlencoded) or query parameters")
	case len(body) > 0 && (body[0] == '{' || body[0] == '['):
		var err error
		if alerts, err = genericJSON(body); err != nil {
			return nil, err
		}
	default:
		form := url.Values{}
		for k, v := range r.Query {
			form[strings.ToLower(k)] = v
		}
		for k, v := range parseForm(string(body)) {
			form[k] = v // the body wins over the query
		}
		a := genericAlert{}
		for k, v := range form {
			if knownName(k) {
				a[k] = v
			}
		}
		if len(a) == 0 {
			return nil, parseErrorf("the request contains no alert: send JSON such as {\"service\": \"VPN\", \"status\": \"down\"}, or the fields service and status as form fields or query parameters")
		}
		alerts = []genericAlert{a}
	}
	if len(alerts) == 0 {
		return nil, parseErrorf("the request contains no alert")
	}
	b := &Batch{}
	if len(alerts) > MaxAlerts {
		b.Notes = append(b.Notes, fmt.Sprintf("Only the first %d of %d alerts were read.", MaxAlerts, len(alerts)))
		alerts = alerts[:MaxAlerts]
	}
	tests := 0
	for i, g := range alerts {
		where := ""
		if len(alerts) > 1 {
			where = fmt.Sprintf("alert %d: ", i+1)
		}
		status := g.get("status")
		if status == "" {
			return nil, parseErrorf("%sthe status is missing: send firing (or problem, down, critical, warning, …) or resolved (or ok, up, recovered, …)", where)
		}
		st, implied := parseStatus(status)
		switch st {
		case stateTest:
			tests++
			continue
		case stateUnknown:
			b.Notes = append(b.Notes, fmt.Sprintf("%sstatus “%s” is neither a problem nor a recovery: ignored.", capitalize(where), status))
			continue
		}
		var targets []string
		for _, v := range g.values("service") {
			if v = strings.TrimSpace(v); v != "" {
				targets = append(targets, v)
			}
		}
		if len(targets) == 0 {
			return nil, parseErrorf("%sthe service is missing: send the name of the service (or one of its names in monitoring tools) in the field service", where)
		}
		a := Alert{
			Key:      g.get("key"),
			Firing:   st == stateFiring,
			Targets:  targets,
			Title:    g.get("title"),
			Message:  g.get("message"),
			Severity: g.get("severity"),
		}
		if a.Key == "" {
			a.Key = "service:" + strings.ToLower(oneLine(targets[0]))
		}
		if a.Severity == "" {
			a.Severity = implied
		}
		if v := g.get("impact"); v != "" {
			impact, ok := ParseImpact(v)
			if !ok {
				return nil, parseErrorf("%sunknown impact “%s”: send degraded, partial_outage or major_outage, or leave it out", where, v)
			}
			a.Impact = impact
		}
		b.Alerts = append(b.Alerts, a)
	}
	b.Test = tests > 0 && len(b.Alerts) == 0
	return b, nil
}

// genericJSON reads the alerts of a JSON body: an object, a list of objects
// or an object with "alerts".
func genericJSON(body []byte) ([]genericAlert, error) {
	var objects []map[string]json.RawMessage
	if body[0] == '[' {
		if err := json.Unmarshal(body, &objects); err != nil {
			return nil, parseErrorf("%s", jsonError(err))
		}
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, parseErrorf("%s", jsonError(err))
		}
		obj = lowerKeys(obj)
		if list, ok := obj["alerts"]; ok {
			if err := json.Unmarshal(list, &objects); err != nil {
				return nil, parseErrorf("“alerts” must be a list of objects: %s", jsonError(err))
			}
		} else {
			objects = []map[string]json.RawMessage{obj}
		}
	}
	alerts := make([]genericAlert, 0, len(objects))
	for i, obj := range objects {
		a := genericAlert{}
		for k, raw := range obj {
			name := strings.ToLower(k)
			if !knownName(name) {
				continue
			}
			var v texts
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, parseErrorf("alert %d: “%s” must be text (or, for service, a list of texts)", i+1, k)
			}
			a[name] = append(a[name], v...)
		}
		alerts = append(alerts, a)
	}
	return alerts, nil
}

// parseForm reads form fields leniently: values that are not properly
// escaped (e.g. a "%" in a message) are taken as they are. Names are
// returned in lower case.
func parseForm(s string) url.Values {
	v := url.Values{}
	for _, pair := range strings.Split(s, "&") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		v.Add(strings.ToLower(strings.TrimSpace(unescape(name))), unescape(value))
	}
	return v
}

func unescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return strings.ReplaceAll(s, "+", " ")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
