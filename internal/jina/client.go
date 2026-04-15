package jina

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/raymondkneipp/aggregate/internal/domain"
)

type Client struct {
	apiKey string
	http   *http.Client
}

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searchResponse struct {
	Code int      `json:"code"`
	Data []Result `json:"data"`
}

func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Search(ctx context.Context, site, role string) ([]domain.RawJob, error) {
	q := fmt.Sprintf(`site:%s "%s"`, site, role)
	u := "https://s.jina.ai/?" + url.Values{"q": {q}}.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Respond-With", "no-content")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina request: %w", err)
	}
	defer resp.Body.Close()

	var result searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	jobs := make([]domain.RawJob, 0, len(result.Data))
	for _, d := range result.Data {
		if d.URL == "" {
			continue
		}
		jobs = append(jobs, domain.RawJob{
			URL:   d.URL,
			Title: d.Title,
			Site:  site,
			Role:  role,
		})
	}
	return jobs, nil
}

func (c *Client) SearchByURL(ctx context.Context, jobURL string) ([]Result, error) {
	u := "https://r.jina.ai/" + jobURL // Jina reader API — fetches full page content

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}
