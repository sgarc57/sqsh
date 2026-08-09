// Package commands holds the built-in commands, one per file (SPEC.md §11).
package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
)

// SystemctlPath is the absolute path of the systemctl binary.
const SystemctlPath = "/usr/bin/systemctl"

// maxArgLen bounds a single argument before any set lookup. Real unit names
// are far shorter; this only keeps pathological input cheap to refuse.
const maxArgLen = 256

// unitSuffixes are the systemd unit types. A unit argument without one of
// these is normalized by appending ".service" (§11.3).
var unitSuffixes = []string{
	".service", ".socket", ".target", ".timer", ".mount", ".automount",
	".path", ".slice", ".scope", ".device", ".swap",
}

// successCodes declares, per verb, which exit codes mean the query worked.
// `status` and `is-active` exit 3 for a stopped unit and `is-enabled` exits 1
// for a disabled one: those are answers, not failures (§11.3).
var successCodes = map[string][]int{
	"status":     {0, 3},
	"is-active":  {0, 3},
	"is-enabled": {0, 1},
	"show":       {0},
}

// Systemctl is the read-only systemctl command. Verb and unit are validated
// against separate allowlists.
type Systemctl struct {
	// Path overrides SystemctlPath; tests point it at a stub.
	Path string
	Exec *dispatcher.Executor
}

func (s *Systemctl) Name() string { return "systemctl" }

// Semantics is AtLeastOnce: only read-only verbs exist in this release.
func (s *Systemctl) Semantics() dispatcher.Semantics { return dispatcher.AtLeastOnce }

// Validate implements dispatcher.Command. It accepts exactly a verb and a
// unit, each by exact membership in its own allowlist.
func (s *Systemctl) Validate(cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("systemctl takes exactly 2 arguments (verb, unit), got %d", len(args))
	}
	// Structural checks are defence in depth only; set membership below is
	// what admits a value.
	for i, a := range args {
		if len(a) == 0 || len(a) > maxArgLen {
			return fmt.Errorf("argument %d has invalid length %d", i, len(a))
		}
		if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "/@%\x00") || strings.Contains(a, "..") {
			return fmt.Errorf("argument %d contains a forbidden character or prefix", i)
		}
	}
	verb, unit := args[0], NormalizeUnit(args[1])
	if !slices.Contains(config.ReadOnlySystemctlVerbs, verb) || !slices.Contains(cfg.Commands.Systemctl.Verbs, verb) {
		return fmt.Errorf("verb %q is not allowed", verb)
	}
	if !slices.Contains(cfg.Commands.Systemctl.Units, unit) {
		return fmt.Errorf("unit %q is not allowed", unit)
	}
	return nil
}

// Execute implements dispatcher.Command. args must have passed Validate.
func (s *Systemctl) Execute(ctx context.Context, args []string) dispatcher.Outcome {
	verb, unit := args[0], NormalizeUnit(args[1])
	path := s.Path
	if path == "" {
		path = SystemctlPath
	}
	spec := dispatcher.Spec{
		Path:         path,
		Args:         []string{verb, "--no-pager", "--", unit},
		SuccessCodes: successCodes[verb],
	}
	return s.Exec.Run(ctx, spec)
}

// NormalizeUnit appends ".service" when u carries no known unit suffix.
func NormalizeUnit(u string) string {
	for _, sfx := range unitSuffixes {
		if strings.HasSuffix(u, sfx) && len(u) > len(sfx) {
			return u
		}
	}
	return u + ".service"
}
