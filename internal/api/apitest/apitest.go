// Package apitest checks what a server answers against the OpenAPI
// description of the API. It is what keeps the description true: tests send
// requests to the real server and hand every answer to a Spec.
//
// It understands the part of OpenAPI 3.0 and of JSON Schema that the
// description uses, and refuses the rest, so that the description cannot
// say something no test would check.
package apitest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/api"
)

// Operation is one route of the description.
type Operation struct {
	// Method and Path are as in the description: the path has its
	// parameters in braces, as the patterns of the server do.
	Method, Path string
	// Access is public, member or admin.
	Access  string
	Summary string
	Tag     string
	// Public says that the operation asks for no credential.
	Public    bool
	responses map[string]any
}

// Key is the operation as a route pattern of the server: "GET /api/v1/me".
func (o Operation) Key() string { return o.Method + " " + o.Path }

// Spec is the description, read.
type Spec struct {
	doc        map[string]any
	operations map[string]Operation

	mu   sync.Mutex
	seen map[string]map[int]bool
}

// Load reads the description that the binary embeds.
func Load(t testing.TB) *Spec {
	t.Helper()
	raw, err := api.OpenAPIJSON()
	if err != nil {
		t.Fatalf("read the OpenAPI description: %v", err)
	}
	s := &Spec{operations: map[string]Operation{}, seen: map[string]map[int]bool{}}
	if err := json.Unmarshal(raw, &s.doc); err != nil {
		t.Fatalf("the OpenAPI description is not JSON: %v", err)
	}
	paths, _ := s.doc["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, raw := range methods {
			op, _ := raw.(map[string]any)
			o := Operation{Method: strings.ToUpper(method), Path: path}
			o.Access, _ = op["x-beacon-access"].(string)
			o.Summary, _ = op["summary"].(string)
			if tags, _ := op["tags"].([]any); len(tags) > 0 {
				o.Tag, _ = tags[0].(string)
			}
			if security, set := op["security"].([]any); set && len(security) == 0 {
				o.Public = true
			}
			o.responses, _ = op["responses"].(map[string]any)
			s.operations[o.Key()] = o
		}
	}
	return s
}

