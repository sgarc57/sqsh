package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
host_id: web01
region: eu-central-1
queue_url: https://sqs.example/123/sqsh-cmd-web01
results_table: sqsh-results
`

func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "deploy", "config.example.yaml"))
	if err != nil {
		t.Fatalf("example config must load: %v", err)
	}
	if c.HostID != "web01" || len(c.Commands.Systemctl.Units) == 0 {
		t.Fatalf("unexpected example config: %+v", c)
	}
}

func TestDefaultsApplied(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"wait_time_seconds", c.WaitTimeSeconds, 20},
		{"visibility_timeout", c.VisibilityTimeout.D(), 90 * time.Second},
		{"max_concurrent_commands", c.MaxConcurrentCommands, 4},
		{"default_command_timeout", c.DefaultCommandTimeout.D(), 30 * time.Second},
		{"max_command_timeout", c.MaxCommandTimeout.D(), 60 * time.Second},
		{"max_clock_skew", c.MaxClockSkew.D(), 300 * time.Second},
		{"shutdown_grace", c.ShutdownGrace.D(), 45 * time.Second},
		{"max_output_bytes", c.MaxOutputBytes, ByteSize(32 * 1024)},
		{"result_retention", c.ResultRetention.D(), 336 * time.Hour},
		{"allowed_commands", len(c.AllowedCommands), 0},
		{"execute", c.ShouldExecute(), true},
		{"log_level", c.LogLevel, "info"},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s: got %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"unknown key (typo'd allowlist)", minimal + "alowed_commands: [systemctl]\n", "alowed_commands"},
		{"unknown nested key", minimal + "commands:\n  systemctl:\n    verb: [status]\n", "verb"},
		{"deferred key is unknown", minimal + "require_signature: false\n", "require_signature"},
		{"missing host_id", strings.Replace(minimal, "host_id: web01", "", 1), "host_id is required"},
		{"missing region", strings.Replace(minimal, "region: eu-central-1", "", 1), "region is required"},
		{"missing queue_url", strings.Replace(minimal, "queue_url: https://sqs.example/123/sqsh-cmd-web01", "", 1), "queue_url is required"},
		{"missing results_table", strings.Replace(minimal, "results_table: sqsh-results", "", 1), "results_table is required"},
		{"visibility below derived minimum", minimal + "max_command_timeout: 60s\nvisibility_timeout: 89s\n", "visibility_timeout"},
		{"visibility below default-derived minimum", minimal + "max_command_timeout: 120s\n", "visibility_timeout"},
		{"wait_time_seconds above 20", minimal + "wait_time_seconds: 21\n", "wait_time_seconds"},
		{"default timeout above max", minimal + "default_command_timeout: 61s\n", "default_command_timeout"},
		{"bad duration", minimal + "shutdown_grace: soon\n", "invalid duration"},
		{"systemctl without allowlists", minimal + "allowed_commands: [systemctl]\n", "commands.systemctl"},
		{"state-changing verb", minimal + "allowed_commands: [systemctl]\ncommands:\n  systemctl:\n    verbs: [restart]\n    units: [nginx.service]\n", "restart"},
		{"templated unit", minimal + "allowed_commands: [systemctl]\ncommands:\n  systemctl:\n    verbs: [status]\n    units: [foo@bar.service]\n", "foo@bar.service"},
		{"unit without suffix", minimal + "allowed_commands: [systemctl]\ncommands:\n  systemctl:\n    verbs: [status]\n    units: [nginx]\n", "nginx"},
		{"duplicate allowed command", minimal + "allowed_commands: [puppet, puppet]\n", "twice"},
		{"bad log level", minimal + "log_level: chatty\n", "log_level"},
		{"output above item limit", minimal + "max_output_bytes: 1MiB\n", "max_output_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestDerivedConstraintBoundary(t *testing.T) {
	doc := minimal + "max_command_timeout: 60s\nvisibility_timeout: 90s\n"
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("visibility exactly max+30s must be accepted: %v", err)
	}
}

func TestAllErrorsReportedTogether(t *testing.T) {
	_, err := Parse([]byte("wait_time_seconds: 99\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"host_id", "region", "queue_url", "results_table", "wait_time_seconds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q: %v", want, err)
		}
	}
}

func TestByteSize(t *testing.T) {
	c, err := Parse([]byte(minimal + "max_output_bytes: 2048\n"))
	if err != nil || c.MaxOutputBytes != 2048 {
		t.Fatalf("plain integer: %v %v", c, err)
	}
	c, err = Parse([]byte(minimal + "max_output_bytes: 64KiB\n"))
	if err != nil || c.MaxOutputBytes != 64*1024 {
		t.Fatalf("KiB suffix: %v %v", c, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}
