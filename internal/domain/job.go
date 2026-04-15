package domain

import "time"

type RawJob struct {
	URL   string
	Title string
	Site  string
	Role  string // The query that found it
}

type Job struct {
	ID         string
	URL        string
	Title      string
	Company    string
	Seniority  string // junior / mid / senior / staff / unknown
	Remote     string // remote / hybrid / onsite / unknown
	SalaryMin  *int
	SalaryMax  *int
	Skills     []string
	Location   string
	RawContent string
	EnrichedAt *time.Time
	CreatedAt  time.Time
}

type SQSMessage struct {
	URL       string `json:"url"`
	Site      string `json:"site"`
	RoleQuery string `json:"role_query"`
}
