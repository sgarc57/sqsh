package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const validID = "018f3a2c-6c1e-7b3a-9f2d-4a1b6c8e0d55"

var (
	issued  = time.Date(2026, 7, 31, 10, 20, 15, 0, time.UTC)
	expires = time.Date(2026, 7, 31, 10, 25, 15, 0, time.UTC)
	lim     = Limits{MaxCommandTimeout: 60 * time.Second, MaxClockSkew: 300 * time.Second}
)

// body returns a valid v1 request as a generic map so tests can delete or
// mutate individual fields.
func body() map[string]any {
	return map[string]any{
		"version":         1,
		"request_id":      validID,
		"target":          map[string]any{"host": "web01"},
		"command":         "systemctl",
		"args":            []any{"status", "nginx.service"},
		"issued_at":       "2026-07-31T10:20:15Z",
		"not_before":      nil,
		"expires_at":      "2026-07-31T10:25:15Z",
		"timeout_seconds": 30,
		"issuer":          "arn:aws:sts::123456789012:assumed-role/oncall/someone",
		"signature":       nil,
		"signature_alg":   nil,
	}
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseValid(t *testing.T) {
	r, rej := Parse(encode(t, body()))
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if r.RequestID != validID || r.Target.Host != "web01" || r.Command != "systemctl" ||
		!slices.Equal(r.Args, []string{"status", "nginx.service"}) || !r.IssuedAt.Equal(issued) ||
		!r.ExpiresAt.Equal(expires) || r.NotBefore != nil || r.TimeoutSeconds != 30 {
		t.Fatalf("parsed request mismatch: %+v", r)
	}
	if rej := Validate(r, issued.Add(time.Second), lim); rej != nil {
		t.Fatalf("unexpected validation failure: %v", rej)
	}
}

func TestEmptyArgsValid(t *testing.T) {
	m := body()
	m["args"] = []any{}
	if _, rej := Parse(encode(t, m)); rej != nil {
		t.Fatalf("empty args must be valid: %v", rej)
	}
}

func TestParseRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		raw    string
		want   Reason
	}{
		{name: "not json", raw: "{nope", want: ReasonMalformed},
		{name: "json array", raw: "[]", want: ReasonMalformed},
		{name: "missing version", mutate: func(m map[string]any) { delete(m, "version") }, want: ReasonMalformed},
		{name: "version 2", mutate: func(m map[string]any) { m["version"] = 2 }, want: ReasonUnsupportedVersion},
		{name: "version 0", mutate: func(m map[string]any) { m["version"] = 0 }, want: ReasonUnsupportedVersion},
		{name: "version 2 with garbage fields", mutate: func(m map[string]any) {
			m["version"] = 2
			delete(m, "request_id")
		}, want: ReasonUnsupportedVersion},
		{name: "version as string", mutate: func(m map[string]any) { m["version"] = "1" }, want: ReasonMalformed},
		{name: "missing request_id", mutate: func(m map[string]any) { delete(m, "request_id") }, want: ReasonMalformed},
		{name: "missing target", mutate: func(m map[string]any) { delete(m, "target") }, want: ReasonMalformed},
		{name: "empty target.host", mutate: func(m map[string]any) { m["target"] = map[string]any{"host": ""} }, want: ReasonMalformed},
		{name: "missing command", mutate: func(m map[string]any) { delete(m, "command") }, want: ReasonMalformed},
		{name: "missing args", mutate: func(m map[string]any) { delete(m, "args") }, want: ReasonMalformed},
		{name: "null args", mutate: func(m map[string]any) { m["args"] = nil }, want: ReasonMalformed},
		{name: "non-string arg", mutate: func(m map[string]any) { m["args"] = []any{1} }, want: ReasonMalformed},
		{name: "missing issued_at", mutate: func(m map[string]any) { delete(m, "issued_at") }, want: ReasonMalformed},
		{name: "missing expires_at", mutate: func(m map[string]any) { delete(m, "expires_at") }, want: ReasonMalformed},
		{name: "missing timeout_seconds", mutate: func(m map[string]any) { delete(m, "timeout_seconds") }, want: ReasonMalformed},
		{name: "missing issuer", mutate: func(m map[string]any) { delete(m, "issuer") }, want: ReasonMalformed},
		{name: "request_id not a uuid", mutate: func(m map[string]any) { m["request_id"] = "not-a-uuid" }, want: ReasonMalformed},
		{name: "request_id uuidv4", mutate: func(m map[string]any) { m["request_id"] = "3f0e9b8a-1c2d-4e5f-8a9b-0c1d2e3f4a5b" }, want: ReasonMalformed},
		{name: "request_id braces", mutate: func(m map[string]any) { m["request_id"] = "{" + validID + "}" }, want: ReasonMalformed},
		{name: "issued_at with offset", mutate: func(m map[string]any) { m["issued_at"] = "2026-07-31T12:20:15+02:00" }, want: ReasonMalformed},
		{name: "expires_at not rfc3339", mutate: func(m map[string]any) { m["expires_at"] = "31/07/2026 10:25Z" }, want: ReasonMalformed},
		{name: "not_before not rfc3339", mutate: func(m map[string]any) { m["not_before"] = "tomorrow" }, want: ReasonMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			if tc.mutate != nil {
				m := body()
				tc.mutate(m)
				raw = encode(t, m)
			}
			_, rej := Parse(raw)
			if rej == nil {
				t.Fatalf("expected rejection %s", tc.want)
			}
			if rej.Reason != tc.want {
				t.Fatalf("got %s (%s), want %s", rej.Reason, rej.Detail, tc.want)
			}
		})
	}
}

