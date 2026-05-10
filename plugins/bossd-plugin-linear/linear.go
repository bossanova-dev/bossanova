package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// issueNumberRe matches the first contiguous run of digits in a query so we
// can recover an issue number from inputs like "1181" or "FRE-1181". Linear
// stores per-team integer numbers, not the prefixed identifier, so this is the
// piece we can actually push down to the API.
var issueNumberRe = regexp.MustCompile(`\d+`)

// extractIssueNumber pulls the first integer out of query, returning ok=false
// when no digits are present or the run overflows int (e.g. a 20-digit blob).
func extractIssueNumber(query string) (int, bool) {
	match := issueNumberRe.FindString(query)
	if match == "" {
		return 0, false
	}
	n, err := strconv.Atoi(match)
	if err != nil {
		return 0, false
	}
	return n, true
}

// httpClient is used for Linear API requests with a 30-second timeout to prevent
// hung connections from blocking the TUI indefinitely.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// defaultLinearEndpoint is the production Linear GraphQL endpoint.
const defaultLinearEndpoint = "https://api.linear.app/graphql"

// linearClient is a GraphQL client for the Linear API.
type linearClient struct {
	apiKey   string
	endpoint string
}

// newLinearClient creates a new Linear client with the given API key. The
// endpoint is resolved by resolveLinearEndpoint, which returns the production
// endpoint in normal builds; under the `e2e` build tag it additionally
// honours LINEAR_API_ENDPOINT so integration tests can route traffic to a
// local httptest.Server. Keeping the override behind a build tag ensures the
// shipped plugin binary has no env-var redirect surface.
func newLinearClient(apiKey string) *linearClient {
	return &linearClient{
		apiKey:   apiKey,
		endpoint: resolveLinearEndpoint(),
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
//
// When titleQuery is non-empty, a case-insensitive substring filter on title
// is pushed to the API so the search reaches issues outside Linear's default
// first page. If titleQuery also contains digits, the integer is OR'd in as
// a number filter so searching by issue number (e.g. "1181" or "FRE-1181")
// finds the matching issue even when the title doesn't contain those digits.
// An empty titleQuery returns the most recently updated active issues
// (Linear's default page size, currently 50).
func (c *linearClient) FetchIssues(ctx context.Context, titleQuery string) ([]linearIssue, error) {
	// GraphQL query to fetch issues across all accessible teams, filtered by
	// state type and (optionally) by title and/or issue number. The filter
	// shape is built dynamically below so we can omit clauses Linear would
	// otherwise reject (a number filter has no null form). state.type uses
	// the stable values triage / backlog / unstarted / started / completed /
	// canceled.
	query := `
		query Issues($filter: IssueFilter!) {
			issues(filter: $filter, orderBy: updatedAt) {
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

	filter := map[string]any{
		"state": map[string]any{
			"type": map[string]any{
				"in": []string{"unstarted", "started"},
			},
		},
	}
	if titleQuery != "" {
		titleClause := map[string]any{
			"title": map[string]any{"containsIgnoreCase": titleQuery},
		}
		// When the query contains digits (a bare number like "1181" or an
		// identifier like "FRE-1181"), OR a number filter alongside the title
		// match. Without this, typing an issue number wipes the cached row
		// because Linear titles rarely contain the number itself.
		if num, ok := extractIssueNumber(titleQuery); ok {
			filter["or"] = []map[string]any{
				titleClause,
				{"number": map[string]any{"eq": num}},
			}
		} else {
			// Lift the single title clause to the top level — wrapping it in
			// a one-element `or` is valid but pointlessly noisy.
			filter["title"] = titleClause["title"]
		}
	}

	reqBody := graphqlRequest{
		Query:     query,
		Variables: map[string]any{"filter": filter},
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

	resp, err := httpClient.Do(req)
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
