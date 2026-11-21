package github

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type apiRequestShape struct {
	path, method      string
	fields            map[string]any
	headers           http.Header
	include, paginate bool
}

func parseAPIArgs(args []string) (apiRequestShape, bool) {
	s := apiRequestShape{fields: map[string]any{}, headers: make(http.Header)}
	if len(args) < 2 || args[0] != "api" {
		return s, false
	}

	for i := 1; i < len(args); i++ {
		next, ok := s.consumeArg(args, i)
		if !ok {
			return s, false
		}
		i = next
	}
	if s.path == "" || strings.ContainsAny(s.path, "{}") || strings.Contains(s.path, "://") || strings.HasPrefix(s.path, "//") {
		return s, false
	}
	if s.path == "graphql" && s.paginate {
		return s, false
	}
	if s.method == "" {
		s.method = http.MethodGet
		if len(s.fields) > 0 || s.path == "graphql" {
			s.method = http.MethodPost
		}
	}
	return s, true
}

func (s apiRequestShape) request(ctx context.Context, base string) (*http.Request, error) {
	rawURL := strings.TrimRight(base, "/") + "/" + strings.TrimPrefix(s.path, "/")
	var body []byte
	if len(s.fields) > 0 {
		if s.method == http.MethodGet {
			u, err := url.Parse(rawURL)
			if err != nil {
				return nil, err
			}
			query := u.Query()
			for key, value := range s.fields {
				v := ""
				if value != nil {
					v = fmt.Sprint(value)
				}
				query.Add(key, v)
			}
			u.RawQuery = query.Encode()
			rawURL = u.String()
		} else {
			var err error
			body, err = apiJSONBody(s.fields, s.path == "graphql")
			if err != nil {
				return nil, err
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, s.method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.merge-info-preview+json, application/vnd.github.nebula-preview")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bossd")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	return req, nil
}

func (s apiRequestShape) applyHeaders(req *http.Request) {
	for key, values := range s.headers {
		req.Header[key] = append([]string(nil), values...)
	}
}

func nextAPIPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ">")
		if ok && strings.HasPrefix(target, "<") && strings.Contains(params, `rel="next"`) {
			return strings.TrimPrefix(target, "<")
		}
	}
	return ""
}

func (s apiRequestShape) addField(flag, value string) bool {
	key, val, ok := strings.Cut(value, "=")
	if !ok || key == "" || strings.ContainsAny(key, "[]") {
		return false
	}
	if _, exists := s.fields[key]; exists {
		return false
	}
	var field any = val
	if flag == "-F" || flag == "--field" {
		if strings.HasPrefix(val, "@") || strings.Contains(val, "{owner}") || strings.Contains(val, "{repo}") || strings.Contains(val, "{branch}") {
			return false
		}
		field = typedAPIField(val)
	}
	s.fields[key] = field
	return true
}

func typedAPIField(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	default:
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
		return value
	}
}

func (s *apiRequestShape) consumeArg(args []string, index int) (int, bool) {
	flag, value, hasValue := strings.Cut(args[index], "=")
	switch flag {
	case "-i", "--include":
		s.include = true
		return index, !hasValue
	case "--paginate":
		s.paginate = true
		return index, !hasValue
	case "-X", "--method", "-f", "--raw-field", "-F", "--field", "-H", "--header", "--hostname":
		if !hasValue {
			index++
			if index >= len(args) {
				return index, false
			}
			value = args[index]
		}
		return index, s.setValueFlag(flag, value)
	default:
		if strings.HasPrefix(args[index], "-") || s.path != "" {
			return index, false
		}
		s.path = args[index]
		return index, true
	}
}

func (s *apiRequestShape) setValueFlag(flag, value string) bool {
	switch flag {
	case "--hostname":
		return value == "github.com"
	case "-X", "--method":
		s.method = strings.ToUpper(value)
		return true
	case "-H", "--header":
		key, val, ok := strings.Cut(value, ":")
		if !ok {
			return false
		}
		s.headers.Set(strings.TrimSpace(key), strings.TrimSpace(val))
		return true
	default:
		return s.addField(flag, value)
	}
}
