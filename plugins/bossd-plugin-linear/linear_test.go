package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIssues_Success(t *testing.T) {
	// Mock Linear API response
	mockResponse := graphqlResponse{
		Data: &graphqlData{
			Issues: struct {
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
			}{
				Nodes: []struct {
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
					BranchName  string `json:"branchName"`
					URL         string `json:"url"`
					State       struct {
						Name string `json:"name"`
					} `json:"state"`
				}{
					{
						Identifier:  "ENG-123",
						Title:       "Fix login bug",
						Description: "Users cannot log in",
						BranchName:  "eng-123-fix-login",
						URL:         "https://linear.app/issue/ENG-123",
						State: struct {
							Name string `json:"name"`
						}{Name: "In Progress"},
					},
					{
						Identifier:  "ENG-124",
						Title:       "Add dark mode",
						Description: "Implement dark mode toggle",
						BranchName:  "eng-124-dark-mode",
						URL:         "https://linear.app/issue/ENG-124",
						State: struct {
							Name string `json:"name"`
						}{Name: "Todo"},
					},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-api-key" {
			t.Errorf("Expected Authorization header 'test-api-key', got '%s'", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type 'application/json', got '%s'", r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResponse)
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "test-api-key",
		endpoint: server.URL,
	}

	issues, err := client.FetchIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchIssues failed: %v", err)
	}

	if len(issues) != 2 {
		t.Fatalf("Expected 2 issues, got %d", len(issues))
	}

	if issues[0].Identifier != "ENG-123" {
		t.Errorf("Expected identifier 'ENG-123', got '%s'", issues[0].Identifier)
	}
	if issues[0].Title != "Fix login bug" {
		t.Errorf("Expected title 'Fix login bug', got '%s'", issues[0].Title)
	}
	if issues[0].State != "In Progress" {
		t.Errorf("Expected state 'In Progress', got '%s'", issues[0].State)
	}
}

func TestFetchIssues_EmptyNodes(t *testing.T) {
	mockResponse := graphqlResponse{
		Data: &graphqlData{
			Issues: struct {
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
			}{
				Nodes: []struct {
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
					BranchName  string `json:"branchName"`
					URL         string `json:"url"`
					State       struct {
						Name string `json:"name"`
					} `json:"state"`
				}{},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResponse)
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "test-api-key",
		endpoint: server.URL,
	}

	issues, err := client.FetchIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchIssues failed: %v", err)
	}

	if len(issues) != 0 {
		t.Fatalf("Expected 0 issues, got %d", len(issues))
	}
}

func TestFetchIssues_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "test-api-key",
		endpoint: server.URL,
	}

	_, err := client.FetchIssues(context.Background())
	if err == nil {
		t.Fatal("Expected error for malformed JSON, got nil")
	}
}

func TestFetchIssues_MissingData(t *testing.T) {
	mockResponse := graphqlResponse{
		Data: nil,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResponse)
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "test-api-key",
		endpoint: server.URL,
	}

	_, err := client.FetchIssues(context.Background())
	if err == nil {
		t.Fatal("Expected error for missing data, got nil")
	}
	if err.Error() != "no data in response" {
		t.Errorf("Expected 'no data in response', got '%s'", err.Error())
	}
}

func TestFetchIssues_AuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "invalid-key",
		endpoint: server.URL,
	}

	_, err := client.FetchIssues(context.Background())
	if err == nil {
		t.Fatal("Expected error for auth failure, got nil")
	}
}

func TestFetchIssues_GraphQLErrors(t *testing.T) {
	mockResponse := graphqlResponse{
		Errors: []graphqlError{
			{Message: "Something went wrong"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResponse)
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "test-api-key",
		endpoint: server.URL,
	}

	_, err := client.FetchIssues(context.Background())
	if err == nil {
		t.Fatal("Expected error for GraphQL errors, got nil")
	}
	if err.Error() != "GraphQL errors: Something went wrong" {
		t.Errorf("Expected 'GraphQL errors: Something went wrong', got '%s'", err.Error())
	}
}

func TestFetchIssues_SetsCorrectHeaders(t *testing.T) {
	headerChecked := false
	emptyResponse := makeEmptyResponse()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin_api_test123" {
			t.Errorf("Expected Authorization header 'lin_api_test123', got '%s'", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type 'application/json', got '%s'", r.Header.Get("Content-Type"))
		}
		headerChecked = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emptyResponse)
	}))
	defer server.Close()

	client := &linearClient{
		apiKey:   "lin_api_test123",
		endpoint: server.URL,
	}

	_, err := client.FetchIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchIssues failed: %v", err)
	}

	if !headerChecked {
		t.Error("Headers were not checked")
	}
}

func TestFetchIssues_UsesCorrectEndpoint(t *testing.T) {
	endpointChecked := false
	emptyResponse := makeEmptyResponse()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpointChecked = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emptyResponse)
	}))
	defer server.Close()

	client := newLinearClient("test-key")
	client.endpoint = server.URL

	_, err := client.FetchIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchIssues failed: %v", err)
	}

	if !endpointChecked {
		t.Error("Endpoint was not called")
	}
}

// makeEmptyResponse creates a valid graphqlResponse with no issues.
func makeEmptyResponse() graphqlResponse {
	return graphqlResponse{
		Data: &graphqlData{
			Issues: struct {
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
			}{
				Nodes: []struct {
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
					BranchName  string `json:"branchName"`
					URL         string `json:"url"`
					State       struct {
						Name string `json:"name"`
					} `json:"state"`
				}{},
			},
		},
	}
}
