package cli

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/thanoslabs/beacon/internal/postgres"
)

// Settings can be given as a flag or as an environment variable. The flag
// wins, then the environment variable, then the default. The helpers below
// register a flag and apply that order.

// stringSetting registers a string flag that falls back to envName.
func stringSetting(fs *flag.FlagSet, e env, p *string, name, envName, def, usage string) {
	if v := e.getenv(envName); v != "" {
		def = v
	}
	fs.StringVar(p, name, def, fmt.Sprintf("%s (env %s)", usage, envName))
}

// boolSetting registers a bool flag that falls back to envName.
func boolSetting(fs *flag.FlagSet, e env, p *bool, name, envName string, def bool, usage string, envErrs *[]error) {
	if v := e.getenv(envName); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*envErrs = append(*envErrs, fmt.Errorf("%s: %q is not true or false", envName, v))
		} else {
			def = b
		}
	}
	fs.BoolVar(p, name, def, fmt.Sprintf("%s (env %s)", usage, envName))
}

// secretSetting registers a string flag that falls back to envName, for values
// that hold credentials. The value from the environment is deliberately not
// made the flag's default, so the command's help never prints it.
func secretSetting(fs *flag.FlagSet, e env, p *string, name, envName, usage string) {
	*p = e.getenv(envName)
	fs.Func(name, fmt.Sprintf("%s (env %s)", usage, envName), func(v string) error {
		*p = v
		return nil
	})
}

// intSetting registers an int flag that falls back to envName. A value in the
// environment that is not a number is reported when the flags are parsed.
func intSetting(fs *flag.FlagSet, e env, p *int, name, envName string, def int, usage string, envErrs *[]error) {
	if v := e.getenv(envName); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			*envErrs = append(*envErrs, fmt.Errorf("%s: %q is not a whole number", envName, v))
		} else {
			def = n
		}
	}
	fs.IntVar(p, name, def, fmt.Sprintf("%s (env %s)", usage, envName))
}

// durationSetting registers a duration flag that falls back to envName.
func durationSetting(fs *flag.FlagSet, e env, p *time.Duration, name, envName string, def time.Duration, usage string, envErrs *[]error) {
	if v := e.getenv(envName); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			*envErrs = append(*envErrs, fmt.Errorf("%s: %q is not a duration such as 30s", envName, v))
		} else {
			def = d
		}
	}
	fs.DurationVar(p, name, def, fmt.Sprintf("%s (env %s)", usage, envName))
}

// databaseSettings registers the flags shared by every command that connects
// to Postgres.
func databaseSettings(fs *flag.FlagSet, e env, cfg *postgres.Config, envErrs *[]error) {
	secretSetting(fs, e, &cfg.URL, "database-url", "BEACON_DATABASE_URL",
		"Postgres connection URL, for example postgres://user:password@host:5432/beacon")
	intSetting(fs, e, &cfg.MaxConns, "database-max-conns", "BEACON_DATABASE_MAX_CONNS", 0,
		"maximum connections in the pool; 0 uses the driver default", envErrs)
	durationSetting(fs, e, &cfg.ConnectTimeout, "database-connect-timeout", "BEACON_DATABASE_CONNECT_TIMEOUT",
		postgres.DefaultConnectTimeout, "how long to wait for the first database connection", envErrs)
}

// checkDatabaseSettings validates what databaseSettings collected.
func checkDatabaseSettings(cfg postgres.Config) error {
	if cfg.URL == "" {
		return fmt.Errorf("the database URL is required: set --database-url or BEACON_DATABASE_URL")
	}
	if cfg.MaxConns < 0 || cfg.MaxConns > 10000 {
		return fmt.Errorf("--database-max-conns must be between 0 and 10000, got %d", cfg.MaxConns)
	}
	if cfg.ConnectTimeout <= 0 {
		return fmt.Errorf("--database-connect-timeout must be positive, got %s", cfg.ConnectTimeout)
	}
	return nil
}
