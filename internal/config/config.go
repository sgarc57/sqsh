// Package config loads and validates the sqshd configuration file (SPEC.md §13).
//
// Validation is fail-fast and total: unknown keys, missing required keys, and
// violated derived constraints are all startup errors. The daemon maps any
// error returned from Load to exit code 78 (EX_CONFIG).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ExitConfig is the process exit status for configuration errors (EX_CONFIG).
const ExitConfig = 78

// VisibilityMargin is the minimum headroom visibility_timeout must keep over
// max_command_timeout (§8.5).
const VisibilityMargin = 30 * time.Second

// ReadOnlySystemctlVerbs are the only systemctl verbs this release permits.
// State-changing verbs are AtMostOnce and need the deferred dedupe store.
var ReadOnlySystemctlVerbs = []string{"status", "is-active", "is-enabled", "show"}

// Config is the subset of §13 this slice reads. Keys outside it are rejected
// as unknown, so a deferred key cannot be set and silently ignored.
type Config struct {
	HostID       string `yaml:"host_id"`
	Region       string `yaml:"region"`
	TopicARN     string `yaml:"topic_arn"`
	QueueURL     string `yaml:"queue_url"`
	ResultsTable string `yaml:"results_table"`

	WaitTimeSeconds       int      `yaml:"wait_time_seconds"`
	VisibilityTimeout     Duration `yaml:"visibility_timeout"`
	MaxConcurrentCommands int      `yaml:"max_concurrent_commands"`

	DefaultCommandTimeout Duration `yaml:"default_command_timeout"`
	MaxCommandTimeout     Duration `yaml:"max_command_timeout"`
	MaxClockSkew          Duration `yaml:"max_clock_skew"`
	ShutdownGrace         Duration `yaml:"shutdown_grace"`

	MaxOutputBytes  ByteSize `yaml:"max_output_bytes"`
	ResultRetention Duration `yaml:"result_retention"`

	AllowedCommands []string `yaml:"allowed_commands"`
	Commands        Commands `yaml:"commands"`

	// Execute set to false is the §17 dry-run mode: argv is logged, never spawned.
	Execute *bool `yaml:"execute"`

	LogLevel    string `yaml:"log_level"`
	MetricsAddr string `yaml:"metrics_addr"`
}

// Commands holds per-command allowlists.
type Commands struct {
	Systemctl SystemctlConfig `yaml:"systemctl"`
}

// SystemctlConfig holds the two independent systemctl allowlists (§11.3).
type SystemctlConfig struct {
	Verbs []string `yaml:"verbs"`
	Units []string `yaml:"units"`
}

// ShouldExecute reports whether commands are spawned (false in dry-run mode).
func (c *Config) ShouldExecute() bool { return c.Execute == nil || *c.Execute }

// Load reads, decodes, defaults, and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data)
}

