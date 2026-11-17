package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// LoadOptions changes how Load treats secrets.
type LoadOptions struct {
	// Getenv looks up the environment variables that secrets refer to. When
	// it is nil no secret is read, from the environment or from files: the
	// references are only checked for their form. That is what lets
	// configuration be checked in CI, where the secrets are absent.
	Getenv func(string) string
}

// document is the shape of every configuration file: a key per kind of
// object, each holding a list. routing is the exception.
type document struct {
	Sources            []Source           `yaml:"sources"`
	Integrations       []Integration      `yaml:"integrations"`
	Teams              []Team             `yaml:"teams"`
	Schedules          []Schedule         `yaml:"schedules"`
	EscalationPolicies []EscalationPolicy `yaml:"escalation_policies"`
	Routing            *Routing           `yaml:"routing"`
	Auth               *Auth              `yaml:"auth"`
}

// The names of the kinds, as error messages give them.
const (
	kindSource      = "source"
	kindIntegration = "integration"
	kindTeam        = "team"
	kindSchedule    = "schedule"
	kindPolicy      = "escalation policy"
	kindRouting     = "routing"
	kindRule        = "routing rule"
	kindAuth        = "auth"
)

// listKinds maps the top-level keys that hold lists to their kinds.
var listKinds = map[string]string{
	"sources":             kindSource,
	"integrations":        kindIntegration,
	"teams":               kindTeam,
	"schedules":           kindSchedule,
	"escalation_policies": kindPolicy,
}

// Load reads the configuration at path: one YAML file, or a directory, which
// is read recursively, taking every *.yaml and *.yml file in lexical order.
// Files and directories whose names start with a dot are skipped.
//
// Load checks the structure of the files: YAML syntax, unknown fields, the
// type of each value and the form of secrets. It does not check what the
// values mean; that is Validate. When anything is wrong the error is an
// Errors holding every problem found.
func Load(path string, opts LoadOptions) (Config, error) {
	files, err := configFiles(path)
	if err != nil {
		return Config{}, err
	}
	var (
		cfg         Config
		errs        Errors
		routingFile string
		authFile    string
	)
	secrets := secretReader{getenv: opts.Getenv, readFile: os.ReadFile}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, Error{File: file, Msg: "cannot read the file: " + pathErrorCause(err)})
			continue
		}
		doc, fileErrs := parseFile(file, raw)
		errs = append(errs, fileErrs...)
		if opts.Getenv != nil {
			errs = append(errs, resolveSecrets(&doc, secrets, filepath.Dir(file))...)
		}

		cfg.Sources = append(cfg.Sources, doc.Sources...)
		cfg.Integrations = append(cfg.Integrations, doc.Integrations...)
		cfg.Teams = append(cfg.Teams, doc.Teams...)
		cfg.Schedules = append(cfg.Schedules, doc.Schedules...)
		cfg.EscalationPolicies = append(cfg.EscalationPolicies, doc.EscalationPolicies...)
		switch {
		case doc.Routing == nil:
		case cfg.Routing != nil:
			errs = append(errs, Error{File: file, Line: doc.Routing.Origin.Line, Object: kindRouting,
				Msg: fmt.Sprintf("routing is already defined in %s; its rules are ordered, so the whole block must be in one file", routingFile)})
		default:
			cfg.Routing, routingFile = doc.Routing, file
		}
		switch {
		case doc.Auth == nil:
		case cfg.Auth != nil:
			errs = append(errs, Error{File: file, Line: doc.Auth.Origin.Line, Object: kindAuth,
				Msg: fmt.Sprintf("auth is already defined in %s; it is one group of settings, so it must be in one file", authFile)})
		default:
			cfg.Auth, authFile = doc.Auth, file
		}
	}
	return cfg, errs.err()
}

// configFiles lists the files to read for path.
func configFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, Errors{{File: path, Msg: "cannot read the configuration: " + pathErrorCause(err)}}
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var files []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Hidden entries are skipped. Among them are the ..data and timestamped
		// directories of a Kubernetes ConfigMap volume, which would otherwise
		// make every file appear twice.
		if p != path && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); !d.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, Errors{{File: path, Msg: "cannot read the configuration directory: " + pathErrorCause(err)}}
	}
	if len(files) == 0 {
		return nil, Errors{{File: path, Msg: "the directory holds no *.yaml or *.yml file"}}
	}
	sort.Strings(files)
	return files, nil
}

