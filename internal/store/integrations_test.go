package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestIntegrations(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		b := &Integration{Name: "b Prometheus", Provider: "alertmanager", Impact: StatusDegraded, Notify: true}
		check(t, s.CreateIntegration(ctx, b, "hash-b"))
		if b.ID == 0 || !recent(b.CreatedAt) || !b.TokenCreatedAt.Equal(b.CreatedAt) {
			t.Fatalf("fields not set: %+v", b)
		}
		a := &Integration{Name: "A Kuma", Provider: "uptimekuma"}
		check(t, s.CreateIntegration(ctx, a, "hash-a"))

		got, err := s.GetIntegration(ctx, b.ID)
		check(t, err)
		equal(t, "stored", *got, *b)
		got, err = s.IntegrationByTokenHash(ctx, "hash-a")
		check(t, err)
		equal(t, "by token", got.ID, a.ID)
		_, err = s.IntegrationByTokenHash(ctx, "hash-x")
		wantNotFound(t, "unknown token", err)
		_, err = s.IntegrationByTokenHash(ctx, "")
		wantNotFound(t, "no token", err)
		_, err = s.GetIntegration(ctx, 999)
		wantNotFound(t, "unknown id", err)

		all, err := s.ListIntegrations(ctx)
		check(t, err)
		equal(t, "by name", []string{all[0].Name, all[1].Name}, []string{"A Kuma", "b Prometheus"})

		// validation
		wantError(t, "no name", s.CreateIntegration(ctx, &Integration{Name: " ", Provider: "x"}, "h"))
		wantError(t, "no provider", s.CreateIntegration(ctx, &Integration{Name: "x"}, "h"))
		wantError(t, "bad impact", s.CreateIntegration(ctx, &Integration{Name: "x", Provider: "x", Impact: StatusMaintenance}, "h"))
		wantError(t, "no token", s.CreateIntegration(ctx, &Integration{Name: "x", Provider: "x"}, ""))
		wantError(t, "same token", s.CreateIntegration(ctx, &Integration{Name: "x", Provider: "x"}, "hash-a"))

		// update
		a.Name, a.Provider, a.Impact, a.Notify, a.Paused = "Kuma", "generic", StatusMajorOutage, true, true
		check(t, s.UpdateIntegration(ctx, a))
		got, err = s.GetIntegration(ctx, a.ID)
		check(t, err)
		equal(t, "updated", []any{got.Name, got.Provider, got.Impact, got.Notify, got.Paused}, []any{"Kuma", "generic", StatusMajorOutage, true, true})
		wantNotFound(t, "update unknown", s.UpdateIntegration(ctx, &Integration{ID: 999, Name: "x", Provider: "x"}))

		// new token: the old one stops working
		check(t, s.SetIntegrationToken(ctx, a.ID, "hash-a2"))
		_, err = s.IntegrationByTokenHash(ctx, "hash-a")
		wantNotFound(t, "old token", err)
		got, err = s.IntegrationByTokenHash(ctx, "hash-a2")
		check(t, err)
		equal(t, "new token", got.ID, a.ID)
		wantNotFound(t, "token of unknown", s.SetIntegrationToken(ctx, 999, "hash-z"))

		// delete: alerts and events go, the incidents stay
		mustService(t, s, "VPN", "", 0)
		apply(t, s, b, firing("k", "VPN", "VPN down", StatusMajorOutage))
		check(t, s.AddIntegrationEvent(ctx, &IntegrationEvent{IntegrationID: b.ID, Outcome: EventChanged, Summary: "x"}))
		check(t, s.DeleteIntegration(ctx, b.ID))
		wantNotFound(t, "delete again", s.DeleteIntegration(ctx, b.ID))
		alerts, err := s.ListIncidentAlerts(ctx, 1)
		check(t, err)
		equal(t, "alerts deleted", len(alerts), 0)
		evs, err := s.ListIntegrationEvents(ctx, b.ID, 10)
		check(t, err)
		equal(t, "events deleted", len(evs), 0)
		equal(t, "incident kept open", mustIncident(t, s, 1).Status, IncidentInvestigating)
	})
}

func TestIntegrationEvents(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		in := mustIntegration(t, s, "Prometheus", false)
		other := mustIntegration(t, s, "Other", false)
		check(t, s.AddIntegrationEvent(ctx, &IntegrationEvent{IntegrationID: other.ID, Outcome: EventTest, Summary: "other"}))
		for i := range KeepIntegrationEvents + 5 {
			check(t, s.AddIntegrationEvent(ctx, &IntegrationEvent{IntegrationID: in.ID, Outcome: EventChanged, Summary: fmt.Sprint(i)}))
		}
		evs, err := s.ListIntegrationEvents(ctx, in.ID, 1000)
		check(t, err)
		equal(t, "kept", len(evs), KeepIntegrationEvents)
		equal(t, "newest first", evs[0].Summary, fmt.Sprint(KeepIntegrationEvents+4))
		equal(t, "oldest kept", evs[len(evs)-1].Summary, "5")
		evs, err = s.ListIntegrationEvents(ctx, other.ID, 1000)
		check(t, err)
		equal(t, "other integration untouched", len(evs), 1)
		got, err := s.GetIntegration(ctx, in.ID)
		check(t, err)
		if got.LastEventAt == nil || !recent(*got.LastEventAt) {
			t.Fatalf("last event: %v", got.LastEventAt)
		}

		// the payload is cut at a character boundary
		payload := strings.Repeat("ä", MaxEventPayload) // 2 bytes each
		ev := &IntegrationEvent{IntegrationID: in.ID, Outcome: EventInvalid, Summary: "big", Payload: payload}
		check(t, s.AddIntegrationEvent(ctx, ev))
		evs, err = s.ListIntegrationEvents(ctx, in.ID, 1)
		check(t, err)
		kept, note, _ := strings.Cut(evs[0].Payload, "\n… (shortened")
		equal(t, "payload length", len(kept), MaxEventPayload)
		equal(t, "payload", kept, strings.Repeat("ä", MaxEventPayload/2))
		equal(t, "says it was shortened", note, ": the first 8 KB of 16384 bytes are kept)")
	})
}

// Concurrent requests with alerts of one service open one incident.
func TestApplyAlertsConcurrent(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Prometheus", false)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.ApplyAlerts(ctx, in, []AlertInput{firing(fmt.Sprint(i), "VPN", fmt.Sprint("alert ", i), StatusDegraded)})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			check(t, err)
		}
		open, err := s.ListOpenIncidents(ctx)
		check(t, err)
		equal(t, "one incident", len(open), 1)
		alerts, err := s.ListIncidentAlerts(ctx, open[0].ID)
		check(t, err)
		equal(t, "with all alerts", len(alerts), 8)
		equal(t, "status", currentStatus(t, s, vpn.ID), StatusDegraded)
	})
}
