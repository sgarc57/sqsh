package commands

import (
	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
)

// NewRegistry registers every built-in command against cfg's allowlist and
// checks that the allowlist names only implemented commands.
func NewRegistry(cfg *config.Config, exec *dispatcher.Executor) (*dispatcher.Registry, error) {
	r := dispatcher.NewRegistry(cfg.AllowedCommands)
	r.Register(&Systemctl{Exec: exec}, true)
	r.Register(&Puppet{Exec: exec}, false)
	if err := r.Check(); err != nil {
		return nil, err
	}
	return r, nil
}