// pathErrorCause returns the cause of a file system error without the path,
// which the caller reports as the file.
func pathErrorCause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// objectRef is where one object starts in a file.
type objectRef struct {
	kind string
	// label is the kind and name, or the kind and position when the object
	// has no usable name.
	label string
	node  *yaml.Node
}

// parseFile decodes one file and gives each object its origin.
func parseFile(file string, raw []byte) (document, Errors) {
	var doc document

	// The file is parsed twice. The first pass keeps the nodes, which carry
	// the line numbers that turn a decoding error into the object and field
	// at fault. The second decodes into the types and rejects unknown fields.
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return doc, Errors{syntaxError(file, err)}
	}
	if len(root.Content) == 0 {
		return doc, nil // an empty file, or only comments
	}
	refs := indexObjects(root.Content[0])

	var errs Errors
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	err := dec.Decode(&doc)
	var typeErr *yaml.TypeError
	switch {
	case err == nil:
	case errors.As(err, &typeErr):
		for _, msg := range typeErr.Errors {
			errs = append(errs, decodeError(file, root.Content[0], refs, msg))
		}
	default:
		return doc, Errors{syntaxError(file, err)}
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		errs = append(errs, Error{File: file, Msg: "the file holds more than one YAML document; use one document per file"})
	}

	// A list item that is not a mapping is dropped by the decoder, so
	// positions no longer line up. It is reported above, and without origins
	// nothing is lost.
	origin := func(kind string, n int) func(i int) Origin {
		var of []objectRef
		for _, r := range refs {
			if r.kind == kind {
				of = append(of, r)
			}
		}
		return func(i int) Origin {
			if len(of) != n {
				return Origin{File: file}
			}
			fields := map[string]int{}
			fieldLines(of[i].node, "", fields)
			return Origin{File: file, Line: of[i].node.Line, fields: fields}
		}
	}
	at := origin(kindSource, len(doc.Sources))
	for i := range doc.Sources {
		doc.Sources[i].Origin = at(i)
	}
	at = origin(kindIntegration, len(doc.Integrations))
	for i := range doc.Integrations {
		doc.Integrations[i].Origin = at(i)
	}
	at = origin(kindTeam, len(doc.Teams))
	for i := range doc.Teams {
		doc.Teams[i].Origin = at(i)
	}
	at = origin(kindSchedule, len(doc.Schedules))
	for i := range doc.Schedules {
		doc.Schedules[i].Origin = at(i)
	}
	at = origin(kindPolicy, len(doc.EscalationPolicies))
	for i := range doc.EscalationPolicies {
		doc.EscalationPolicies[i].Origin = at(i)
	}
	if doc.Auth != nil {
		doc.Auth.Origin = origin(kindAuth, 1)(0)
	}
	if doc.Routing != nil {
		doc.Routing.Origin = origin(kindRouting, 1)(0)
		at = origin(kindRule, len(doc.Routing.Rules))
		for i := range doc.Routing.Rules {
			doc.Routing.Rules[i].Origin = at(i)
		}
	}
	return doc, errs
}

// indexObjects lists where each object of a file starts, in file order.
func indexObjects(top *yaml.Node) []objectRef {
	var refs []objectRef
	list := func(kind string, seq *yaml.Node) {
		if seq.Kind != yaml.SequenceNode {
			return
		}
		for i, item := range seq.Content {
			label := fmt.Sprintf("%s %d", kind, i+1)
			if name := mappingValue(item, "name"); name != nil && name.Kind == yaml.ScalarNode && name.Value != "" {
				label = fmt.Sprintf("%s %q", kind, name.Value)
			}
			refs = append(refs, objectRef{kind: kind, label: label, node: item})
		}
	}
	if top.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, value := top.Content[i], top.Content[i+1]
		if kind, ok := listKinds[key.Value]; ok {
			list(kind, value)
		}
		if key.Value == "auth" && value.Kind == yaml.MappingNode {
			refs = append(refs, objectRef{kind: kindAuth, label: kindAuth, node: value})
		}
		if key.Value == "routing" && value.Kind == yaml.MappingNode {
			// The rules come first: they lie inside the routing block, and
			// the innermost object that holds a line is the one named.
			if rules := mappingValue(value, "rules"); rules != nil {
				list(kindRule, rules)
			}
			refs = append(refs, objectRef{kind: kindRouting, label: kindRouting, node: value})
		}
	}
	return refs
}

