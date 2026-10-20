package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zaentrum/live-worker/api"
)

func TestSegmentSplitsInitAndFragment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		var req api.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Segment == nil || req.Segment.Index != 4 {
			w.WriteHeader(400)
			return
		}
		w.Header().Set(api.HeaderInitLength, "4")
		w.Header().Set(api.HeaderCodec, "abc")
		w.Header().Set(api.HeaderAttempts, "2")
		w.Header().Set(api.HeaderResult, api.EncodeResult(api.Result{Frames: 144}))
		_, _ = w.Write([]byte("INITFRAGMENT"))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", nil)
	o, err := c.Segment(context.Background(), api.Request{Segment: &api.Segment{Index: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if string(o.Init) != "INIT" || string(o.Fragment) != "FRAGMENT" || o.Codec != "abc" || o.Attempts != 2 || o.Result.Frames != 144 {
		t.Fatalf("%+v", o)
	}
	if _, err := New(srv.URL, "", nil).Segment(context.Background(), api.Request{}); ClassOf(err) != api.ClassUnauthorized {
		t.Fatalf("no token: %v", err)
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/segment":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(api.ErrorBody{Class: api.ClassBusy, Error: "full", RetryAfterMs: 1500})
		case "/v1/init":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(504)
			_, _ = w.Write([]byte("gateway timeout"))
		case "/v1/subtitle":
			w.Header().Set(api.HeaderInitLength, "99") // no fragment to split off
			_, _ = w.Write([]byte("x"))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", nil)
	_, err := c.Segment(context.Background(), api.Request{})
	e, ok := err.(*Error)
	if !ok || e.Class() != api.ClassBusy || e.RetryAfter != 1500*time.Millisecond || e.Status != 503 {
		t.Fatalf("busy: %v", err)
	}
	_, err = c.Init(context.Background(), api.Request{})
	e, ok = err.(*Error)
	if !ok || e.Class() != api.ClassDeadline || e.RetryAfter != 3*time.Second || e.Body.Error != "gateway timeout" {
		t.Fatalf("a plain 504: %v", err)
	}
	if ClassOf(context.Canceled) != "" {
		t.Error("a transport error has a class")
	}
}
