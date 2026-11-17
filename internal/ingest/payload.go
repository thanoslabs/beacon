package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// The values of the payload's status field.
const (
	statusFiring   = "firing"
	statusResolved = "resolved"
)

// maxFingerprintLength bounds a fingerprint chosen by the sender.
const maxFingerprintLength = 256

// payload is the body of the generic JSON webhook, after validation.
type payload struct {
	Status      string
	Fingerprint string
	Labels      map[string]string
	Annotations map[string]string
	StartsAt    time.Time
	EndsAt      time.Time
}

// fieldError is a problem with the payload. field names the part at fault,
// and is empty when the body as a whole is wrong.
type fieldError struct {
	field string
	msg   string
}

func (e *fieldError) Error() string {
	if e.field == "" {
		return e.msg
	}
	return e.field + ": " + e.msg
}

// parsePayload decodes and validates a body. Every error is a *fieldError.
//
// The fields are decoded one at a time rather than into a struct, so that each
// error can name the field at fault.
func parsePayload(body []byte) (payload, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return payload{}, &fieldError{msg: "the body must be a JSON object"}
	}

	p := payload{Status: statusFiring}
	for name, value := range raw {
		var err error
		switch name {
		case "status":
			if err = stringField(name, value, &p.Status); err == nil && p.Status != statusFiring && p.Status != statusResolved {
				err = &fieldError{field: name, msg: fmt.Sprintf("must be %q or %q", statusFiring, statusResolved)}
			}
		case "fingerprint":
			if err = stringField(name, value, &p.Fingerprint); err == nil && (p.Fingerprint == "" || len(p.Fingerprint) > maxFingerprintLength) {
				err = &fieldError{field: name, msg: fmt.Sprintf("must be between 1 and %d characters", maxFingerprintLength)}
			}
		case "labels":
			p.Labels, err = stringMapField(name, value)
		case "annotations":
			p.Annotations, err = stringMapField(name, value)
		case "starts_at":
			p.StartsAt, err = timeField(name, value)
		case "ends_at":
			p.EndsAt, err = timeField(name, value)
		default:
			err = &fieldError{field: name, msg: "unknown field"}
		}
		if err != nil {
			return payload{}, err
		}
	}

	switch {
	case p.Status == statusFiring && len(p.Labels) == 0:
		return payload{}, &fieldError{field: "labels", msg: "a firing alert needs at least one label"}
	case p.Fingerprint == "" && len(p.Labels) == 0:
		return payload{}, &fieldError{field: "labels", msg: "give the labels or the fingerprint of the alert to resolve"}
	case p.Status == statusFiring && !p.EndsAt.IsZero():
		return payload{}, &fieldError{field: "ends_at", msg: `is only allowed when status is "resolved"`}
	}
	return p, nil
}

func stringField(name string, value json.RawMessage, into *string) error {
	// A JSON null would decode into a string without an error.
	if err := json.Unmarshal(value, into); err != nil || isNull(value) {
		return &fieldError{field: name, msg: "must be a string"}
	}
	return nil
}

func stringMapField(name string, value json.RawMessage) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(value, &raw); err != nil || raw == nil {
		return nil, &fieldError{field: name, msg: "must be an object of string values"}
	}
	out := make(map[string]string, len(raw))
	for key, v := range raw {
		if key == "" {
			return nil, &fieldError{field: name, msg: "must not have an empty name"}
		}
		var s string
		if err := stringField(name+"."+key, v, &s); err != nil {
			return nil, err
		}
		out[key] = s
	}
	return out, nil
}

func timeField(name string, value json.RawMessage) (time.Time, error) {
	var s string
	if err := stringField(name, value, &s); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, &fieldError{field: name, msg: "must be a time in RFC 3339 form, such as 2026-01-02T15:04:05Z"}
	}
	return t, nil
}

func isNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