// fieldLines records the line of every field under n, by path.
func fieldLines(n *yaml.Node, prefix string, into map[string]int) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			path := joinPath(prefix, n.Content[i].Value)
			if prefix == "" {
				path = n.Content[i].Value
			}
			into[path] = n.Content[i].Line
			fieldLines(n.Content[i+1], path, into)
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			into[path] = item.Line
			fieldLines(item, path, into)
		}
	}
}

// lastLine returns the last line that n reaches.
func lastLine(n *yaml.Node) int {
	last := n.Line
	for _, c := range n.Content {
		last = max(last, lastLine(c))
	}
	return last
}

// mappingValue returns the value of key in a mapping node, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

var (
	lineRE = regexp.MustCompile(`(?s)^(?:yaml: )?line (\d+): (?:column (\d+): )?(.*)$`)
	// The decoder's messages name Go types and quote the offending value.
	// They are rewritten: users do not know the types, and a quoted value
	// could be a secret typed into the wrong field.
	unknownFieldRE = regexp.MustCompile(`^field (\S+) not found in type \S+$`)
	duplicateKeyRE = regexp.MustCompile(`^mapping key "(.*)" already defined at line (\d+)$`)
	wrongTypeRE    = regexp.MustCompile(`(?s)^cannot unmarshal !!(\w+)(?: .*)? into (\S+)$`)
)

// yamlKinds and goKinds give the plain names of the YAML tags and Go types
// that the decoder's messages mention.
var (
	yamlKinds = map[string]string{"str": "a string", "int": "a number", "float": "a number", "bool": "true or false",
		"seq": "a list", "map": "a mapping", "null": "empty", "timestamp": "a date"}
	goKinds = map[string]string{"string": "a string", "int": "a whole number", "time.Time": "a date and time such as 2026-01-05T09:00:00Z"}
)

// decodeError turns one message of the decoder into an Error that names the
// object and field at the message's line.
func decodeError(file string, top *yaml.Node, refs []objectRef, msg string) Error {
	e := Error{File: file, Msg: msg}
	column := 0
	if m := lineRE.FindStringSubmatch(msg); m != nil {
		e.Line, _ = strconv.Atoi(m[1])
		column, _ = strconv.Atoi(m[2])
		e.Msg = m[3]
	}
	// Which field on the line is at fault. The decoder gives only the line,
	// so the message decides: an unknown or repeated field is the key of
	// that name, and a value of the wrong type belongs to the first key on
	// the line. Beacon's own types give the exact position.
	onLine := func(key, value *yaml.Node) bool {
		if key == nil {
			return value.Kind != yaml.MappingNode && value.Line == e.Line
		}
		return key.Line == e.Line
	}
	named := func(name string) func(key, value *yaml.Node) bool {
		return func(key, _ *yaml.Node) bool { return key != nil && key.Line == e.Line && key.Value == name }
	}
	match := onLine
	switch {
	case column > 0:
		match = func(_, value *yaml.Node) bool { return value.Line == e.Line && value.Column == column }
	case unknownFieldRE.MatchString(e.Msg):
		match = named(unknownFieldRE.FindStringSubmatch(e.Msg)[1])
		e.Msg = "unknown field"
	case duplicateKeyRE.MatchString(e.Msg):
		m := duplicateKeyRE.FindStringSubmatch(e.Msg)
		match = named(m[1])
		e.Msg = "the field is set twice; first at line " + m[2]
	case wrongTypeRE.MatchString(e.Msg):
		m := wrongTypeRE.FindStringSubmatch(e.Msg)
		got, ok := yamlKinds[m[1]]
		if !ok {
			got = "a " + m[1]
		}
		want, ok := goKinds[m[2]]
		switch {
		case ok:
		case strings.HasPrefix(m[2], "[]"):
			want = "a list"
		case strings.HasPrefix(m[2], "map["):
			want = "a mapping of names to values"
		default:
			want = "a mapping of fields"
		}
		e.Msg = fmt.Sprintf("must be %s, not %s", want, got)
	}
	if e.Line == 0 {
		return e
	}
	// The object is the one that spans the line, and the innermost when one
	// lies inside another.
	var obj *objectRef
	for i := range refs {
		if r := &refs[i]; r.node.Line <= e.Line && e.Line <= lastLine(r.node) && (obj == nil || r.node.Line > obj.node.Line) {
			obj = r
		}
	}
	if obj == nil {
		// Not inside any object: a field at the top of the file.
		e.Field, _ = findPath(top, match)
		return e
	}
	e.Object = obj.label
	e.Field, _ = findPath(obj.node, match)
	return e
}

