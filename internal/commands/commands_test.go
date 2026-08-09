package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
	"sqsh/internal/protocol"
)

func testConfig() *config.Config {
	return &config.Config{
		AllowedCommands: []string{"systemctl"},
		Commands: config.Commands{Systemctl: config.SystemctlConfig{
			Verbs: []string{"status", "is-active", "is-enabled", "show"},
			Units: []string{"nginx.service", "sshd.service", "backup.timer"},
		}},
	}
}

// stubSystemctl writes an executable that records its argv to a log file and
// exits with the given code. It lets tests observe whether, and how, a
// process was spawned.
func stubSystemctl(t *testing.T, exit int) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	path, log = filepath.Join(dir, "systemctl"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + log + "\necho active\nexit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func TestSystemctlRejectionCorpus(t *testing.T) {
	s := &Systemctl{}
	cfg := testConfig()
	cases := []struct {
		name string
		args []string
	}{
		{"traversal", []string{"status", "../../evil"}},
		{"encoded traversal", []string{"status", "..%2f"}},
		{"encoded traversal service", []string{"status", "..%2fnginx.service"}},
		{"embedded null", []string{"status", "nginx.service\x00"}},
		{"command chaining", []string{"status", "nginx.service;reboot"}},
		{"pipe", []string{"status", "nginx.service|reboot"}},
		{"subshell", []string{"status", "$(reboot)"}},
		{"newline", []string{"status", "nginx.service\nreboot"}},
		{"leading dash unit", []string{"status", "-nginx"}},
		{"flag as unit", []string{"status", "--all"}},
		{"flag as verb", []string{"--all", "nginx.service"}},
		{"double dash", []string{"status", "--"}},
		{"templated unit", []string{"status", "nginx@1.service"}},
		{"absolute path unit", []string{"status", "/etc/systemd/system/nginx.service"}},
		{"absolute path verb", []string{"/usr/bin/reboot", "nginx.service"}},
		{"64 KiB argument", []string{"status", strings.Repeat("a", 64*1024)}},
		{"64 KiB allowlisted-prefix argument", []string{"status", "nginx.service" + strings.Repeat("a", 64*1024)}},
		{"empty args", []string{}},
		{"one arg", []string{"status"}},
		{"3 args", []string{"status", "nginx.service", "sshd.service"}},
		{"empty unit", []string{"status", ""}},
		{"state-changing verb", []string{"restart", "nginx.service"}},
		{"stop verb", []string{"stop", "sshd.service"}},
		{"unknown verb", []string{"reboot", "nginx.service"}},
		{"verb case", []string{"STATUS", "nginx.service"}},
		{"non-allowlisted unit", []string{"status", "docker.service"}},
		{"unit prefix", []string{"status", "nginx"[:3]}},
		{"unit with suffix variant", []string{"status", "nginx.socket"}},
		{"unit trailing space", []string{"status", "nginx.service "}},
		{"unit glob", []string{"status", "nginx*"}},
		{"timer listed only as timer", []string{"status", "backup"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Validate(cfg, tc.args); err == nil {
				t.Fatalf("args %q must be rejected", tc.args)
			}
		})
	}
}

func TestSystemctlAccepted(t *testing.T) {
	s := &Systemctl{}
	cfg := testConfig()
	for _, args := range [][]string{
		{"status", "nginx.service"},
		{"status", "nginx"}, // normalized to nginx.service
		{"is-active", "sshd.service"},
		{"is-enabled", "sshd"},
		{"show", "backup.timer"},
	} {
		if err := s.Validate(cfg, args); err != nil {
			t.Errorf("%q should be accepted: %v", args, err)
		}
	}
}

func TestVerbAndUnitListsAreIndependent(t *testing.T) {
	s := &Systemctl{}
	cfg := testConfig()
	cfg.Commands.Systemctl.Verbs = []string{"is-active"}
	cfg.Commands.Systemctl.Units = []string{"sshd.service"}
	if err := s.Validate(cfg, []string{"status", "sshd.service"}); err == nil {
		t.Fatal("permitting a unit must not permit every verb")
	}
	if err := s.Validate(cfg, []string{"is-active", "nginx.service"}); err == nil {
		t.Fatal("permitting a verb must not permit every unit")
	}
}

func TestStateChangingVerbRefusedEvenIfConfigured(t *testing.T) {
	// Config validation also refuses this; the command enforces it on its own.
	cfg := testConfig()
	cfg.Commands.Systemctl.Verbs = append(cfg.Commands.Systemctl.Verbs, "restart")
	if err := (&Systemctl{}).Validate(cfg, []string{"restart", "nginx.service"}); err == nil {
		t.Fatal("restart must be refused in this release")
	}
}

func TestSystemctlExecutesWithFixedArgv(t *testing.T) {
	stub, log := stubSystemctl(t, 3)
	s := &Systemctl{Path: stub, Exec: &dispatcher.Executor{MaxOutputBytes: 1024}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := s.Execute(ctx, []string{"status", "nginx"})
	// Exit 3 for status means "unit not active": the query worked.
	if o.Status() != protocol.StatusSucceeded || *o.ExitCode != 3 || o.Stdout != "active\n" {
		t.Fatalf("unexpected outcome: %+v", o)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "status\n--no-pager\n--\nnginx.service\n" {
		t.Fatalf("argv = %q", got)
	}
}

func TestSystemctlShowExit3IsFailure(t *testing.T) {
	stub, _ := stubSystemctl(t, 3)
	s := &Systemctl{Path: stub, Exec: &dispatcher.Executor{MaxOutputBytes: 1024}}
	o := s.Execute(context.Background(), []string{"show", "nginx.service"})
	if o.Status() != protocol.StatusFailed {
		t.Fatalf("show declares only 0 as success: %+v", o)
	}
}

// Validation must cover the whole argument list before anything spawns: a
// valid verb followed by an invalid unit must never reach the process.
func TestValidationPrecedesSpawn(t *testing.T) {
	stub, log := stubSystemctl(t, 0)
	cfg := testConfig()
	r := dispatcher.NewRegistry(cfg.AllowedCommands)
	r.Register(&Systemctl{Path: stub, Exec: &dispatcher.Executor{MaxOutputBytes: 1024}}, true)
	r.Register(&Puppet{Exec: &dispatcher.Executor{}}, false)

	for _, args := range [][]string{
		{"status", "../../evil"},
		{"status", "nginx.service", "sshd.service;reboot"},
	} {
		if _, rej := r.Prepare(cfg, "systemctl", args); rej == nil || rej.Reason != protocol.ReasonArgNotAllowed {
			t.Fatalf("%q: got %v, want arg_not_allowed", args, rej)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("a process was spawned for a rejected request")
	}
}

func TestPuppetIsRegisteredButDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedCommands = []string{"systemctl", "puppet"}
	r, err := NewRegistry(cfg, &dispatcher.Executor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, rej := r.Prepare(cfg, "puppet", nil); rej == nil || rej.Reason != protocol.ReasonCommandNotAllowed {
		t.Fatalf("disabled puppet must be command_not_allowed even when allowlisted, got %v", rej)
	}
	cfg.AllowedCommands = []string{"systemctl"}
	r, _ = NewRegistry(cfg, &dispatcher.Executor{})
	if _, rej := r.Resolve("puppet"); rej == nil || rej.Reason != protocol.ReasonCommandNotAllowed {
		t.Fatalf("registered puppet is never unknown_command, got %v", rej)
	}
	if (&Puppet{}).Semantics() != dispatcher.AtMostOnce || (&Puppet{}).Validate(cfg, []string{"--noop"}) == nil {
		t.Fatal("puppet declares AtMostOnce and takes no arguments")
	}
}

func TestNewRegistryRejectsUnknownAllowlistEntry(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedCommands = []string{"systemctl", "reboot"}
	if _, err := NewRegistry(cfg, &dispatcher.Executor{}); err == nil {
		t.Fatal("reboot is not implemented in this release and must be a config error")
	}
}

func TestNormalizeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"nginx": "nginx.service", "nginx.service": "nginx.service", "backup.timer": "backup.timer",
		".service": ".service.service", "nginx.service;reboot": "nginx.service;reboot.service",
	} {
		if got := NormalizeUnit(in); got != want {
			t.Errorf("NormalizeUnit(%q) = %q, want %q", in, got, want)
		}
	}
}
