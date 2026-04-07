package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// linearClient is a GraphQL client for the Linear API.
type linearClient struct {
	apiKey   string
	endpoint string
}

// newLinearClient creates a new Linear client with the given API key.
func newLinearClient(apiKey string) *linearClient {
	return &linearClient{
		apiKey:   apiKey,
		endpoint: "https://api.linear.app/graphql",
	}
}

// linearIssue represents an issue fetched from Linear.
type linearIssue struct {
	Identifier  string `json:"identifier"` // e.g., "ENG-123"
	Title       string `json:"title"`
	Description string `json:"description"`
	BranchName  string `json:"branchName"`
	URL         string `json:"url"`
	State       string `json:"state"`
}

// graphqlRequest is the structure for a GraphQL query request.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlResponse is the structure for a GraphQL query response.
type graphqlResponse struct {
	Data   *graphqlData   `json:"data,omitempty"`
	Errors []graphqlError `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlData struct {
	Issues struct {
		Nodes []struct {
			Identifier  string `json:"identifier"`
			Title       string `json:"title"`
			Description string `json:"description"`
			BranchName  string `json:"branchName"`
			URL         string `json:"url"`
			State       struct {
				Name string `json:"name"`
			} `json:"state"`
		} `json:"nodes"`
	} `json:"issues"`
}

// FetchIssues retrieves issues from Linear filtered by workflow state type.
// Uses the stable state type field ("unstarted", "started") rather than
// customizable display names which vary across workspaces.
func (c *linearClient) FetchIssues(ctx context.Context) ([]linearIssue, error) {
	// GraphQL query to fetch issues across all accessible teams, filtered by state type.
	// The state.type field uses stable values: triage, backlog, unstarted, started, completed, canceled.
	query := `
		{
			issues(
				filter: {
					state: { type: { in: ["unstarted", "started"] } }
				}
				orderBy: updatedAt
			) {
				nodes {
					identifier
					title
					description
					branchName
					url
					state {
						name
					}
				}
			}
		}
	`

	reqBody := graphqlRequest{
		Query: query,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Linear uses the API key directly in the Authorization header (no "Bearer" prefix for personal API keys)
	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("linear API error (status %d): %s", resp.StatusCode, string(body))
	}

	var graphqlResp graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&graphqlResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(graphqlResp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL errors: %s", graphqlResp.Errors[0].Message)
	}

	if graphqlResp.Data == nil {
		return nil, fmt.Errorf("no data in response")
	}

	// Map the GraphQL response to linearIssue structs
	nodes := graphqlResp.Data.Issues.Nodes
	issues := make([]linearIssue, 0, len(nodes))
	for _, node := range nodes {
		issues = append(issues, linearIssue{
			Identifier:  node.Identifier,
			Title:       node.Title,
			Description: node.Description,
			BranchName:  node.BranchName,
			URL:         node.URL,
			State:       node.State.Name,
		})
	}

	return issues, nil
}