func TestPartialRequestRecoveredOnRejection(t *testing.T) {
	m := body()
	delete(m, "expires_at")
	r, rej := Parse(encode(t, m))
	if rej == nil || r.RequestID != validID || r.Target.Host != "web01" || r.Issuer == "" {
		t.Fatalf("expected identifying fields on a rejected parse, got %+v %v", r, rej)
	}
}

func TestUnknownFieldsIgnoredAtEveryLevel(t *testing.T) {
	m := body()
	m["future_top_level"] = map[string]any{"deep": []any{1, 2}}
	m["target"] = map[string]any{"host": "web01", "zone": "eu-central-1a"}
	m["signature"] = "AAAA-not-a-real-signature"
	m["signature_alg"] = "ed25519"
	r, rej := Parse(encode(t, m))
	if rej != nil {
		t.Fatalf("unknown fields must be ignored: %v", rej)
	}
	if rej := Validate(r, issued, lim); rej != nil {
		t.Fatalf("signature must not be verified: %v", rej)
	}
	if r.Target.Host != "web01" {
		t.Fatalf("known field lost next to unknown one: %+v", r.Target)
	}
}

func TestValidateRejections(t *testing.T) {
	future := issued.Add(time.Minute)
	cases := []struct {
		name   string
		mutate func(r *Request)
		now    time.Time
		want   Reason
	}{
		{"timeout zero", func(r *Request) { r.TimeoutSeconds = 0 }, issued, ReasonInvalidTimeout},
		{"timeout negative", func(r *Request) { r.TimeoutSeconds = -5 }, issued, ReasonInvalidTimeout},
		{"timeout above max", func(r *Request) { r.TimeoutSeconds = 61 }, issued, ReasonInvalidTimeout},
		{"issued_at beyond skew", func(r *Request) { r.IssuedAt = issued.Add(301 * time.Second) }, issued, ReasonMalformed},
		{"not_before in future", func(r *Request) { r.NotBefore = &future }, issued, ReasonNotYetValid},
		{"expired", func(r *Request) {}, expires.Add(time.Second), ReasonExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rej := Parse(encode(t, body()))
			if rej != nil {
				t.Fatal(rej)
			}
			tc.mutate(&r)
			got := Validate(r, tc.now, lim)
			if got == nil || got.Reason != tc.want {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}

func TestValidateBoundaries(t *testing.T) {
	r, _ := Parse(encode(t, body()))
	r.TimeoutSeconds = 60
	if rej := Validate(r, issued, lim); rej != nil {
		t.Fatalf("timeout equal to max must be accepted: %v", rej)
	}
	r.TimeoutSeconds = 1
	if rej := Validate(r, expires, lim); rej != nil {
		t.Fatalf("now == expires_at is not yet expired: %v", rej)
	}
	r.IssuedAt = issued.Add(300 * time.Second)
	if rej := Validate(r, issued, lim); rej != nil {
		t.Fatalf("issued_at exactly at skew limit must be accepted: %v", rej)
	}
}

func TestExpiredRejectionRecordsExpiredStatus(t *testing.T) {
	if (&Rejection{Reason: ReasonExpired}).Status() != StatusExpired {
		t.Fatal("expired rejection must record status expired")
	}
	if (&Rejection{Reason: ReasonMalformed}).Status() != StatusRejected {
		t.Fatal("other rejections record status rejected")
	}
}

func TestRoundTrip(t *testing.T) {
	nb := issued.Add(30 * time.Second)
	sig := "c2ln"
	in := Request{
		Version: Version, RequestID: validID, Target: Target{Host: "web01"},
		Command: "systemctl", Args: []string{"status", "sshd.service"},
		IssuedAt: issued, NotBefore: &nb, ExpiresAt: expires, TimeoutSeconds: 30,
		Issuer: "arn:aws:iam::123456789012:user/x", Signature: &sig,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"issued_at":"2026-07-31T10:20:15Z"`) {
		t.Fatalf("timestamps must encode as second-precision Z: %s", raw)
	}
	out, rej := Parse(raw)
	if rej != nil {
		t.Fatalf("round-trip rejected: %v", rej)
	}
	if SigningString(in) != SigningString(out) {
		t.Fatalf("round-trip changed the signing string:\n%q\n%q", SigningString(in), SigningString(out))
	}
	if out.Issuer != in.Issuer || *out.Signature != sig {
		t.Fatalf("round-trip lost fields: %+v", out)
	}
}

func TestMarshalNilArgsAsEmptyArray(t *testing.T) {
	raw, _ := json.Marshal(Request{Version: 1})
	if !strings.Contains(string(raw), `"args":[]`) || !strings.Contains(string(raw), `"not_before":null`) {
		t.Fatalf("args must be [] and not_before null: %s", raw)
	}
}

func TestNewRequestIDIsUUIDv7AndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := NewRequestID()
		if err != nil || !IsUUIDv7(id) || seen[id] {
			t.Fatalf("bad or repeated id %q: %v", id, err)
		}
		seen[id] = true
	}
}

func TestStatusAndReasonSets(t *testing.T) {
	if StatusRunning.Terminal() {
		t.Fatal("running is not terminal")
	}
	for _, s := range []Status{StatusSucceeded, StatusFailed, StatusTimedOut, StatusRejected, StatusExpired} {
		if !s.Terminal() {
			t.Fatalf("%s must be terminal", s)
		}
	}
	if Status("accepted").Valid() {
		t.Fatal("accepted does not exist in this release")
	}
	if len(Reasons) != 11 || slices.Contains(Reasons, Reason("bad_signature")) {
		t.Fatalf("reason set must be the 11 values of this release: %v", Reasons)
	}
}

// --- canonical signing string -------------------------------------------

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSigningStringGolden(t *testing.T) {
	r := Request{
		RequestID: validID, Target: Target{Host: "web01"}, Command: "systemctl",
		Args:     []string{"status", "nginx.service", "café \n:x"},
		IssuedAt: issued, ExpiresAt: expires, TimeoutSeconds: 30,
	}
	if got, want := SigningString(r), readGolden(t, "signing_v1.golden"); got != want {
		t.Fatalf("signing string drifted from golden file:\n got %q\nwant %q", got, want)
	}

	nb := time.Date(2026, 7, 31, 10, 21, 0, 0, time.UTC)
	r.Args, r.NotBefore, r.TimeoutSeconds = []string{}, &nb, 1
	if got, want := SigningString(r), readGolden(t, "signing_v1_notbefore_noargs.golden"); got != want {
		t.Fatalf("signing string drifted from golden file:\n got %q\nwant %q", got, want)
	}
}

func TestSigningStringTruncatesSubSecond(t *testing.T) {
	r := Request{RequestID: validID, IssuedAt: issued.Add(999 * time.Millisecond), ExpiresAt: expires}
	if !strings.Contains(SigningString(r), "\n2026-07-31T10:20:15Z\n") {
		t.Fatalf("timestamps must be second precision: %q", SigningString(r))
	}
}

func TestSigningStringArgumentBoundaries(t *testing.T) {
	base := Request{RequestID: validID, Target: Target{Host: "h"}, Command: "c", IssuedAt: issued, ExpiresAt: expires, TimeoutSeconds: 1}
	pairs := [][2][]string{
		{{"a", "bc"}, {"ab", "c"}},
		{{"a\n1:b"}, {"a", "b"}},
		{{""}, {}},
		{{"", ""}, {""}},
	}
	for _, p := range pairs {
		x, y := base, base
		x.Args, y.Args = p[0], p[1]
		if SigningString(x) == SigningString(y) {
			t.Fatalf("args %q and %q produced the same signing string", p[0], p[1])
		}
	}
}
