package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store defines the persistence interface for the model registry.
type Store interface {
	// Models
	ListModels(ctx context.Context, activeOnly bool) ([]Model, error)
	GetModelByName(ctx context.Context, name string) (*Model, error)
	GetModelByID(ctx context.Context, id string) (*Model, error)
	CreateModel(ctx context.Context, m *Model) error
	UpdateModel(ctx context.Context, m *Model) error
	DeleteModel(ctx context.Context, id string) error

	// Backends
	ListBackends(ctx context.Context, modelID string) ([]Backend, error)
	CreateBackend(ctx context.Context, b *Backend) error
	UpdateBackend(ctx context.Context, b *Backend) error
	DeleteBackend(ctx context.Context, id string) error
	UpdateBackendHealth(ctx context.Context, id string, healthy bool) error
}

// PGStore implements Store using PostgreSQL via pgx.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore creates a new PostgreSQL-backed registry store.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (s *PGStore) ListModels(ctx context.Context, activeOnly bool) ([]Model, error) {
	query := `
		SELECT id, name, model_id, active,
		       default_params, reasoning_config, transforms, rate_limits,
		       created_at, updated_at
		FROM models`
	if activeOnly {
		query += ` WHERE active = true`
	}
	query += ` ORDER BY name`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying models: %w", err)
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating models: %w", err)
	}

	// Load backends for each model.
	for i := range models {
		backends, err := s.ListBackends(ctx, models[i].ID)
		if err != nil {
			return nil, fmt.Errorf("loading backends for model %s: %w", models[i].ID, err)
		}
		models[i].Backends = backends
	}

	return models, nil
}

func (s *PGStore) GetModelByName(ctx context.Context, name string) (*Model, error) {
	query := `
		SELECT id, name, model_id, active,
		       default_params, reasoning_config, transforms, rate_limits,
		       created_at, updated_at
		FROM models
		WHERE name = $1`

	row := s.pool.QueryRow(ctx, query, name)
	m, err := scanModelRow(row)
	if err != nil {
		return nil, err
	}

	backends, err := s.ListBackends(ctx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("loading backends: %w", err)
	}
	m.Backends = backends
	return m, nil
}

func (s *PGStore) GetModelByID(ctx context.Context, id string) (*Model, error) {
	query := `
		SELECT id, name, model_id, active,
		       default_params, reasoning_config, transforms, rate_limits,
		       created_at, updated_at
		FROM models
		WHERE id = $1`

	row := s.pool.QueryRow(ctx, query, id)
	m, err := scanModelRow(row)
	if err != nil {
		return nil, err
	}

	backends, err := s.ListBackends(ctx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("loading backends: %w", err)
	}
	m.Backends = backends
	return m, nil
}

func (s *PGStore) CreateModel(ctx context.Context, m *Model) error {
	defaultParams, err := json.Marshal(m.DefaultParams)
	if err != nil {
		return fmt.Errorf("marshaling default_params: %w", err)
	}
	reasoningConfig, err := json.Marshal(m.ReasoningConfig)
	if err != nil {
		return fmt.Errorf("marshaling reasoning_config: %w", err)
	}
	transforms, err := json.Marshal(m.Transforms)
	if err != nil {
		return fmt.Errorf("marshaling transforms: %w", err)
	}
	rateLimits, err := json.Marshal(m.RateLimits)
	if err != nil {
		return fmt.Errorf("marshaling rate_limits: %w", err)
	}

	query := `
		INSERT INTO models (name, model_id, active, default_params, reasoning_config, transforms, rate_limits)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at`

	return s.pool.QueryRow(ctx, query,
		m.Name, m.ModelID, m.Active,
		defaultParams, reasoningConfig, transforms, rateLimits,
	).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
}

func (s *PGStore) UpdateModel(ctx context.Context, m *Model) error {
	defaultParams, err := json.Marshal(m.DefaultParams)
	if err != nil {
		return fmt.Errorf("marshaling default_params: %w", err)
	}
	reasoningConfig, err := json.Marshal(m.ReasoningConfig)
	if err != nil {
		return fmt.Errorf("marshaling reasoning_config: %w", err)
	}
	transforms, err := json.Marshal(m.Transforms)
	if err != nil {
		return fmt.Errorf("marshaling transforms: %w", err)
	}
	rateLimits, err := json.Marshal(m.RateLimits)
	if err != nil {
		return fmt.Errorf("marshaling rate_limits: %w", err)
	}

	query := `
		UPDATE models
		SET name = $1, model_id = $2, active = $3,
		    default_params = $4, reasoning_config = $5,
		    transforms = $6, rate_limits = $7,
		    updated_at = now()
		WHERE id = $8
		RETURNING updated_at`

	result := s.pool.QueryRow(ctx, query,
		m.Name, m.ModelID, m.Active,
		defaultParams, reasoningConfig, transforms, rateLimits,
		m.ID,
	)
	if err := result.Scan(&m.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("model not found: %s", m.ID)
		}
		return fmt.Errorf("updating model: %w", err)
	}
	return nil
}

