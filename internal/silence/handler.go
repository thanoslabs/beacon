package silence

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// Path is the route of the silences.
const Path = "/api/v1/silences"

// maxRequestBytes bounds the body of a request for a silence. Twenty
// matchers of the greatest length, the comment and the JSON around them fit
// well within it.
const maxRequestBytes = 64 << 10

// Handler serves the silences API.
type Handler struct {
	logger *slog.Logger
	store  *Store
}

// NewHandler returns a handler that keeps silences in store.
func NewHandler(logger *slog.Logger, store *Store) *Handler {
	return &Handler{logger: logger, store: store}
}

// Routes are the routes of the silences. All are open to members: silencing
// is part of responding to alerts, like acknowledging one, and whoever is on
// call must be able to do it and to end a silence that someone else left
// behind. The author of every silence is recorded.
func (h *Handler) Routes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + Path, Level: access.Member, Handler: http.HandlerFunc(h.list)},
		{Pattern: "POST " + Path, Level: access.Member, Handler: http.HandlerFunc(h.create)},
		{Pattern: "GET " + Path + "/{id}", Level: access.Member, Handler: http.HandlerFunc(h.get)},
		{Pattern: "DELETE " + Path + "/{id}", Level: access.Member, Handler: http.HandlerFunc(h.expire)},
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	var expired bool
	switch r.URL.Query().Get("expired") {
	case "", "false":
	case "true":
		expired = true
	default:
		h.fail(w, r, http.StatusBadRequest, "expired", "must be true or false")
		return
	}
	silences, err := h.store.List(r.Context(), expired)
	if err != nil {
		h.internal(w, r, "list silences", err)
		return
	}
	if silences == nil {
		silences = []Silence{}
	}
	h.write(w, r, http.StatusOK, map[string][]Silence{"silences": silences})
}

// createRequest is the body of a request for a new silence.
type createRequest struct {
	Matchers []Matcher `json:"matchers"`
	// StartsAt and EndsAt are times in RFC 3339 form. Without StartsAt the
	// silence begins now.
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
	// Duration is how long the silence lasts, such as 2h, in place of
	// EndsAt.
	Duration string `json:"duration"`
	Comment  string `json:"comment"`
}

const createExample = `the body must be JSON such as {"matchers": [{"label": "alertname", "operator": "=", "value": "DiskFull"}], "duration": "2h", "comment": "replacing the disk"}`

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, r, http.StatusRequestEntityTooLarge, "", "the body is larger than 64 KiB")
			return
		}
		h.fail(w, r, http.StatusBadRequest, "", createExample+"; times are in RFC 3339 form")
		return
	}
	n := New{Matchers: req.Matchers, Comment: req.Comment}
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
		if err != nil || d <= 0 || time.Duration(d) > MaxDuration {
			h.fail(w, r, http.StatusBadRequest, "duration", "must be a duration such as 30m, 2h or 1d, at most a year")
			return
		}
		begin := n.StartsAt
		if now := h.store.now(); begin.Before(now) {
			begin = now
		}
		n.EndsAt = begin.Add(time.Duration(d))
	}

	by, _ := access.FromContext(r.Context())
	created, err := h.store.Create(r.Context(), n, by)
	var bad *Error
	if errors.As(err, &bad) {
		h.fail(w, r, http.StatusBadRequest, bad.Field, bad.Msg)
		return
	}
	if err != nil {
		h.internal(w, r, "create silence", err)
		return
	}
	h.logger.InfoContext(r.Context(), "silence created", slog.String("silence_id", created.ID), slog.Int("matchers", len(created.Matchers)),
		slog.Time("starts_at", created.StartsAt), slog.Time("ends_at", created.EndsAt), slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
	h.write(w, r, http.StatusCreated, created)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	found, err := h.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, "", "no such silence")
		return
	}
	if err != nil {
		h.internal(w, r, "read silence", err)
		return
	}
	h.write(w, r, http.StatusOK, found)
}

func (h *Handler) expire(w http.ResponseWriter, r *http.Request) {
	expired, err := h.store.Expire(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, "", "no such silence")
		return
	}
	if err != nil {
		h.internal(w, r, "expire silence", err)
		return
	}
	by, _ := access.FromContext(r.Context())
	h.logger.InfoContext(r.Context(), "silence expired", slog.String("silence_id", expired.ID),
		slog.Time("ends_at", expired.EndsAt), slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
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
