package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrTaskInProgress is returned by StartTask when the sandbox already
// has an active task (one task at a time).
var ErrTaskInProgress = errors.New("a task is already in progress")

// Client is the sandboxd-side client for one sandbox's runtimed,
// reached over its Unix domain socket. It is the integration seam:
// sandboxd constructs a Client per sandbox from the host-side socket
// path (<workspaces>/<id>.mnt/.runtimed/sock).
type Client struct {
	socketPath string
	http       *http.Client // short-timeout: status, start, cancel
	stream     *http.Client // no timeout: the task event stream
}

// NewClient builds a Client for the runtimed listening at socketPath.
func NewClient(socketPath string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socketPath: socketPath,
		http:       &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: dial}},
		stream:     &http.Client{Transport: &http.Transport{DialContext: dial}},
	}
}

// SocketPath is the Unix socket this client dials.
func (c *Client) SocketPath() string { return c.socketPath }

// Status fetches the runtimed snapshot. A connection error (socket
// absent or refused) means the sandbox is stopped or runtimed has not
// finished booting.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	resp, err := c.do(ctx, c.http, http.MethodGet, "http://runtimed/status", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtimed /status: %s", resp.Status)
	}
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode runtimed status: %w", err)
	}
	return &s, nil
}

// StartTask asks runtimed to begin a coding task. ErrTaskInProgress is
// returned when a task is already running.
func (c *Client) StartTask(ctx context.Context, req StartTaskRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, c.http, http.MethodPost, "http://runtimed/tasks", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return ErrTaskInProgress
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("runtimed POST /tasks: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// CancelTask asks runtimed to cancel a task. It is idempotent.
func (c *Client) CancelTask(ctx context.Context, taskID string) error {
	resp, err := c.do(ctx, c.http, http.MethodPost,
		"http://runtimed/tasks/"+url.PathEscape(taskID)+"/cancel", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtimed cancel: %s", resp.Status)
	}
	return nil
}

// TaskEvents opens the task event stream from event index `since`.
// The returned ReadCloser is a stream of newline-delimited JSON
// Events; the caller decodes and must Close it. The stream ends after
// the terminal `done` event.
func (c *Client) TaskEvents(ctx context.Context, taskID string, since int) (io.ReadCloser, error) {
	u := fmt.Sprintf("http://runtimed/tasks/%s/events?since=%d", url.PathEscape(taskID), since)
	resp, err := c.do(ctx, c.stream, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("runtimed task events: %s", resp.Status)
	}
	return resp.Body, nil
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, u string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return hc.Do(req)
}

// DecodeEvents reads newline-delimited JSON Events from r and calls fn
// for each. It returns when the stream ends or fn returns false.
func DecodeEvents(r io.Reader, fn func(Event) bool) error {
	dec := json.NewDecoder(r)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !fn(ev) {
			return nil
		}
	}
}