func (s *PGStore) DeleteModel(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM models WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting model: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("model not found: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Backends
// ---------------------------------------------------------------------------

func (s *PGStore) ListBackends(ctx context.Context, modelID string) ([]Backend, error) {
	query := `
		SELECT id, model_id, url, weight, active, healthy, last_health_check,
		       created_at, updated_at
		FROM model_backends
		WHERE model_id = $1
		ORDER BY url`

	rows, err := s.pool.Query(ctx, query, modelID)
	if err != nil {
		return nil, fmt.Errorf("querying backends: %w", err)
	}
	defer rows.Close()

	var backends []Backend
	for rows.Next() {
		var b Backend
		if err := rows.Scan(
			&b.ID, &b.ModelID, &b.URL, &b.Weight, &b.Active, &b.Healthy,
			&b.LastHealthCheck, &b.CreatedAt, &b.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning backend: %w", err)
		}
		backends = append(backends, b)
	}
	return backends, rows.Err()
}

func (s *PGStore) CreateBackend(ctx context.Context, b *Backend) error {
	query := `
		INSERT INTO model_backends (model_id, url, weight, active)
		VALUES ($1, $2, $3, $4)
		RETURNING id, healthy, created_at, updated_at`

	return s.pool.QueryRow(ctx, query,
		b.ModelID, b.URL, b.Weight, b.Active,
	).Scan(&b.ID, &b.Healthy, &b.CreatedAt, &b.UpdatedAt)
}

func (s *PGStore) UpdateBackend(ctx context.Context, b *Backend) error {
	query := `
		UPDATE model_backends
		SET url = $1, weight = $2, active = $3, updated_at = now()
		WHERE id = $4
		RETURNING updated_at`

	result := s.pool.QueryRow(ctx, query, b.URL, b.Weight, b.Active, b.ID)
	if err := result.Scan(&b.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("backend not found: %s", b.ID)
		}
		return fmt.Errorf("updating backend: %w", err)
	}
	return nil
}

func (s *PGStore) DeleteBackend(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM model_backends WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting backend: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backend not found: %s", id)
	}
	return nil
}

func (s *PGStore) UpdateBackendHealth(ctx context.Context, id string, healthy bool) error {
	query := `
		UPDATE model_backends
		SET healthy = $1, last_health_check = now(), updated_at = now()
		WHERE id = $2`

	tag, err := s.pool.Exec(ctx, query, healthy, id)
	if err != nil {
		return fmt.Errorf("updating backend health: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backend not found: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Row scanning helpers
// ---------------------------------------------------------------------------

func scanModel(rows pgx.Rows) (*Model, error) {
	var m Model
	var dp, rc, tf, rl []byte
	if err := rows.Scan(
		&m.ID, &m.Name, &m.ModelID, &m.Active,
		&dp, &rc, &tf, &rl,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning model: %w", err)
	}
	if err := json.Unmarshal(dp, &m.DefaultParams); err != nil {
		return nil, fmt.Errorf("unmarshaling default_params: %w", err)
	}
	if err := json.Unmarshal(rc, &m.ReasoningConfig); err != nil {
		return nil, fmt.Errorf("unmarshaling reasoning_config: %w", err)
	}
	if err := json.Unmarshal(tf, &m.Transforms); err != nil {
		return nil, fmt.Errorf("unmarshaling transforms: %w", err)
	}
	if err := json.Unmarshal(rl, &m.RateLimits); err != nil {
		return nil, fmt.Errorf("unmarshaling rate_limits: %w", err)
	}
	return &m, nil
}

func scanModelRow(row pgx.Row) (*Model, error) {
	var m Model
	var dp, rc, tf, rl []byte
	if err := row.Scan(
		&m.ID, &m.Name, &m.ModelID, &m.Active,
		&dp, &rc, &tf, &rl,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("model not found")
		}
		return nil, fmt.Errorf("scanning model: %w", err)
	}
	if err := json.Unmarshal(dp, &m.DefaultParams); err != nil {
		return nil, fmt.Errorf("unmarshaling default_params: %w", err)
	}
	if err := json.Unmarshal(rc, &m.ReasoningConfig); err != nil {
		return nil, fmt.Errorf("unmarshaling reasoning_config: %w", err)
	}
	if err := json.Unmarshal(tf, &m.Transforms); err != nil {
		return nil, fmt.Errorf("unmarshaling transforms: %w", err)
	}
	if err := json.Unmarshal(rl, &m.RateLimits); err != nil {
		return nil, fmt.Errorf("unmarshaling rate_limits: %w", err)
	}
	return &m, nil
}

// nowPtr returns a pointer to the current time. Useful for optional timestamps.
func nowPtr() *time.Time {
	t := time.Now()
	return &t
}
