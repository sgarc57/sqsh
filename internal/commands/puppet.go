package commands

import (
	"context"
	"fmt"

	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
)

// PuppetPath is the absolute path of the Puppet agent binary.
const PuppetPath = "/opt/puppetlabs/bin/puppet"

// Puppet is registered as a disabled placeholder (§11.4): its shape is
// declared so the dispatcher supports it, but the registry never resolves it
// for execution in this release, and requests are refused command_not_allowed.
type Puppet struct {
	Exec *dispatcher.Executor
}

func (p *Puppet) Name() string { return "puppet" }

// Semantics is AtMostOnce: runs converge and overlapping runs conflict.
func (p *Puppet) Semantics() dispatcher.Semantics { return dispatcher.AtMostOnce }

// Validate implements dispatcher.Command. Puppet takes no arguments in v1;
// --environment, --noop, and friends change what converges.
func (p *Puppet) Validate(_ *config.Config, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("puppet takes no arguments, got %d", len(args))
	}
	return nil
}

// Execute implements dispatcher.Command with the fixed §11.4 argv. Exit 2
// means changes were applied, which is success.
func (p *Puppet) Execute(ctx context.Context, _ []string) dispatcher.Outcome {
	return p.Exec.Run(ctx, dispatcher.Spec{
		Path:         PuppetPath,
		Args:         []string{"agent", "-t"},
		SuccessCodes: []int{0, 2},
	})
}
