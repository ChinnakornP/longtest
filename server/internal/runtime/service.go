package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
)

// DefaultOnlineWithin is the contract's liveness window: a runtime that has
// not heartbeated for longer than this is offline, and the runs it was holding
// are finished with a reason.
const DefaultOnlineWithin = 30 * time.Second

// Service is the domain layer for runtimes.
type Service struct {
	store        auth.Store
	registry     *realtime.Registry
	onlineWithin time.Duration
}

// NewService returns the runtime service. The registry is consulted alongside
// the heartbeat window: a daemon whose control-plane socket is attached to this
// process is online now, whatever its last stored heartbeat says.
func NewService(store auth.Store, registry *realtime.Registry, onlineWithin time.Duration) *Service {
	if onlineWithin <= 0 {
		onlineWithin = DefaultOnlineWithin
	}
	return &Service{store: store, registry: registry, onlineWithin: onlineWithin}
}

// View is a runtime as the API renders it.
type View struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Online is derived, never stored. See the package doc.
	Online     bool            `json:"online"`
	Version    string          `json:"version"`
	LastSeenAt *time.Time      `json:"lastSeenAt,omitempty"`
	DisabledAt *time.Time      `json:"disabledAt,omitempty"`
	Browsers   json.RawMessage `json:"browsers"`
	Agents     json.RawMessage `json:"agents"`
	HostInfo   json.RawMessage `json:"hostInfo"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// List returns every runtime in the caller's organization, ordered by name.
func (s *Service) List(ctx context.Context, scope auth.OrgScope) ([]View, error) {
	rows, err := s.store.ListRuntimes(ctx, dbgen.ListRuntimesParams{
		OrgID:        scope.OrgID(),
		OnlineWithin: pgtype.Interval{Microseconds: s.onlineWithin.Microseconds(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list runtimes: %w", db.Classify(err))
	}

	views := make([]View, 0, len(rows))
	for _, row := range rows {
		view := View{
			ID:        row.ID,
			Name:      row.Name,
			Version:   row.Version,
			Browsers:  jsonOr(row.Browsers, "[]"),
			Agents:    jsonOr(row.Agents, "[]"),
			HostInfo:  jsonOr(row.HostInfo, "{}"),
			CreatedAt: row.CreatedAt.Time.UTC(),
			// A live control-plane socket beats a stale heartbeat: a daemon
			// that connected two seconds ago and has not sent its first
			// heartbeat yet is reachable, and reporting it offline would make
			// "start a run" look impossible on a machine that just came up.
			Online: (row.Online.Valid && row.Online.Bool) ||
				(!row.DisabledAt.Valid && s.registry.Online(realtime.Target{OrgID: row.OrgID, RuntimeID: row.ID})),
		}
		if row.LastSeenAt.Valid {
			seen := row.LastSeenAt.Time.UTC()
			view.LastSeenAt = &seen
		}
		if row.DisabledAt.Valid {
			disabled := row.DisabledAt.Time.UTC()
			view.DisabledAt = &disabled
		}
		views = append(views, view)
	}
	return views, nil
}

// jsonOr guards against a NULL or empty jsonb column reaching a client as a
// bare `null` where it documents an array or an object.
func jsonOr(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}
