package monitoring

// PRTG Network Monitor: a notification template with "Execute HTTP Action"
// that POSTs the generic format as form fields (the payload in
// providers.go):
//
//	key=%sensorid&status=%laststatus&service=%device&service=%group&title=%device: %name&message=%message
//
// One state trigger for Down sends the template when a sensor goes Down and
// when it is no longer Down, with the sensor's current status (%laststatus;
// before PRTG 20.1.57 %status, e.g. "Down ended (now: Warning)" or "Down
// (was: Up)"). So the status is read as the state of the sensor: Down in
// any form – Down, Down (Partial) in a failover cluster (a partial outage),
// Down (Acknowledged) – is a problem, and every other status (Up, Warning,
// Unusual, Paused, Unknown, None) ends it: a sensor that leaves Down for
// Warning sends nothing more, so Warning cannot be a problem of its own.
// The test of a notification template sends the placeholders unfilled
// (status %laststatus, key %sensorid): a test.

import (
	"strings"
	"unicode"
)

// prtgOther are the first words of PRTG's statuses other than Down, in
// lower case.
var prtgOther = map[string]bool{"up": true, "warning": true, "unusual": true, "paused": true, "unknown": true, "none": true}

// ParsePRTG reads the HTTP action of PRTG.
func ParsePRTG(r *Request) (*Batch, error) {
	return parseGeneric(r, dialect{read: prtgRead, test: prtgTest})
}

// prtgRead reads the status of a sensor as its current state: Down (any
// form) firing, PRTG's other statuses resolved, other words ignored (e.g.
// a placeholder that PRTG did not fill in). The severity is the field
// severity, else "partial" for Down (Partial).
func prtgRead(g genericAlert) (state, string, string) {
	status := strings.ToLower(g.get("status"))
	if i := strings.LastIndex(status, "(now:"); i >= 0 { // "Down ended (now: Warning)"
		status = strings.TrimRight(status[i+len("(now:"):], ") ")
	} else if i := strings.Index(status, "(was:"); i >= 0 { // "Down (was: Up)"
		status = status[:i]
	}
	word := strings.FieldsFunc(status, func(r rune) bool { return !unicode.IsLetter(r) })
	switch {
	case len(word) == 0:
	case word[0] == "down":
		severity := g.get("severity")
		if severity == "" && strings.Contains(status, "partial") {
			severity = "partial"
		}
		return stateFiring, severity, ""
	case prtgOther[word[0]]:
		return stateResolved, "", ""
	case word[0] == "test" || word[0] == "testing":
		return stateTest, "", ""
	}
	return stateUnknown, "", ""
}

// prtgTest reports the test of a notification template: PRTG sends the
// placeholders as they are written, without the sensor's values.
func prtgTest(g genericAlert) bool {
	return strings.HasPrefix(g.get("status"), "%") && strings.HasPrefix(g.get("key"), "%")
}
