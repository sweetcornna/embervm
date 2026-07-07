package guestapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is the host-side guestd client. The API server proxy and the e2e
// suites use it; Phase 4 injects an *http.Client whose dialer enters the
// sandbox's netns.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client for the guestd at baseURL (e.g.
// "http://172.16.0.2:7777"). hc may be nil; timeouts come from the per-call
// context.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// do issues the request and returns the response, translating any non-2xx
// into an error carrying the server's ErrorResponse detail.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e ErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("guestd: %s %s: %d: %s", method, path, resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("guestd: %s %s: %d", method, path, resp.StatusCode)
	}
	return resp, nil
}

func decodeJSON[T any](resp *http.Response) (*T, error) {
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("guestd: decode response: %w", err)
	}
	return &v, nil
}

// Health fetches /healthz. Seq is the per-process monotone counter used for
// restore-continuity assertions.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil, nil, "")
	if err != nil {
		return nil, err
	}
	return decodeJSON[HealthResponse](resp)
}

// Exec runs a command in the guest and returns its buffered result. A
// command that starts and fails is a successful Exec (check ExitCode); a
// command that cannot start is an error.
func (c *Client) Exec(ctx context.Context, req *ExecRequest) (*ExecResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/exec", nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, err
	}
	return decodeJSON[ExecResponse](resp)
}

// ReadFile fetches an absolute path from the guest.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "/files", url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// WriteFile writes data to an absolute path in the guest, creating parent
// directories as needed.
func (c *Client) WriteFile(ctx context.Context, path string, mode fs.FileMode, data []byte) error {
	q := url.Values{
		"path": {path},
		"mode": {"0" + strconv.FormatUint(uint64(mode.Perm()), 8)},
	}
	resp, err := c.do(ctx, http.MethodPut, "/files", q, bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
