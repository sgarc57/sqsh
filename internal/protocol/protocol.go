// Package protocol defines the wire contract between sqsh and sqshd: request
// and result records (SPEC.md §7.1–7.2), request validation, versioning rules
// (§7.3), and the canonical signing string (§7.4).
package protocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Version is the only request version this release understands.
const Version = 1

// Status is a result status. This release defines no `accepted` status; it
// ships with the terminal commands in a later change.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusTimedOut  Status = "timed_out"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
)

// Terminal reports whether s is a final status.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusTimedOut, StatusRejected, StatusExpired:
		return true
	}
	return false
}

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool { return s == StatusRunning || s.Terminal() }

// Reason is a rejection reason from the closed set of §7.2. `bad_signature`
// is reserved and absent until verification ships.
type Reason string

const (
	ReasonUnsupportedVersion Reason = "unsupported_version"
	ReasonMalformed          Reason = "malformed"
	ReasonInvalidTarget      Reason = "invalid_target"
	ReasonUnknownCommand     Reason = "unknown_command"
	ReasonCommandNotAllowed  Reason = "command_not_allowed"
	ReasonArgNotAllowed      Reason = "arg_not_allowed"
	ReasonInvalidTimeout     Reason = "invalid_timeout"
	ReasonNotYetValid        Reason = "not_yet_valid"
	ReasonExpired            Reason = "expired"
	ReasonDuplicate          Reason = "duplicate"
	ReasonConcurrencyLimit   Reason = "concurrency_limit"
)

// Reasons is the closed set of rejection reasons.
var Reasons = []Reason{
	ReasonUnsupportedVersion, ReasonMalformed, ReasonInvalidTarget, ReasonUnknownCommand,
	ReasonCommandNotAllowed, ReasonArgNotAllowed, ReasonInvalidTimeout, ReasonNotYetValid,
	ReasonExpired, ReasonDuplicate, ReasonConcurrencyLimit,
}

// Rejection is a validation failure carrying its recorded reason.
type Rejection struct {
	Reason Reason
	Detail string
}

func (r *Rejection) Error() string { return string(r.Reason) + ": " + r.Detail }

// Status returns the result status a rejection is recorded under: `expired`
// for an expired request, `rejected` for everything else.
func (r *Rejection) Status() Status {
	if r.Reason == ReasonExpired {
		return StatusExpired
	}
	return StatusRejected
}

func reject(reason Reason, format string, args ...any) *Rejection {
	return &Rejection{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Target names the host a request is addressed to.
type Target struct {
	Host string `json:"host"`
}

// Request is a validated command request (§7.1). Timestamps are truncated to
// second precision, matching the wire and signing-string format.
type Request struct {
	Version        int        `json:"version"`
	RequestID      string     `json:"request_id"`
	Target         Target     `json:"target"`
	Command        string     `json:"command"`
	Args           []string   `json:"args"`
	IssuedAt       time.Time  `json:"-"`
	NotBefore      *time.Time `json:"-"`
	ExpiresAt      time.Time  `json:"-"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	Issuer         string     `json:"issuer"`
	Signature      *string    `json:"signature"`
	SignatureAlg   *string    `json:"signature_alg"`
}

// Result is a result record (§7.2). Pointer fields encode as null when unset.
type Result struct {
	RequestID       string  `json:"request_id"`
	Host            string  `json:"host"`
	Command         string  `json:"command,omitempty"`
	Issuer          string  `json:"issuer,omitempty"`
	Status          Status  `json:"status"`
	ExitCode        *int    `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	OutputTruncated bool    `json:"output_truncated"`
	RejectionReason *Reason `json:"rejection_reason"`
	Error           *string `json:"error"`
	DurationMS      int64   `json:"duration_ms"`
	StartedAt       string  `json:"started_at,omitempty"`
	CompletedAt     string  `json:"completed_at,omitempty"`
	Attempt         int     `json:"attempt"`
	AgentVersion    string  `json:"agent_version"`
	TTL             int64   `json:"ttl"`
}

// FormatTime renders t as RFC 3339 UTC with a Z suffix at second precision.
func FormatTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// FormatTimeMillis renders t as RFC 3339 UTC with millisecond precision, for
// result timestamps where sub-second timing is informative.
func FormatTimeMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// NewRequestID returns a fresh UUIDv7.
func NewRequestID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// wireRequest is the decoding shape. Pointers distinguish "absent" from
// "zero" so required-field checks are exact. Unknown fields are ignored by
// encoding/json at every level, which is the §7.3 rule.
type wireRequest struct {
	Version   *int    `json:"version"`
	RequestID *string `json:"request_id"`
	Target    *struct {
		Host *string `json:"host"`
	} `json:"target"`
	Command        *string   `json:"command"`
	Args           *[]string `json:"args"`
	IssuedAt       *string   `json:"issued_at"`
	NotBefore      *string   `json:"not_before"`
	ExpiresAt      *string   `json:"expires_at"`
	TimeoutSeconds *int      `json:"timeout_seconds"`
	Issuer         *string   `json:"issuer"`
	Signature      *string   `json:"signature"`
	SignatureAlg   *string   `json:"signature_alg"`
}

// MarshalJSON encodes the request in wire form, with second-precision Z
// timestamps and an explicit null for an absent not_before.
func (r Request) MarshalJSON() ([]byte, error) {
	var nb *string
	if r.NotBefore != nil {
		s := FormatTime(*r.NotBefore)
		nb = &s
	}
	args := r.Args
	if args == nil {
		args = []string{}
	}
	return json.Marshal(struct {
		Version        int      `json:"version"`
		RequestID      string   `json:"request_id"`
		Target         Target   `json:"target"`
		Command        string   `json:"command"`
		Args           []string `json:"args"`
		IssuedAt       string   `json:"issued_at"`
		NotBefore      *string  `json:"not_before"`
		ExpiresAt      string   `json:"expires_at"`
		TimeoutSeconds int      `json:"timeout_seconds"`
		Issuer         string   `json:"issuer"`
		Signature      *string  `json:"signature"`
		SignatureAlg   *string  `json:"signature_alg"`
	}{r.Version, r.RequestID, r.Target, r.Command, args, FormatTime(r.IssuedAt), nb,
		FormatTime(r.ExpiresAt), r.TimeoutSeconds, r.Issuer, r.Signature, r.SignatureAlg})
}

// Parse decodes a request body and checks its shape: version first, then
// required fields, identifier format, and timestamp format. It does not apply
// agent-specific limits; see Validate.
//
// On rejection the returned Request carries whatever identifying fields could
// be recovered (request_id, target, command, issuer) so the rejection can be
// recorded against them.
func Parse(body []byte) (Request, *Rejection) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return Request{}, reject(ReasonMalformed, "body is not a JSON request object: %v", err)
	}
	partial := partialRequest(&w)

	// Version is checked before anything else: a future version may change
	// field meanings, so nothing else in the body can be trusted.
	if w.Version == nil {
		return partial, reject(ReasonMalformed, "version is required")
	}
	if *w.Version != Version {
		return partial, reject(ReasonUnsupportedVersion, "version %d is not supported (want %d)", *w.Version, Version)
	}

	var missing []string
	need := func(name string, present bool) {
		if !present {
			missing = append(missing, name)
		}
	}
	need("request_id", w.RequestID != nil)
	need("target.host", w.Target != nil && w.Target.Host != nil && *w.Target.Host != "")
	need("command", w.Command != nil && *w.Command != "")
	need("args", w.Args != nil)
	need("issued_at", w.IssuedAt != nil)
	need("expires_at", w.ExpiresAt != nil)
	need("timeout_seconds", w.TimeoutSeconds != nil)
	need("issuer", w.Issuer != nil && *w.Issuer != "")
	if len(missing) > 0 {
		return partial, reject(ReasonMalformed, "missing required fields: %v", missing)
	}

	if !IsUUIDv7(*w.RequestID) {
		return partial, reject(ReasonMalformed, "request_id %q is not a UUIDv7", *w.RequestID)
	}

	req := partial
	var err error
	if req.IssuedAt, err = ParseTime(*w.IssuedAt); err != nil {
		return partial, reject(ReasonMalformed, "issued_at: %v", err)
	}
	if req.ExpiresAt, err = ParseTime(*w.ExpiresAt); err != nil {
		return partial, reject(ReasonMalformed, "expires_at: %v", err)
	}
	if w.NotBefore != nil {
		nb, err := ParseTime(*w.NotBefore)
		if err != nil {
			return partial, reject(ReasonMalformed, "not_before: %v", err)
		}
		req.NotBefore = &nb
	}
	req.Version = *w.Version
	req.TimeoutSeconds = *w.TimeoutSeconds
	return req, nil
}

