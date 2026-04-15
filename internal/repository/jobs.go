package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/domain"
)

type JobRepository struct {
	db *pgxpool.Pool
}

func NewJobRepository(db *pgxpool.Pool) *JobRepository {
	return &JobRepository{db: db}
}

// Upsert inserts a job or updates it if URL already exists.
// Returns true if this was a new record (not a duplicate).
func (r *JobRepository) Upsert(ctx context.Context, job *domain.Job) (bool, error) {
	query := `
		INSERT INTO jobs (url, title, company, seniority, remote, salary_min, salary_max, skills, location, enriched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (url) DO UPDATE SET
			title      = EXCLUDED.title,
			company    = EXCLUDED.company,
			seniority  = EXCLUDED.seniority,
			remote     = EXCLUDED.remote,
			salary_min = EXCLUDED.salary_min,
			salary_max = EXCLUDED.salary_max,
			skills     = EXCLUDED.skills,
			location   = EXCLUDED.location,
			enriched_at = NOW()
		WHERE jobs.enriched_at IS NULL  -- only re-enrich if not yet enriched
		RETURNING (xmax = 0) AS inserted
	`
	var inserted bool
	err := r.db.QueryRow(ctx, query,
		job.URL, job.Title, job.Company, job.Seniority, job.Remote,
		job.SalaryMin, job.SalaryMax, job.Skills, job.Location,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// URL already exists and was already enriched — skip silently
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("upsert job: %w", err)
	}
	return inserted, nil
}

type ListParams struct {
	Seniority string
	Remote    string
	Company   string
	Limit     int
	Offset    int
}

func (r *JobRepository) List(ctx context.Context, p ListParams) ([]domain.Job, error) {
	if p.Limit == 0 || p.Limit > 100 {
		p.Limit = 20
	}

	query := `
		SELECT id, url, title, company, seniority, remote,
		       salary_min, salary_max, skills, location, created_at
		FROM jobs
		WHERE ($1 = '' OR seniority = $1)
		  AND ($2 = '' OR remote    = $2)
		  AND ($3 = '' OR company ILIKE '%' || $3 || '%')
		ORDER BY created_at DESC
		LIMIT $4 OFFSET $5
	`
	rows, err := r.db.Query(ctx, query, p.Seniority, p.Remote, p.Company, p.Limit, p.Offset)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []domain.Job
	for rows.Next() {
		var j domain.Job
		if err := rows.Scan(
			&j.ID, &j.URL, &j.Title, &j.Company, &j.Seniority, &j.Remote,
			&j.SalaryMin, &j.SalaryMax, &j.Skills, &j.Location, &j.CreatedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (r *JobRepository) GetByID(ctx context.Context, id string) (*domain.Job, error) {
	query := `
		SELECT id, url, title, company, seniority, remote,
		       salary_min, salary_max, skills, location, created_at
		FROM jobs WHERE id = $1
	`
	var j domain.Job
	err := r.db.QueryRow(ctx, query, id).Scan(
		&j.ID, &j.URL, &j.Title, &j.Company, &j.Seniority, &j.Remote,
		&j.SalaryMin, &j.SalaryMax, &j.Skills, &j.Location, &j.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return &j, nil
}
