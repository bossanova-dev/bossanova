package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const daemonStreamPath = "/bossanova.v1.OrchestratorService/DaemonStream"

type Result struct {
	Duplex     bool
	Status     string
	StatusCode int
	Proto      string
	HeaderWait time.Duration
}

type probeResponse struct {
	status     string
	statusCode int
	proto      string
	err        error
}

func probeDuplex(baseURL string, wait time.Duration, insecure bool) (Result, error) {
	bodyReader, bodyWriter := io.Pipe()
	defer func() {
		_ = bodyReader.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), wait+30*time.Second)
	defer cancel()

	targetURL := strings.TrimRight(baseURL, "/") + daemonStreamPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bodyReader)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/connect+proto")

	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // Diagnostic flag intentionally supports probing self-signed endpoints.
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
	}

	start := time.Now()
	respCh := make(chan probeResponse, 1)
	go func() {
		resp, err := client.Do(req)
		pr := probeResponse{err: err}
		if resp != nil {
			pr.status = resp.Status
			pr.statusCode = resp.StatusCode
			pr.proto = resp.Proto
			_ = resp.Body.Close()
		}
		respCh <- pr
	}()

	go func() {
		_, _ = bodyWriter.Write([]byte{0})
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case pr := <-respCh:
		headerWait := time.Since(start)
		_ = bodyWriter.Close()
		return resultFromResponse(true, headerWait, pr)
	case <-timer.C:
		headerWait := time.Since(start)
		_ = bodyWriter.Close()
		pr := <-respCh
		return resultFromResponse(false, headerWait, pr)
	}
}

func resultFromResponse(duplex bool, headerWait time.Duration, pr probeResponse) (Result, error) {
	res := Result{
		Duplex:     duplex && pr.proto == "HTTP/2.0",
		HeaderWait: headerWait,
	}
	res.Status = pr.status
	res.StatusCode = pr.statusCode
	res.Proto = pr.proto
	if pr.err != nil {
		return res, pr.err
	}
	return res, nil
}

func formatResult(res Result) string {
	outcome := "FAIL half-duplex"
	if res.Duplex {
		outcome = "PASS full-duplex"
	}

	proto := res.Proto
	if proto == "" {
		proto = "unknown"
	}
	status := res.Status
	if status == "" {
		status = "no response"
	}

	return fmt.Sprintf("%s proto=%s status=%s header_wait=%s", outcome, proto, status, res.HeaderWait.Round(time.Millisecond))
}