func partialRequest(w *wireRequest) Request {
	var r Request
	if w.RequestID != nil {
		r.RequestID = *w.RequestID
	}
	if w.Target != nil && w.Target.Host != nil {
		r.Target.Host = *w.Target.Host
	}
	if w.Command != nil {
		r.Command = *w.Command
	}
	if w.Args != nil {
		r.Args = *w.Args
	}
	if w.Issuer != nil {
		r.Issuer = *w.Issuer
	}
	r.Signature, r.SignatureAlg = w.Signature, w.SignatureAlg
	return r
}

// IsUUIDv7 reports whether s is a canonical-form RFC 9562 version 7 UUID.
func IsUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	id, err := uuid.Parse(s)
	return err == nil && id.Version() == 7 && id.Variant() == uuid.RFC4122
}

// ParseTime parses an RFC 3339 timestamp that must carry a `Z` suffix.
// Fractional seconds are accepted on input and truncated, so the stored value
// always matches the second-precision signing-string encoding.
func ParseTime(s string) (time.Time, error) {
	if len(s) == 0 || s[len(s)-1] != 'Z' {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339 UTC with a Z suffix", s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339: %v", s, err)
	}
	return t.UTC().Truncate(time.Second), nil
}

// Limits are the agent-side bounds a request is checked against.
type Limits struct {
	MaxCommandTimeout time.Duration
	MaxClockSkew      time.Duration
}

// Validate applies agent limits and the time window to a parsed request.
// Checks run in a fixed order so a request violating several rules always
// reports the same reason: timeout, clock skew, not_before, expiry.
func Validate(r Request, now time.Time, lim Limits) *Rejection {
	maxSecs := int(lim.MaxCommandTimeout / time.Second)
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > maxSecs {
		return reject(ReasonInvalidTimeout, "timeout_seconds %d is outside 1..%d", r.TimeoutSeconds, maxSecs)
	}
	if r.IssuedAt.After(now.Add(lim.MaxClockSkew)) {
		return reject(ReasonMalformed, "issued_at %s is more than %s in the future", FormatTime(r.IssuedAt), lim.MaxClockSkew)
	}
	if r.NotBefore != nil && now.Before(*r.NotBefore) {
		return reject(ReasonNotYetValid, "not_before %s has not been reached", FormatTime(*r.NotBefore))
	}
	if now.After(r.ExpiresAt) {
		return reject(ReasonExpired, "expires_at %s has passed", FormatTime(r.ExpiresAt))
	}
	return nil
}
