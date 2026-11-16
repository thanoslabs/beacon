package cli

import (
	"context"
	"fmt"

	"github.com/thanoslabs/beacon/internal/version"
)

func runVersion(_ context.Context, e env, args []string) error {
	fs := newFlagSet(e, "version", "Print the version and exit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_, err := fmt.Fprintln(e.stdout, version.Get())
	return err
}
