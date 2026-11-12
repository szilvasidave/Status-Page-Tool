package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

const serviceCols = `sv.id, sv.name, sv.description, sv.category, sv.sort_order, sv.created_at`

func scanService(sc scanner) (Service, error) {
	var (
		svc     Service
		created int64
	)
	err := sc.Scan(&svc.ID, &svc.Name, &svc.Description, &svc.Category, &svc.SortOrder, &created)
	svc.CreatedAt = fromUnix(created)
	return svc, err
}

// listServices returns the services matching where (SQL with alias sv, may
// be empty) in status page order, with GroupIDs filled.
func (s *Store) listServices(ctx context.Context, where string, args ...any) ([]Service, error) {
	services, err := queryAll(ctx, s.db, scanService, `SELECT `+serviceCols+` FROM services sv `+where+` ORDER BY `+serviceOrderSQL, args...)
	if err != nil || len(services) == 0 {
		return services, err
	}
	ids := make([]int64, len(services))
	for i, svc := range services {
		ids[i] = svc.ID
	}
	in, inArgs := inList(ids)
	type link struct{ service, group int64 }
	links, err := queryAll(ctx, s.db, func(sc scanner) (link, error) {
		var l link
		return l, sc.Scan(&l.service, &l.group)
	}, `SELECT service_id, group_id FROM service_groups WHERE service_id IN (`+in+`) ORDER BY group_id`, inArgs...)
	if err != nil {
		return nil, err
	}
	byService := make(map[int64][]int64)
	for _, l := range links {
		byService[l.service] = append(byService[l.service], l.group)
	}
	for i := range services {
		services[i].GroupIDs = byService[services[i].ID]
	}
	return services, nil
}

// ListServices returns all services ordered by category, sort_order, name,
// with GroupIDs filled.
func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	services, err := s.listServices(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	return services, nil
}

// GetService returns one service (GroupIDs filled) or ErrNotFound.
func (s *Store) GetService(ctx context.Context, id int64) (*Service, error) {
	services, err := s.listServices(ctx, `WHERE sv.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("get service %d: %w", id, err)
	}
	if len(services) == 0 {
		return nil, ErrNotFound
	}
	return &services[0], nil
}

// setServiceGroups replaces the groups of a service. Unknown group ids are
// ignored.
func setServiceGroups(ctx context.Context, tx *db.Tx, serviceID int64, groupIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM service_groups WHERE service_id = ?`, serviceID); err != nil {
		return err
	}
	for _, gid := range uniqueIDs(groupIDs) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_groups (service_id, group_id) SELECT ?, id FROM idp_groups WHERE id = ?`, serviceID, gid); err != nil {
			return err
		}
	}
	return nil
}

// CreateService inserts svc (ID and CreatedAt are set on svc) together with
// its GroupIDs (unknown group ids are ignored).
func (s *Store) CreateService(ctx context.Context, svc *Service) error {
	if strings.TrimSpace(svc.Name) == "" {
		return errors.New("create service: the name must not be empty")
	}
	now := nowTime()
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO services (name, description, category, sort_order, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			svc.Name, svc.Description, svc.Category, svc.SortOrder, now.Unix()).Scan(&id)
		if err != nil {
			return err
		}
		if err := setServiceGroups(ctx, tx, id, svc.GroupIDs); err != nil {
			return err
		}
		svc.ID = id
		return nil
	})
	if err != nil {
		return fmt.Errorf("create service %q: %w", svc.Name, err)
	}
	svc.CreatedAt = now
	return nil
}

// UpdateService updates name, description, category, sort order and replaces
// the GroupIDs of svc. Returns ErrNotFound for an unknown service.
func (s *Store) UpdateService(ctx context.Context, svc *Service) error {
	if strings.TrimSpace(svc.Name) == "" {
		return errors.New("update service: the name must not be empty")
	}
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		err := affected(tx.ExecContext(ctx,
			`UPDATE services SET name = ?, description = ?, category = ?, sort_order = ? WHERE id = ?`,
			svc.Name, svc.Description, svc.Category, svc.SortOrder, svc.ID))
		if err != nil {
			return err
		}
		return setServiceGroups(ctx, tx, svc.ID, svc.GroupIDs)
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("update service %d: %w", svc.ID, err)
	}
	return err
}

// DeleteService deletes a service (and its incident/maintenance links).
// Returns ErrNotFound for an unknown service.
func (s *Store) DeleteService(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete service %d: %w", id, err)
	}
	return err
}
