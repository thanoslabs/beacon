// Package api holds the parts of Beacon's REST API that belong to no other
// package, and the conventions every route follows. docs/api.md describes
// them for users; in short:
//
//   - A list is an object with one array, named after what it holds, and a
//     next_cursor when there is more: {"alerts": [...], "next_cursor": "…"}.
//   - A list that can grow without bound takes limit and cursor.
//   - An error is {"error": "...", "field": "..."}; field names the part of
//     the request at fault, when there is one.
//   - Times are RFC 3339, in UTC. Durations are written as in the
//     configuration: 30m, 2h, 1d.
//   - Something that is absent is null or left out, never an empty string.
//   - An identifier that is not one answers 404, like one that nothing has.
package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// The bounds of a page.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Error is the body of a failed request.
type Error struct {
	Error string `json:"error"`
	// Field names the part of the request at fault, when there is one.
	Field string `json:"field,omitempty"`
}

// Write answers with a status and a JSON body. Nothing the API answers may
// be kept by a cache: it is about who asks, or changes by the second.
func Write(logger *slog.Logger, w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}

// Fail answers with an error. field may be empty.
func Fail(logger *slog.Logger, w http.ResponseWriter, r *http.Request, status int, field, msg string) {
	Write(logger, w, r, status, Error{Error: msg, Field: field})
}

// Internal logs err and answers 500 without saying what went wrong.
func Internal(logger *slog.Logger, w http.ResponseWriter, r *http.Request, what string, err error) {
	logger.ErrorContext(r.Context(), what, slog.Any("error", err))
	Fail(logger, w, r, http.StatusInternalServerError, "", "internal error")
}

// Limit reads the limit of a list from the query: DefaultLimit when it is
// absent. ok is false, with what to tell the caller, when it is not a number
// between 1 and MaxLimit.
func Limit(q url.Values) (limit int, problem string, ok bool) {
	raw := q.Get("limit")
	if raw == "" {
		return DefaultLimit, "", true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, "must be a number between 1 and " + strconv.Itoa(MaxLimit), false
	}
	return n, "", true
}

// Cursor encodes where a list continues. A cursor is opaque to the caller:
// it is given back as it was received, and means nothing in another list.
func Cursor(position any) string {
	raw, err := json.Marshal(position)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ErrCursor means that a cursor is not one that Beacon made.
var ErrCursor = errors.New("is not a cursor from an earlier answer of this list")

// maxCursorLength bounds a cursor before it is decoded.
const maxCursorLength = 512

// ParseCursor decodes a cursor into position. An empty cursor leaves
// position as it is and reports false.
func ParseCursor(cursor string, position any) (present bool, err error) {
	if cursor == "" {
		return false, nil
	}
	if len(cursor) > maxCursorLength {
		return false, ErrCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return false, ErrCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(position); err != nil {
		return false, ErrCursor
	}
	return true, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s has the form of an identifier of a row.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }
