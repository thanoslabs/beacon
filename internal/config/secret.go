package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
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

// Ref returns where the secret comes from.
func (s Secret) Ref() SecretRef { return s.ref }

// IsReferenced reports whether the secret names an environment variable or a
// file, whether or not its value has been read.
func (s Secret) IsReferenced() bool { return s.ref != SecretRef{} }

// inlineSecretMsg is the error for a secret written into a file. It must not
// repeat the value.
const inlineSecretMsg = "secrets are never written inline; use {env: NAME} or {file: /path}"

// UnmarshalYAML reads a reference: {env: NAME} or {file: /path}. The value is
// read later, by Load. A plain string is an error, and the error does not
// repeat it.
func (s *Secret) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return typeError(n, inlineSecretMsg)
	}
	var ref SecretRef
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if value.Kind != yaml.ScalarNode || value.Value == "" {
			return typeError(n, "must be the name of an environment variable or the path of a file")
		}
		switch key.Value {
		case "env":
			ref.Env = value.Value
		case "file":
			ref.File = value.Value
		default:
			return typeError(n, fmt.Sprintf("unknown field %q; a secret takes env or file", key.Value))
		}
	}
	if (ref.Env == "") == (ref.File == "") {
		return typeError(n, "a secret takes exactly one of env and file")
	}
	*s = Secret{ref: ref}
	return nil
}

// secretReader reads secret values from the environment and from files.
type secretReader struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
}

// resolve reads the value s refers to. A relative file path is taken from
// dir, the directory of the configuration file that names it. The errors name
// the variable or the file, never a value.
func (r secretReader) resolve(s *Secret, dir string) error {
	switch {
	case s.ref.Env != "":
		s.value = r.getenv(s.ref.Env)
		if s.value == "" {
			return fmt.Errorf("the environment variable %s is not set", s.ref.Env)
		}
	case s.ref.File != "":
		path := s.ref.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		raw, err := r.readFile(path)
		if err != nil {
			// The error of os.ReadFile names the path and the cause only.
			return fmt.Errorf("read the secret file: %w", err)
		}
		// Files usually end with a newline that is not part of the secret.
		s.value = strings.TrimRight(string(raw), "\r\n")
		if s.value == "" {
			return fmt.Errorf("the secret file %s is empty", path)
		}
	}
	return nil
}

// HeaderValue is the value of an outbound webhook header: a plain string, or
// a secret reference for headers that carry credentials.
type HeaderValue struct {
	// Plain is the value when it is not a secret.
	Plain string
	// Secret is set when the value was given as a reference.
	Secret Secret
}

// Reveal returns the value to send.
func (h HeaderValue) Reveal() string {
	if h.Secret.IsReferenced() {
		return h.Secret.Reveal()
	}
	return h.Plain
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (h *HeaderValue) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*h = HeaderValue{Plain: n.Value}
		return nil
	}
	return h.Secret.UnmarshalYAML(n)
}

// MarshalJSON encodes a plain value as a string and a secret as its
// reference.
func (h HeaderValue) MarshalJSON() ([]byte, error) {
	if h.Secret.IsReferenced() {
		return json.Marshal(h.Secret)
	}
	return json.Marshal(h.Plain)
}
