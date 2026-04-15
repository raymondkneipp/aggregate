package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/raymondkneipp/aggregate/internal/domain"
)

type Enricher struct {
	client anthropic.Client
}

func NewEnricher(apiKey string) *Enricher {
	return &Enricher{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
	}
}

var toolSchema = anthropic.ToolInputSchemaParam{
	Properties: map[string]any{
		"title":      map[string]any{"type": "string", "description": "Job title"},
		"company":    map[string]any{"type": "string", "description": "Company name"},
		"seniority":  map[string]any{"type": "string", "enum": []string{"junior", "mid", "senior", "staff", "principal", "unknown"}},
		"remote":     map[string]any{"type": "string", "enum": []string{"remote", "hybrid", "onsite", "unknown"}},
		"salary_min": map[string]any{"type": "integer", "description": "Min annual salary USD, null if not mentioned"},
		"salary_max": map[string]any{"type": "integer", "description": "Max annual salary USD, null if not mentioned"},
		"skills":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Required technical skills"},
		"location":   map[string]any{"type": "string", "description": "City/region, or 'Remote'"},
	},
	Required: []string{"title", "company", "seniority", "remote", "skills"},
}

type enrichedFields struct {
	Title     string   `json:"title"`
	Company   string   `json:"company"`
	Seniority string   `json:"seniority"`
	Remote    string   `json:"remote"`
	SalaryMin *int     `json:"salary_min"`
	SalaryMax *int     `json:"salary_max"`
	Skills    []string `json:"skills"`
	Location  string   `json:"location"`
}

func (e *Enricher) Enrich(ctx context.Context, job domain.RawJob, content string) (*domain.Job, error) {
	prompt := fmt.Sprintf(
		"Extract structured details from this job posting.\n\nURL: %s\n\nContent:\n%s",
		job.URL, truncate(content, 4000),
	)

	resp, err := e.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		Tools: []anthropic.ToolUnionParam{
			{
				OfTool: &anthropic.ToolParam{
					Name:        "extract_job_details",
					Description: anthropic.String("Extract structured job details from a job posting"),
					InputSchema: toolSchema,
				},
			},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{
				Name: "extract_job_details",
			},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("claude API: %w", err)
	}

	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			var fields enrichedFields
			if err := json.Unmarshal([]byte(block.AsToolUse().Input), &fields); err != nil {
				return nil, fmt.Errorf("parse tool output: %w", err)
			}
			return &domain.Job{
				URL:       job.URL,
				Title:     fields.Title,
				Company:   fields.Company,
				Seniority: fields.Seniority,
				Remote:    fields.Remote,
				SalaryMin: fields.SalaryMin,
				SalaryMax: fields.SalaryMax,
				Skills:    fields.Skills,
				Location:  fields.Location,
			}, nil
		}
	}
	return nil, fmt.Errorf("no tool use block in response")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "... [truncated]"
}