// Parse decodes, defaults, and validates a config document.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults fills zero-valued optional keys with their §13 defaults.
func (c *Config) ApplyDefaults() {
	if c.WaitTimeSeconds == 0 {
		c.WaitTimeSeconds = 20
	}
	if c.VisibilityTimeout == 0 {
		c.VisibilityTimeout = Duration(90 * time.Second)
	}
	if c.MaxConcurrentCommands == 0 {
		c.MaxConcurrentCommands = 4
	}
	if c.DefaultCommandTimeout == 0 {
		c.DefaultCommandTimeout = Duration(30 * time.Second)
	}
	if c.MaxCommandTimeout == 0 {
		c.MaxCommandTimeout = Duration(60 * time.Second)
	}
	if c.MaxClockSkew == 0 {
		c.MaxClockSkew = Duration(300 * time.Second)
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = Duration(45 * time.Second)
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = 32 * 1024
	}
	if c.ResultRetention == 0 {
		c.ResultRetention = Duration(14 * 24 * time.Hour)
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

// Validate checks required keys and every derived constraint, returning all
// violations joined so an operator fixes the file in one pass.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	for key, val := range map[string]string{
		"host_id":       c.HostID,
		"region":        c.Region,
		"queue_url":     c.QueueURL,
		"results_table": c.ResultsTable,
	} {
		if strings.TrimSpace(val) == "" {
			fail("%s is required", key)
		}
	}

	if c.WaitTimeSeconds < 1 || c.WaitTimeSeconds > 20 {
		fail("wait_time_seconds must be between 1 and 20, got %d", c.WaitTimeSeconds)
	}
	if c.MaxConcurrentCommands < 1 {
		fail("max_concurrent_commands must be at least 1, got %d", c.MaxConcurrentCommands)
	}
	if c.MaxCommandTimeout.D() < time.Second {
		fail("max_command_timeout must be at least 1s, got %s", c.MaxCommandTimeout)
	}
	if c.DefaultCommandTimeout.D() < time.Second || c.DefaultCommandTimeout > c.MaxCommandTimeout {
		fail("default_command_timeout must be between 1s and max_command_timeout (%s), got %s",
			c.MaxCommandTimeout, c.DefaultCommandTimeout)
	}
	if min := c.MaxCommandTimeout.D() + VisibilityMargin; c.VisibilityTimeout.D() < min {
		fail("visibility_timeout (%s) must be >= max_command_timeout + %s (%s)",
			c.VisibilityTimeout, VisibilityMargin, min)
	}
	if c.VisibilityTimeout.D() > 12*time.Hour {
		fail("visibility_timeout must not exceed the SQS maximum of 12h, got %s", c.VisibilityTimeout)
	}
	if c.MaxClockSkew < 0 {
		fail("max_clock_skew must not be negative")
	}
	if c.ShutdownGrace < 0 {
		fail("shutdown_grace must not be negative")
	}
	if c.MaxOutputBytes < 1024 {
		fail("max_output_bytes must be at least 1KiB, got %d", c.MaxOutputBytes)
	}
	// Two capped streams must fit in one 400 KB DynamoDB item with room to spare.
	if c.MaxOutputBytes > 128*1024 {
		fail("max_output_bytes must not exceed 128KiB (DynamoDB item limit), got %d", c.MaxOutputBytes)
	}
	if c.ResultRetention.D() < time.Hour {
		fail("result_retention must be at least 1h, got %s", c.ResultRetention)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("log_level must be one of debug, info, warn, error; got %q", c.LogLevel)
	}

	seen := map[string]bool{}
	for _, name := range c.AllowedCommands {
		if seen[name] {
			fail("allowed_commands lists %q twice", name)
		}
		seen[name] = true
	}
	if seen["systemctl"] {
		sc := c.Commands.Systemctl
		if len(sc.Verbs) == 0 || len(sc.Units) == 0 {
			fail("allowed_commands includes systemctl, so commands.systemctl.verbs and .units are required")
		}
		for _, v := range sc.Verbs {
			if !slices.Contains(ReadOnlySystemctlVerbs, v) {
				fail("commands.systemctl.verbs: %q is not permitted in this release (allowed: %s)",
					v, strings.Join(ReadOnlySystemctlVerbs, ", "))
			}
		}
		for _, u := range sc.Units {
			if !validUnitName(u) {
				fail("commands.systemctl.units: %q is not a plain unit name with a unit suffix", u)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return nil
}

// validUnitName accepts only plain, non-templated unit names carrying an
// explicit suffix, so the configured set is exactly what the validator matches.
func validUnitName(u string) bool {
	if u == "" || strings.HasPrefix(u, "-") || strings.ContainsAny(u, "/@%\x00 ") || strings.Contains(u, "..") {
		return false
	}
	dot := strings.LastIndexByte(u, '.')
	return dot > 0 && dot < len(u)-1
}

// Duration is a time.Duration decoded from a Go duration string ("90s").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", n.Line, s, err)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a byte count decoded from an integer or a binary-suffixed
// string ("32KiB", "8MiB").
type ByteSize int64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	mult := int64(1)
	for suffix, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSuffix(s, suffix), m
			break
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: invalid byte size %q", n.Line, n.Value)
	}
	*b = ByteSize(v * mult)
	return nil
}