// syntaxError reports YAML that could not be parsed at all.
func syntaxError(file string, err error) Error {
	e := Error{File: file, Msg: strings.TrimPrefix(err.Error(), "yaml: ")}
	if m := lineRE.FindStringSubmatch(err.Error()); m != nil {
		e.Line, _ = strconv.Atoi(m[1])
		e.Msg = m[3]
	}
	e.Msg = "invalid YAML: " + e.Msg
	return e
}

// findPath returns the path, within n, of the first field that match
// accepts, such as severity.default or steps[1].timeout. match is called with
// each key and its value, and with a nil key for each item of a list.
func findPath(n *yaml.Node, match func(key, value *yaml.Node) bool) (string, bool) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if match(key, value) {
				return key.Value, true
			}
			if sub, ok := findPath(value, match); ok {
				return joinPath(key.Value, sub), true
			}
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			if match(nil, item) {
				return fmt.Sprintf("[%d]", i), true
			}
			if sub, ok := findPath(item, match); ok {
				return joinPath(fmt.Sprintf("[%d]", i), sub), true
			}
		}
	}
	return "", false
}

func joinPath(parent, child string) string {
	if child == "" || strings.HasPrefix(child, "[") {
		return parent + child
	}
	return parent + "." + child
}

// resolveSecrets reads the value of every secret in doc.
func resolveSecrets(doc *document, r secretReader, dir string) Errors {
	var errs Errors
	resolve := func(s *Secret, o Origin, object, field string) {
		if err := r.resolve(s, dir); err != nil {
			errs = append(errs, Error{File: o.File, Line: o.lineOf(field), Object: object, Field: field, Msg: err.Error()})
		}
	}
	for i := range doc.Sources {
		s := &doc.Sources[i]
		resolve(&s.Token, s.Origin, label(kindSource, s.Name), "token")
	}
	for i := range doc.Integrations {
		in := &doc.Integrations[i]
		object := label(kindIntegration, in.Name)
		if in.Slack != nil {
			resolve(&in.Slack.BotToken, in.Origin, object, "slack.bot_token")
			resolve(&in.Slack.SigningSecret, in.Origin, object, "slack.signing_secret")
		}
		if in.Discord != nil {
			resolve(&in.Discord.BotToken, in.Origin, object, "discord.bot_token")
		}
		if in.Email != nil {
			resolve(&in.Email.Password, in.Origin, object, "email.password")
		}
		if in.Webhook != nil {
			for name, value := range in.Webhook.Headers {
				resolve(&value.Secret, in.Origin, object, "webhook.headers."+name)
				in.Webhook.Headers[name] = value
			}
		}
	}
	if a := doc.Auth; a != nil && a.OIDC != nil {
		resolve(&a.OIDC.ClientSecret, a.Origin, kindAuth, "oidc.client_secret")
	}
	return errs
}

// label names an object in an error message.
func label(kind, name string) string {
	return fmt.Sprintf("%s %q", kind, name)
}
