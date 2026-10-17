// Package dbrepo provides the PostgreSQL-backed implementations of the
// persistence repository interfaces. It is the infrastructure adapter
// layer: it may import pgx, and it translates driver failures into the
// domain errors defined in internal/persistence (WBS 2.24).
package dbrepo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/project"
)

// projectColumns is the canonical projection of the projects table.
const projectColumns = `id, name, description, status, version, created_at, updated_at`

// ProjectRepository implements persistence.ProjectRepository on PostgreSQL.
type ProjectRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.ProjectRepository = (*ProjectRepository)(nil)

// NewProjectRepository binds the repository to an existing pool.
func NewProjectRepository(pool *pgxpool.Pool) *ProjectRepository {
	return &ProjectRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21); the domain-level transaction surface
// is all the caller sees.
func (r *ProjectRepository) WithTx(tx persistence.Transaction) persistence.ProjectRepository {
	return &ProjectRepository{exec: txExecutor{tx}}
}

// TranslateDBError maps a pgx/pgconn failure to the domain error set
// (WBS 2.24). no rows -> ErrNotFound, unique violation -> ErrConflict,
// foreign-key violation -> ErrInvalidReference, anything else is wrapped
// in *persistence.InfrastructureError so no driver type escapes.
func TranslateDBError(op string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return persistence.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return persistence.ErrConflict
		case "23503":
			return persistence.ErrInvalidReference
		}
	}
	return &persistence.InfrastructureError{Op: op, Cause: err}
}

func scanProject(row interface{ Scan(dest ...any) error }) (*project.Project, error) {
	var (
		p      project.Project
		status string
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &status, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	s, err := project.ParseStatus(status)
	if err != nil {
		return nil, err
	}
	p.Status = s
	return &p, nil
}

// Create inserts a new project (T2.4.1).
func (r *ProjectRepository) Create(ctx context.Context, p *project.Project) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO projects (id, name, description, status, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.Name, p.Description, p.Status.String(), p.Version, p.CreatedAt.UTC(), p.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create project", err)
	}
	return nil
}

// Get returns the project with the given id (T2.4.2, T2.4.3).
func (r *ProjectRepository) Get(ctx context.Context, id string) (*project.Project, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id)
	p, err := scanProject(row)
	if err != nil {
		return nil, TranslateDBError("get project", err)
	}
	return p, nil
}

// Update persists changed fields and refreshes UpdatedAt (T2.4.4).
func (r *ProjectRepository) Update(ctx context.Context, p *project.Project) error {
	p.UpdatedAt = time.Now().UTC()
	tag, err := r.exec.Exec(ctx,
		`UPDATE projects SET name = $2, description = $3, status = $4, updated_at = $5 WHERE id = $1`,
		p.ID, p.Name, p.Description, p.Status.String(), p.UpdatedAt)
	if err != nil {
		return TranslateDBError("update project", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// UpdateWithVersion applies an optimistic-concurrency update (WBS 3.24).
// The row is only written when its stored version equals expectedVersion; on
// success the version is incremented atomically with the field change so a
// stale writer can never win. A missing project is ErrNotFound; a stale
// version (row matched by id but not by version) is ErrConflict.
func (r *ProjectRepository) UpdateWithVersion(ctx context.Context, p *project.Project, expectedVersion int) error {
	p.UpdatedAt = time.Now().UTC()
	tag, err := r.exec.Exec(ctx,
		`UPDATE projects
		 SET name = $2, description = $3, status = $4, version = version + 1, updated_at = $5
		 WHERE id = $1 AND version = $6`,
		p.ID, p.Name, p.Description, p.Status.String(), p.UpdatedAt, expectedVersion)
	if err != nil {
		return TranslateDBError("update project with version", err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "not found" from "stale version" by probing the row.
		var exists bool
		if err := r.exec.QueryRow(ctx,
			`SELECT true FROM projects WHERE id = $1`, p.ID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return persistence.ErrNotFound
			}
			return TranslateDBError("probe project", err)
		}
		if !exists {
			return persistence.ErrNotFound
		}
		return persistence.ErrConflict
	}
	return nil
}

// Delete removes the project (T2.4.5); dependents cascade (WBS 2.22).
func (r *ProjectRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete project", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// List returns projects ordered by updated_at DESC with an id tiebreak
// (deterministic default ordering, WBS 3.22). An empty status filter
// selects all projects; Limit/Offset implement pagination.
func (r *ProjectRepository) List(ctx context.Context, filter persistence.ProjectListFilter) ([]*project.Project, error) {
	query := `SELECT ` + projectColumns + ` FROM projects`
	args := []any{}
	if filter.Status != "" {
		args = append(args, filter.Status)
		query += ` WHERE status = $` + itoa(len(args))
	}
	query += ` ORDER BY updated_at DESC, id`
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		query += ` LIMIT $` + itoa(len(args))
	}
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		query += ` OFFSET $` + itoa(len(args))
	}
	rows, err := r.exec.Query(ctx, query, args...)
	if err != nil {
		return nil, TranslateDBError("list projects", err)
	}
	defer rows.Close()
	var out []*project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, TranslateDBError("scan project", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list projects", err)
	}
	return out, nil
}
