package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

// appServerLaunch names the `codex app-server --stdio` process to start. Args
// are the codex arguments after the binary; the login shell wraps them.
type appServerLaunch struct {
	binary         string
	loginShell     string
	commandFactory func(context.Context, string, ...string) *exec.Cmd
	args           []string
	env            []string
	dir            string
}

// appServerConn is one initialized JSON-RPC session over app-server stdio.
type appServerConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	encoder *json.Encoder
	decoder *json.Decoder
	nextID  int
	closed  bool
}

// openAppServer starts app-server and completes the initialize handshake. The
// caller must close the returned connection.
func openAppServer(ctx context.Context, launch appServerLaunch) (*appServerConn, error) {
	argv := append([]string{launch.binary, "app-server", "--stdio"}, launch.args...)
	argv = loginshell.Wrap(launch.loginShell, loginshell.Flags(launch.loginShell), argv)
	commandFactory := launch.commandFactory
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	cmd := commandFactory(ctx, argv[0], argv[1:]...)
	cmd.Env = launch.env
	cmd.Dir = launch.dir
	cmd.Stderr = io.Discard
	cmd.WaitDelay = codexOperationRegistryWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start app-server: %w", err)
	}
	conn := &appServerConn{cmd: cmd, stdin: stdin, encoder: json.NewEncoder(stdin), decoder: json.NewDecoder(stdout), nextID: 1}
	if _, err := conn.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "bossd-plugin-codex", "version": pluginVersion},
		"capabilities": map[string]any{},
	}); err != nil {
		conn.close()
		return nil, fmt.Errorf("initialize app-server: %w", err)
	}
	if err := conn.encoder.Encode(appServerRequest{Method: "initialized"}); err != nil {
		conn.close()
		return nil, fmt.Errorf("notify app-server initialized: %w", err)
	}
	return conn, nil
}

// call sends one request and returns its result, skipping notifications.
func (c *appServerConn) call(method string, params any) (json.RawMessage, error) {
	id := c.nextID
	c.nextID++
	if err := c.encoder.Encode(appServerRequest{ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	return readAppServerResponse(c.decoder, id)
}

// close ends the session by closing stdin and reaping the process.
func (c *appServerConn) close() {
	if c.closed {
		return
	}
	c.closed = true
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
}

type appServerRequest struct {
	ID     int    `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type appServerResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readAppServerResponse(decoder *json.Decoder, expectedID int) (json.RawMessage, error) {
	for {
		var response appServerResponse
		if err := decoder.Decode(&response); err != nil {
			return nil, fmt.Errorf("read response id %d: %w", expectedID, err)
		}
		if response.ID == nil {
			continue
		}
		if *response.ID != expectedID {
			return nil, fmt.Errorf("response id = %d, want %d", *response.ID, expectedID)
		}
		if response.Error != nil {
			return nil, fmt.Errorf(
				"response id %d error %d: %s",
				expectedID,
				response.Error.Code,
				response.Error.Message,
			)
		}
		if len(response.Result) == 0 {
			return nil, fmt.Errorf("response id %d missing result", expectedID)
		}
		return response.Result, nil
	}
}
