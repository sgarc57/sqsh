package dispatcher

import (
	"context"
	"errors"
	"testing"

	"sqsh/internal/config"
	"sqsh/internal/protocol"
)

type stubCmd struct {
	name     string
	sem      Semantics
	validate func(args []string) error
	executed int
}

func (s *stubCmd) Name() string         { return s.name }
func (s *stubCmd) Semantics() Semantics { return s.sem }
func (s *stubCmd) Validate(_ *config.Config, args []string) error {
	if s.validate != nil {
		return s.validate(args)
	}
	return nil
}
func (s *stubCmd) Execute(context.Context, []string) Outcome { s.executed++; return Outcome{} }

func TestResolve(t *testing.T) {
	r := NewRegistry([]string{"echo", "placeholder"})
	r.Register(&stubCmd{name: "echo"}, true)
	r.Register(&stubCmd{name: "notallowed"}, true)
	r.Register(&stubCmd{name: "placeholder", sem: AtMostOnce}, false)

	cases := []struct {
		name string
		want protocol.Reason
	}{
		{"echo", ""},
		{"rm", protocol.ReasonUnknownCommand},
		{"notallowed", protocol.ReasonCommandNotAllowed},
		{"placeholder", protocol.ReasonCommandNotAllowed},
	}
	for _, tc := range cases {
		_, rej := r.Resolve(tc.name)
		got := protocol.Reason("")
		if rej != nil {
			got = rej.Reason
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckRejectsUnimplementedAllowlistEntry(t *testing.T) {
	r := NewRegistry([]string{"echo", "reboot"})
	r.Register(&stubCmd{name: "echo"}, true)
	if err := r.Check(); err == nil {
		t.Fatal("an allowlisted but unimplemented command must be a config error")
	}
}

func TestDisabledReported(t *testing.T) {
	r := NewRegistry([]string{"placeholder"})
	r.Register(&stubCmd{name: "placeholder"}, false)
	if d := r.Disabled(); len(d) != 1 || d[0] != "placeholder" {
		t.Fatalf("got %v", d)
	}
}

func TestPrepareMapsValidationToArgNotAllowed(t *testing.T) {
	r := NewRegistry([]string{"echo"})
	cmd := &stubCmd{name: "echo", validate: func([]string) error { return errors.New("nope") }}
	r.Register(cmd, true)
	_, rej := r.Prepare(&config.Config{}, "echo", []string{"x"})
	if rej == nil || rej.Reason != protocol.ReasonArgNotAllowed {
		t.Fatalf("got %v", rej)
	}
	if cmd.executed != 0 {
		t.Fatal("Prepare must never execute")
	}
}
