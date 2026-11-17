package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// The password of the bootstrap admin: long enough to resist guessing at the
// rate the sign-in allows, and within what bcrypt reads.
const (
	minPasswordLength = 12
	maxPasswordBytes  = 72
	// bcryptCost is the work factor of the hashes this command makes.
	bcryptCost = 12
)

func runHashPassword(_ context.Context, e env, args []string) error {
	fs := newFlagSet(e, "hash-password", "Hash a password for the bootstrap admin (auth.bootstrap_admin.password_hash). "+
		"The password is read from standard input: typed without being shown, or piped in")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	password, err := readPassword(e)
	if err != nil {
		return err
	}
	switch {
	case len([]rune(password)) < minPasswordLength:
		return fmt.Errorf("the password must be at least %d characters", minPasswordLength)
	case len(password) > maxPasswordBytes:
		return fmt.Errorf("the password must be at most %d bytes, which is all that bcrypt reads", maxPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash the password: %w", err)
	}
	fmt.Fprintln(e.stdout, string(hash))
	return nil
}

// readPassword reads the password from standard input. On a terminal it is
// asked for twice and not shown; otherwise the first line is taken.
func readPassword(e env) (string, error) {
	if f, ok := e.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		ask := func(prompt string) (string, error) {
			fmt.Fprint(e.stderr, prompt)
			typed, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(e.stderr)
			return string(typed), err
		}
		first, err := ask("Password: ")
		if err != nil {
			return "", fmt.Errorf("read the password: %w", err)
		}
		again, err := ask("Again: ")
		if err != nil {
			return "", fmt.Errorf("read the password: %w", err)
		}
		if first != again {
			return "", errors.New("the two passwords differ")
		}
		return first, nil
	}
	line, err := bufio.NewReader(io.LimitReader(e.stdin, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
