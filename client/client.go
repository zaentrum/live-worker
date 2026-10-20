// Package client calls a live worker: the supervisor a stream origin talks
// to, or (inside the supervisor) one of its worker processes, which answer
// the same API over a Unix socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/live-worker/api"
)

// MaxBody bounds a response body read into memory (a 20-s segment of a
// high-bitrate rung, with room to spare).
const MaxBody = 256 << 20

// Client is one live worker's API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New is a client of the worker at base (http://host:port), sending token as
// a bearer token when it is not empty. hc nil: a client with keep-alive
// connections and no overall timeout (each call's context bounds it).
func New(base, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		}}
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: hc}
}

// NewUnix is a client of a worker process listening on the Unix socket path.
func NewUnix(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	return &Client{base: "http://slot", http: &http.Client{Transport: tr}}
}

// CloseIdle closes the client's idle connections.
func (c *Client) CloseIdle() { c.http.CloseIdleConnections() }

// Error is a failure the worker answered.
type Error struct {
	Status     int
	Body       api.ErrorBody
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("live worker: %d %s: %s", e.Status, e.Body.Class, e.Body.Error)
}

// Class is the failure's class (api.Class*).
func (e *Error) Class() string { return e.Body.Class }

// ClassOf is err's class: an *Error's, "" for anything else (a transport
// error: the worker could not be reached or went away).
func ClassOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Class()
	}
	return ""
}

// Reply is a raw answer.
type Reply struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do posts body (JSON) to path and reads the answer, whatever its status.
// An error is a transport error.
func (c *Client) Do(ctx context.Context, path string, body []byte) (*Reply, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("live worker: response over %d bytes", MaxBody)
	}
	return &Reply{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

// AsError is the reply's failure, nil for a 200.
func (r *Reply) AsError() error {
	if r.Status == http.StatusOK {
		return nil
	}
	e := &Error{Status: r.Status}
	if json.Unmarshal(r.Body, &e.Body) != nil || e.Body.Class == "" {
		e.Body = api.ErrorBody{Error: strings.TrimSpace(string(r.Body)), Class: classOfStatus(r.Status)}
	}
	if e.Body.RetryAfterMs > 0 {
		e.RetryAfter = time.Duration(e.Body.RetryAfterMs) * time.Millisecond
	} else if s, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

func classOfStatus(status int) string {
	switch status {
	case http.StatusUnprocessableEntity:
		return api.ClassSource
	case http.StatusServiceUnavailable:
		return api.ClassBusy
	case http.StatusGatewayTimeout:
		return api.ClassDeadline
	case http.StatusBadRequest:
		return api.ClassBad
	case http.StatusUnauthorized:
		return api.ClassUnauthorized
	}
	return api.ClassInternal
}

// Output is a segment or an init: the rung's init, the fragment (nil for an
// init) and what the worker said about them.
type Output struct {
	Init     []byte
	Fragment []byte
	Result   api.Result
	Codec    string
	Slot     string
	Attempts int
}

func (c *Client) output(ctx context.Context, path string, req any, wantFragment bool) (*Output, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := c.Do(ctx, path, body)
	if err != nil {
		return nil, err
	}
	if err := r.AsError(); err != nil {
		return nil, err
	}
	o := &Output{Codec: r.Header.Get(api.HeaderCodec), Slot: r.Header.Get(api.HeaderSlot)}
	o.Attempts, _ = strconv.Atoi(r.Header.Get(api.HeaderAttempts))
	if h := r.Header.Get(api.HeaderResult); h != "" {
		o.Result, _ = api.DecodeResult(h)
	}
	n := len(r.Body)
	if wantFragment {
		n, err = strconv.Atoi(r.Header.Get(api.HeaderInitLength))
		if err != nil || n <= 0 || n >= len(r.Body) {
			return nil, fmt.Errorf("live worker: %s %q does not split a body of %d bytes", api.HeaderInitLength, r.Header.Get(api.HeaderInitLength), len(r.Body))
		}
		o.Fragment = r.Body[n:]
	}
	o.Init = r.Body[:n]
	return o, nil
}

// Segment makes one segment (POST /v1/segment).
func (c *Client) Segment(ctx context.Context, req api.Request) (*Output, error) {
	return c.output(ctx, "/v1/segment", req, true)
}

// Init makes a rung's initialization segment (POST /v1/init).
func (c *Client) Init(ctx context.Context, req api.Request) (*Output, error) {
	return c.output(ctx, "/v1/init", req, false)
}

// Subtitle reads a text subtitle stream as WebVTT (POST /v1/subtitle).
func (c *Client) Subtitle(ctx context.Context, req api.SubtitleRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := c.Do(ctx, "/v1/subtitle", body)
	if err != nil {
		return nil, err
	}
	if err := r.AsError(); err != nil {
		return nil, err
	}
	return r.Body, nil
}

// GetJSON gets a JSON endpoint (/healthz, /v1/status, /v1/jobs) into v and
// returns the status.
func (c *Client) GetJSON(ctx context.Context, path string, v any) (int, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	if c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if v == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}
