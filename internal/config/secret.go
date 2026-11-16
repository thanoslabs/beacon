package config

import (
	"encoding/json"
	"log/slog"
)

// redacted is what a Secret shows instead of its value.
const redacted = "[redacted]"

// SecretRef says where a secret comes from. Exactly one field is set.
type SecretRef struct {
	// Env is the name of an environment variable.
	Env string `json:"env,omitempty"`
	// File is the path of a file that holds the secret.
	File string `json:"file,omitempty"`
}

// Secret is a credential, such as a token. Its value is only reachable
// through Reveal: printing, logging or encoding a Secret shows where it comes
// from, never what it is.
type Secret struct {
	ref   SecretRef
	value string
}

// NewSecret returns a secret with the given value, which came from ref.
func NewSecret(ref SecretRef, value string) Secret {
	return Secret{ref: ref, value: value}
}

// Reveal returns the secret value. Use it only where the value is needed, and
// never pass the result to a logger or into an error.
func (s Secret) Reveal() string { return s.value }

// IsSet reports whether the secret has a value.
func (s Secret) IsSet() bool { return s.value != "" }

// String implements fmt.Stringer, so that %v and %s do not print the value.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so that %#v does not print the value.
func (s Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON encodes where the secret comes from, which is the form stored
// in the database: secret references are left unresolved.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.ref) }
