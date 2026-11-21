package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/recurser/bossd/internal/status"
)

const maxServicesBytes = 64 * 1024

var serviceName = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,32}$`)

func readServicesFile(path string) ([]status.ServiceLink, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxServicesBytes {
		return nil, errors.New("services file must be a regular file of at most 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxServicesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxServicesBytes {
		return nil, errors.New("services file exceeds 64 KiB")
	}
	return parseServices(data)
}

func parseServices(data []byte) ([]status.ServiceLink, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return nil, errors.New("services file must contain a JSON object")
	}
	if len(values) > 16 {
		return nil, errors.New("services file exceeds 16 entries")
	}
	names := slices.Sorted(maps.Keys(values))
	var links []status.ServiceLink
	var problems []error
	for _, name := range names {
		var href string
		if json.Unmarshal(values[name], &href) != nil || !validService(name, href) {
			problems = append(problems, fmt.Errorf("invalid service entry %q", name))
			continue
		}
		links = append(links, status.ServiceLink{Name: name, URL: href})
	}
	return links, errors.Join(problems...)
}

func validService(name, href string) bool {
	if !serviceName.MatchString(name) || len(href) > 2048 || strings.ContainsAny(href, "\r\n\t\\") {
		return false
	}
	u, err := url.Parse(href)
	return err == nil && u.IsAbs() && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}
