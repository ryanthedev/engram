//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"
)

// MCPConn is a JSON-RPC 2.0 client over a spawned engram-mcp subprocess's
// stdio — the exact transport a real agent host drives.
type MCPConn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

// Close terminates the MCP subprocess.
func (c *MCPConn) Close() {
	_ = c.in.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// rpcResult is a decoded JSON-RPC response.
type rpcResult struct {
	Result map[string]any
	Error  map[string]any
}

// call sends a request and reads the matching response line.
func (c *MCPConn) call(method string, params any) (rpcResult, error) {
	c.next++
	req := map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method}
	if params != nil {
		req["params"] = params
	}
	line, _ := json.Marshal(req)
	if _, err := c.in.Write(append(line, '\n')); err != nil {
		return rpcResult{}, err
	}
	respLine, err := c.out.ReadBytes('\n')
	if err != nil {
		return rpcResult{}, err
	}
	var raw struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(respLine, &raw); err != nil {
		return rpcResult{}, fmt.Errorf("decoding response: %w (%s)", err, respLine)
	}
	return rpcResult{Result: raw.Result, Error: raw.Error}, nil
}

// Initialize performs the MCP handshake.
func (c *MCPConn) Initialize() (rpcResult, error) {
	r, err := c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	if err != nil {
		return r, err
	}
	// Send the initialized notification (no response).
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_, _ = c.in.Write(append(line, '\n'))
	return r, nil
}

// ListTools returns the advertised tool names.
func (c *MCPConn) ListTools() ([]string, error) {
	r, err := c.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	tools, _ := r.Result["tools"].([]any)
	var names []string
	for _, tl := range tools {
		if tm, ok := tl.(map[string]any); ok {
			names = append(names, tm["name"].(string))
		}
	}
	return names, nil
}

// CallTool invokes a tool and returns its structuredContent (or an error if
// the tool reported isError).
func (c *MCPConn) CallTool(name string, args map[string]any) (map[string]any, error) {
	r, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("tools/call protocol error: %v", r.Error)
	}
	if isErr, _ := r.Result["isError"].(bool); isErr {
		return r.Result, fmt.Errorf("tool %s reported an error: %v", name, r.Result["content"])
	}
	sc, _ := r.Result["structuredContent"].(map[string]any)
	return sc, nil
}

// waitHTTP polls url until it returns 2xx or the deadline passes.
func waitHTTP(ctx context.Context, url string) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}
