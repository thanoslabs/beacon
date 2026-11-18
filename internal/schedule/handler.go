package schedule

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// Path is the route of the schedules.
const Path = "/api/v1/schedules"

// maxRequestBytes bounds the body of a request.
const maxRequestBytes = 16 << 10

// Handler serves the schedules API.
type Handler struct {
	logger *slog.Logger
	store  *Store
}

// NewHandler returns a handler that reads and writes through store.
func NewHandler(logger *slog.Logger, store *Store) *Handler {
	return &Handler{logger: logger, store: store}
}

// Routes are the routes of the schedules. All are open to members. A
// schedule itself is provisioned and cannot be changed here; an override is
// runtime state, and covering for a colleague is part of being on call, so
// members create and remove overrides, their own and other people's. The
// author of every override is recorded.
func (h *Handler) Routes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + Path + "/{name}/overrides", Level: access.Member, Handler: http.HandlerFunc(h.listOverrides)},
		{Pattern: "POST " + Path + "/{name}/overrides", Level: access.Member, Handler: http.HandlerFunc(h.createOverride)},
		{Pattern: "DELETE " + Path + "/{name}/overrides/{id}", Level: access.Member, Handler: http.HandlerFunc(h.cancelOverride)},
	}
}

func (h *Handler) listOverrides(w http.ResponseWriter, r *http.Request) {
	var past bool
	switch r.URL.Query().Get("past") {
	case "", "false":
	case "true":
		past = true
	default:
		h.fail(w, r, http.StatusBadRequest, "past", "must be true or false")
		return
	}
	overrides, err := h.store.Overrides(r.Context(), r.PathValue("name"), past)
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
		return
	}
	if err != nil {
		h.internal(w, r, "list overrides", err)
		return
	}
	if overrides == nil {
		overrides = []Override{}
	}
	h.write(w, r, http.StatusOK, map[string][]Override{"overrides": overrides})
}

// overrideRequest is the body of a request for a new override.
type overrideRequest struct {
	// User is the email address of the person who covers.
	User string `json:"user"`
	// StartsAt and EndsAt are times in RFC 3339 form. Without StartsAt the
	// override begins now.
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
	// Duration is how long the override lasts, such as 8h, in place of
	// EndsAt.
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

const overrideExample = `the body must be JSON such as {"user": "grace@example.com", "starts_at": "2026-12-24T09:00:00Z", "ends_at": "2026-12-27T09:00:00Z", "reason": "covering the holidays"}`

func (h *Handler) createOverride(w http.ResponseWriter, r *http.Request) {
	var req overrideRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, r, http.StatusRequestEntityTooLarge, "", "the body is larger than 16 KiB")
			return
		}
		h.fail(w, r, http.StatusBadRequest, "", overrideExample+"; times are in RFC 3339 form")
		return
	}
	n := NewOverride{Schedule: r.PathValue("name"), User: req.User, Reason: req.Reason}
	if req.StartsAt != nil {
		n.StartsAt = *req.StartsAt
	}
	switch {
	case req.EndsAt != nil && req.Duration != "":
		h.fail(w, r, http.StatusBadRequest, "duration", "give ends_at or duration, not both")
		return
	case req.EndsAt != nil:
		n.EndsAt = *req.EndsAt
	case req.Duration != "":
		d, err := config.ParseDuration(req.Duration)
		if err != nil || d <= 0 || time.Duration(d) > MaxOverrideLength {
			h.fail(w, r, http.StatusBadRequest, "duration", "must be a duration such as 8h, 1d or 1w, at most a year")
			return
		}
		begin := n.StartsAt
		if now := h.store.now(); begin.Before(now) {
			begin = now
		}
		n.EndsAt = begin.Add(time.Duration(d))
	}

	// The author is whoever makes the request, never something in the body.
	by, _ := access.FromContext(r.Context())
	created, err := h.store.CreateOverride(r.Context(), n, by)
	var (
		bad     *Error
		overlap *OverlapError
	)
	switch {
	case errors.Is(err, ErrNotFound):
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
	case errors.As(err, &bad):
		h.fail(w, r, http.StatusBadRequest, bad.Field, bad.Msg)
	case errors.As(err, &overlap):
		h.fail(w, r, http.StatusConflict, "starts_at", overlap.Error())
	case err != nil:
		h.internal(w, r, "create override", err)
	default:
		h.logger.InfoContext(r.Context(), "schedule override created", slog.String("override_id", created.ID), slog.String("schedule", created.Schedule),
			slog.String("user", created.User), slog.Time("starts_at", created.StartsAt), slog.Time("ends_at", created.EndsAt),
			slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
		h.write(w, r, http.StatusCreated, created)
	}
}

func (h *Handler) cancelOverride(w http.ResponseWriter, r *http.Request) {
	cancelled, err := h.store.CancelOverride(r.Context(), r.PathValue("name"), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
	case errors.Is(err, ErrOverrideNotFound):
		h.fail(w, r, http.StatusNotFound, "", "no such override")
	case err != nil:
		h.internal(w, r, "remove override", err)
	default:
		by, _ := access.FromContext(r.Context())
		h.logger.InfoContext(r.Context(), "schedule override removed", slog.String("override_id", cancelled.ID), slog.String("schedule", cancelled.Schedule),
			slog.String("user", cancelled.User), slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// errorResponse is the body of a failed request.
type errorResponse struct {
	Error string `json:"error"`
	// Field names the part of the request at fault, when there is one.
	Field string `json:"field,omitempty"`
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, field, msg string) {
	h.write(w, r, status, errorResponse{Error: msg, Field: field})
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.logger.ErrorContext(r.Context(), what, slog.Any("error", err))
	h.fail(w, r, http.StatusInternalServerError, "", "internal error")
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
