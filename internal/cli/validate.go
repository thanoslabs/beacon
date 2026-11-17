package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thanoslabs/beacon/internal/config"
)

// configSetting registers the flag that names the configuration.
func configSetting(fs interface {
	StringVar(p *string, name, value, usage string)
}, e env, p *string) {
	def := e.getenv("BEACON_CONFIG")
	fs.StringVar(p, "config", def, "path of the configuration: a YAML file, or a directory read recursively (env BEACON_CONFIG)")
}

func runValidate(_ context.Context, e env, args []string) error {
	var (
		path         string
		checkSecrets bool
	)
	fs := newFlagSet(e, "validate", "Check the configuration without applying it")
	configSetting(fs, e, &path)
	fs.BoolVar(&checkSecrets, "check-secrets", false,
		"also read every secret from the environment and from files, and fail when one is missing; leave it off where the secrets are absent, as in CI")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if path == "" {
		return errors.New("the configuration path is required: set --config or BEACON_CONFIG")
	}

	var opts config.LoadOptions
	if checkSecrets {
		opts.Getenv = e.getenv
	}
	cfg, err := config.Read(path, opts)
	if err != nil {
		var errs config.Errors
		if !errors.As(err, &errs) {
			return err
		}
		// One problem per line, in the file:line form that editors and CI
		// annotations understand.
		for _, problem := range errs {
			fmt.Fprintln(e.stderr, problem.Error())
		}
		return fmt.Errorf("the configuration at %s is not valid: %s", path, plural(len(errs), "problem"))
	}
	fmt.Fprintf(e.stdout, "the configuration at %s is valid: %s\n", path, summary(cfg))
	return nil
}

// summary counts the objects of a configuration.
func summary(cfg config.Config) string {
	rules := 0
	if cfg.Routing != nil {
		rules = len(cfg.Routing.Rules)
	}
	return strings.Join([]string{
		plural(len(cfg.Sources), "source"),
		plural(len(cfg.Integrations), "integration"),
		plural(len(cfg.Teams), "team"),
		plural(len(cfg.Schedules), "schedule"),
		plural(len(cfg.EscalationPolicies), "escalation policy"),
		plural(rules, "routing rule"),
	}, ", ")
}

// plural writes a count with its noun.
func plural(n int, noun string) string {
	switch {
	case n == 1:
		return "1 " + noun
	case strings.HasSuffix(noun, "y"):
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
