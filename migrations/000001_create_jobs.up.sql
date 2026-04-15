CREATE TABLE jobs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url         TEXT UNIQUE NOT NULL,
    title       TEXT,
    company     TEXT,
    seniority   TEXT,
    remote      TEXT,
    salary_min  INT,
    salary_max  INT,
    skills      TEXT[],
    location    TEXT,
    raw_content TEXT,
    enriched_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_jobs_seniority  ON jobs(seniority);
CREATE INDEX idx_jobs_remote     ON jobs(remote);
CREATE INDEX idx_jobs_created_at ON jobs(created_at DESC);
CREATE INDEX idx_jobs_company    ON jobs(company);
