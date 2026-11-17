package config

import (
	"fmt"
	"sort"
	"strings"
)

// Error is one problem in the configuration. It names the file, the object
// and the field at fault, and never contains a secret.
type Error struct {
	// File is the path of the file, and Line the line of the field or, when
	// that is not known, of the object. Both are empty for objects that did
	// not come from a file.
	File string
	Line int
	// Object is the kind and name of the object, such as `source "prod"`.
	Object string
	// Field is the path of the field within the object, such as
	// severity.default or steps[1].timeout.
	Field string
	Msg   string
}

func (e Error) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
		}
		b.WriteString(": ")
	}
	if e.Object != "" {
		b.WriteString(e.Object + ": ")
	}
	if e.Field != "" {
		b.WriteString(e.Field + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors is every problem found in one run.
type Errors []Error

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// sorted orders the errors by file and line, keeping the order in which
// problems on the same line were found.
func (es Errors) sorted() Errors {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].File != es[j].File {
			return es[i].File < es[j].File
		}
		return es[i].Line < es[j].Line
	})
	return es
}

// err returns the errors as an error, or nil when there are none.
func (es Errors) err() error {
	if len(es) == 0 {
		return nil
	}
	return es.sorted()
}