// Operations returns every operation, by path and then by method.
func (s *Spec) Operations() []Operation {
	out := make([]Operation, 0, len(s.operations))
	for _, o := range s.operations {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Match returns the operation that a request with this method and path
// belongs to: the path of the description whose parameters fit.
func (s *Spec) Match(method, path string) (Operation, bool) {
	path, _, _ = strings.Cut(path, "?")
	var best Operation
	found := false
	for _, o := range s.operations {
		if o.Method != method {
			continue
		}
		pattern := "^" + regexp.MustCompile(`\\\{[a-z]+\\\}`).ReplaceAllString(regexp.QuoteMeta(o.Path), `[^/]+`) + "$"
		if !regexp.MustCompile(pattern).MatchString(path) {
			continue
		}
		// A path without parameters wins over one with: /silences/preview
		// over /silences/{id}.
		if !found || strings.Count(o.Path, "{") < strings.Count(best.Path, "{") {
			best, found = o, true
		}
	}
	return best, found
}

// Check validates an answer against the description of the operation: the
// status must be one the operation describes, and a JSON body must fit the
// schema of that status. It records that the answer was seen.
func (s *Spec) Check(o Operation, status int, contentType string, body []byte) error {
	response, described := o.responses[strconv.Itoa(status)].(map[string]any)
	if !described {
		response, described = o.responses["default"].(map[string]any)
		if !described || status < 400 {
			return fmt.Errorf("%s answered %d, which the description does not have", o.Key(), status)
		}
	}
	response = s.deref(response)
	s.mu.Lock()
	if s.seen[o.Key()] == nil {
		s.seen[o.Key()] = map[int]bool{}
	}
	s.seen[o.Key()][status] = true
	s.mu.Unlock()

	content, _ := response["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		// Not JSON, or no body: there is nothing to check it against.
		return nil
	}
	if !strings.HasPrefix(contentType, "application/json") {
		return fmt.Errorf("%s answered %d as %q, want application/json", o.Key(), status, contentType)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("%s answered %d with a body that is not JSON: %.200s", o.Key(), status, body)
	}
	schema, _ := media["schema"].(map[string]any)
	if err := s.validate(schema, value, "body"); err != nil {
		return fmt.Errorf("%s answered %d with a body that the description does not allow: %w\n%.600s", o.Key(), status, err, body)
	}
	return nil
}

// Unseen lists the operations of which no successful answer was checked.
func (s *Spec) Unseen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for key := range s.operations {
		ok := false
		for status := range s.seen[key] {
			ok = ok || status < 400
		}
		if !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// deref follows a $ref inside the document.
func (s *Spec) deref(node map[string]any) map[string]any {
	for range 10 {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		var at any = s.doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := at.(map[string]any)
			at = m[part]
		}
		next, _ := at.(map[string]any)
		if next == nil {
			return map[string]any{"x-missing-ref": ref}
		}
		node = next
	}
	return node
}

// known are the keywords of a schema that validate understands. A schema
// that uses another is refused: it would promise a check nobody makes.
var known = map[string]bool{
	"type": true, "nullable": true, "enum": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"format": true, "description": true, "minimum": true, "maximum": true, "default": true,
}

func (s *Spec) validate(schema map[string]any, value any, at string) error {
	schema = s.deref(schema)
	if ref, missing := schema["x-missing-ref"]; missing {
		return fmt.Errorf("%s: the description refers to %v, which it does not have", at, ref)
	}
	for keyword := range schema {
		if !known[keyword] {
			return fmt.Errorf("%s: the schema uses %q, which the test does not understand", at, keyword)
		}
	}
	if value == nil {
		if nullable, _ := schema["nullable"].(bool); nullable {
			return nil
		}
		return fmt.Errorf("%s: is null, and the schema does not say nullable", at)
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, allowed := range enum {
			found = found || allowed == value
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", at, value, enum)
		}
	}
	switch kind, _ := schema["type"].(string); kind {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: is %T, want an object", at, value)
		}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, name := range required {
			if _, has := object[name.(string)]; !has {
				return fmt.Errorf("%s: the field %q is missing", at, name)
			}
		}
		for name, v := range object {
			property, described := properties[name].(map[string]any)
			switch additional := schema["additionalProperties"].(type) {
			case nil:
				if !described {
					// The default of JSON Schema is to allow anything, which
					// is how a description drifts.
					return fmt.Errorf("%s: the schema must say additionalProperties", at)
				}
			case bool:
				if !described && !additional {
					return fmt.Errorf("%s: the field %q is not in the description", at, name)
				}
			case map[string]any:
				if !described {
					property = additional
				}
			}
			if property != nil {
				if err := s.validate(property, v, at+"."+name); err != nil {
					return err
				}
			}
		}
	case "array":
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: is %T, want an array", at, value)
		}
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return fmt.Errorf("%s: the schema of an array must say items", at)
		}
		for i, v := range list {
			if err := s.validate(items, v, at+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case "string":
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: is %T, want a string", at, value)
		}
		if schema["format"] == "date-time" {
			t, err := time.Parse(time.RFC3339, str)
			if err != nil {
				return fmt.Errorf("%s: %q is not a time in RFC 3339 form", at, str)
			}
			// Times are answered in UTC.
			if _, offset := t.Zone(); offset != 0 {
				return fmt.Errorf("%s: %q is not in UTC", at, str)
			}
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("%s: is %v, want a whole number", at, value)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: is %T, want a number", at, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: is %T, want true or false", at, value)
		}
	default:
		return fmt.Errorf("%s: the schema has the type %q, which the test does not understand", at, kind)
	}
	return nil
}
