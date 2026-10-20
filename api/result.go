package api

import (
	"encoding/json"
	"net/http"
)

// A segment's response body is the rung's initialization segment followed by
// the segment's fragment, both CMAF: the first HeaderInitLength bytes are the
// init (ftyp+moov), the rest the fragment (styp+moof+mdat). The two come from
// the same encoder, so the fragment always decodes with that init; a caller
// that already holds the rung's init compares HeaderCodec with it. The whole
// body is a playable fragmented MP4 as it is.
//
// POST /v1/init answers the init alone, POST /v1/subtitle a WebVTT document.
// A failure answers ErrorBody as JSON with the status of its class.
const (
	HeaderResult     = "Live-Worker-Result"      // JSON Result
	HeaderInitLength = "Live-Worker-Init-Length" // bytes of the body that are the init
	HeaderCodec      = "Live-Worker-Codec"       // the codec configuration's hash (SPS/PPS, AudioSpecificConfig)
	HeaderSlot       = "Live-Worker-Slot"        // the worker process that made it
	HeaderJob        = "Live-Worker-Job"
	HeaderAttempts   = "Live-Worker-Attempts" // how many worker processes were tried
	HeaderCoalesced  = "Live-Worker-Coalesced"
)

// Result describes what was made.
type Result struct {
	Job  string `json:"job"`
	Slot string `json:"slot,omitempty"`
	// Path is the pipeline that made it: cuda, cuda+libplacebo,
	// cuda+libplacebo-roundtrip, cuda+zscale-tonemap, cpu,
	// cpu+zscale-tonemap; aac for audio.
	Path    string `json:"path"`
	Encoder string `json:"encoder,omitempty"`
	Index   int    `json:"index"`
	// Start and End are the segment's boundary as resolved, seconds on the
	// request's timeline.
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Last  bool    `json:"last,omitempty"`
	// Width and Height are the frame encoded.
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Frames    int    `json:"frames"`
	Bytes     int    `json:"bytes"`
	Timescale uint32 `json:"timescale"`
	// FirstPTS and EndPTS are the fragment's first presentation time and
	// its end on its track's own timeline (timescale units): the next
	// segment's fragment starts at EndPTS.
	FirstPTS   int64   `json:"firstPts"`
	EndPTS     int64   `json:"endPts"`
	Dropped    int     `json:"dropped"` // frames decoded before the start and dropped
	InitLength int     `json:"initLength"`
	CodecHash  string  `json:"codecHash"`
	Kbps       float64 `json:"kbps"`
	Timing     Timing  `json:"timing"`
	Note       string  `json:"note,omitempty"`
}

// Timing is where a job's time went, in milliseconds.
type Timing struct {
	Queue   float64 `json:"queue"`   // waiting for a slot
	Open    float64 `json:"open"`    // opening the input
	Seek    float64 `json:"seek"`    // seeking to the keyframe before the start
	Setup   float64 `json:"setup"`   // decoder, filter graph and encoder
	First   float64 `json:"first"`   // to the first encoded packet
	Process float64 `json:"process"` // decode, filter, encode
	Mux     float64 `json:"mux"`     // writing the CMAF fragment
	Total   float64 `json:"total"`
}

// EncodeResult is r as the HeaderResult header.
func EncodeResult(r Result) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// DecodeResult reads the HeaderResult header.
func DecodeResult(s string) (Result, error) {
	var r Result
	err := json.Unmarshal([]byte(s), &r)
	return r, err
}

// Error classes. Each has its status (Status).
const (
	// ClassSource: the source cannot make this (unreadable, undecodable,
	// missing the stream, ending before the segment does). Retrying does not
	// help: 422.
	ClassSource = "source"
	// ClassCircuit: the source failed too often lately; its circuit is open
	// for RetryAfterMs: 422.
	ClassCircuit = "circuit"
	// ClassBusy: no encode slot before the deadline, or a prefetch job with
	// no slot to spare: 503, retry later.
	ClassBusy = "busy"
	// ClassDeadline: the job ran out of time (or stalled past it): 504.
	ClassDeadline = "deadline"
	// ClassBad: the request is malformed or names a path outside the
	// media roots: 400.
	ClassBad = "bad-request"
	// ClassUnauthorized: a token is configured and the request lacks it: 401.
	ClassUnauthorized = "unauthorized"
	// ClassInternal: the worker processes failed it every time they were
	// tried (crashed, killed, an error of their own): 502.
	ClassInternal = "internal"
)

// Scopes of a source failure: the whole source (it cannot be opened, holds
// no such stream) or this segment of it.
const (
	ScopeSource  = "source"
	ScopeSegment = "segment"
)

// ErrorBody is the JSON body of a failure.
type ErrorBody struct {
	Error string `json:"error"`
	Class string `json:"class"`
	Scope string `json:"scope,omitempty"`
	Job   string `json:"job,omitempty"`
	// RetryAfterMs is when to try again (busy, circuit), also sent as
	// Retry-After in seconds.
	RetryAfterMs int `json:"retryAfterMs,omitempty"`
}

// Status is the HTTP status of a class.
func Status(class string) int {
	switch class {
	case ClassSource, ClassCircuit:
		return http.StatusUnprocessableEntity
	case ClassBusy:
		return http.StatusServiceUnavailable
	case ClassDeadline:
		return http.StatusGatewayTimeout
	case ClassBad:
		return http.StatusBadRequest
	case ClassUnauthorized:
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// Retryable says whether a failure of class may succeed when tried again on
// another worker process.
func Retryable(class string) bool {
	return class == ClassInternal || class == ClassBusy || class == ClassDeadline
}
