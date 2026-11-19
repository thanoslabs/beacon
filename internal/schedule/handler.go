package schedule

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/config"
)

// Path is the route of the schedules.
const Path = "/api/v1/schedules"

// TeamOnCallPath is the route that says who is on call for a team.
const TeamOnCallPath = "/api/v1/teams/{name}/on-call"

// DefaultTimelineRange is the period that the shifts route covers when the
// request does not say.
const DefaultTimelineRange = 28 * 24 * time.Hour

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

// Routes are the routes of the schedules. All are open to members: who is
// on call is what every responder needs to know. A schedule itself is
// provisioned and can only be read here; an override is runtime state, and covering for a colleague is part of being on call, so
// members create and remove overrides, their own and other people's. The
// author of every override is recorded.
func (h *Handler) Routes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + Path, Level: access.Member, Handler: http.HandlerFunc(h.list)},
		{Pattern: "GET " + Path + "/{name}", Level: access.Member, Handler: http.HandlerFunc(h.get)},
		{Pattern: "GET " + Path + "/{name}/shifts", Level: access.Member, Handler: http.HandlerFunc(h.shifts)},
		{Pattern: "GET " + TeamOnCallPath, Level: access.Member, Handler: http.HandlerFunc(h.teamOnCall)},
		{Pattern: "GET " + Path + "/{name}/overrides", Level: access.Member, Handler: http.HandlerFunc(h.listOverrides)},
		{Pattern: "POST " + Path + "/{name}/overrides", Level: access.Member, Handler: http.HandlerFunc(h.createOverride)},
		{Pattern: "DELETE " + Path + "/{name}/overrides/{id}", Level: access.Member, Handler: http.HandlerFunc(h.cancelOverride)},
	}
}

// onCallBody is who is on call, as the API answers.
type onCallBody struct {
	Schedule string `json:"schedule"`
	// User is the email address of the person on call, or null when nobody
	// is.
	User *string `json:"user"`
	// Since and Until bound the time for which that is known to hold: the
	// shift, or the override.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Override is the override in force, when there is one, and
	// RotationUser whom it covers.
	Override     *Override `json:"override,omitempty"`
	RotationUser *string   `json:"rotation_user"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func onCallOf(o OnCall) onCallBody {
	return onCallBody{Schedule: o.Schedule, User: optional(o.Email), Since: o.Since.UTC(), Until: o.Until.UTC(),
		Override: o.Override, RotationUser: optional(o.Rotation)}
}

// rotationBody is a rotation, as the API answers.
type rotationBody struct {
	TimeZone    string   `json:"time_zone"`
	Members     []string `json:"members"`
	ShiftLength string   `json:"shift_length"`
	HandoffTime string   `json:"handoff_time"`
	HandoffDay  string   `json:"handoff_day,omitempty"`
}

func rotationOf(d Definition) rotationBody {
	return rotationBody{TimeZone: d.TimeZone, Members: d.Members, HandoffTime: d.HandoffTime, HandoffDay: d.HandoffDay,
		ShiftLength: config.Duration(time.Duration(d.ShiftDays) * config.Day).String()}
}

// scheduleBody is a schedule, as the API answers.
type scheduleBody struct {
	Name string `json:"name"`
	Team string `json:"team"`
	// Rotation is the rotation in force, which is the one in the files
	// unless a change is pending.
	Rotation    rotationBody `json:"rotation"`
	NextHandoff time.Time    `json:"next_handoff"`
	// Pending is a change of definition that has not taken over yet.
	Pending *pendingBody `json:"pending,omitempty"`
	OnCall  onCallBody   `json:"on_call"`
}

type pendingBody struct {
	Rotation    rotationBody `json:"rotation"`
	TakesOverAt time.Time    `json:"takes_over_at"`
	// Stated says that the files state the time; otherwise it is the next
	// handoff.
	Stated bool `json:"stated"`
}

func scheduleOf(i Info) scheduleBody {
	state := i.Position.state()
	out := scheduleBody{Name: i.Name, Team: i.Team, Rotation: rotationOf(i.Position.Definition), NextHandoff: i.Position.NextHandoffAt.UTC(), OnCall: onCallOf(i.OnCall)}
	if state.Pending != nil {
		out.Pending = &pendingBody{Rotation: rotationOf(*state.Pending), TakesOverAt: state.takeover().UTC(), Stated: state.PendingAt != nil}
	}
	return out
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	infos, err := h.store.List(r.Context())
	if err != nil {
		h.internal(w, r, "list schedules", err)
		return
	}
	out := make([]scheduleBody, 0, len(infos))
	for _, i := range infos {
		out = append(out, scheduleOf(i))
	}
	h.write(w, r, http.StatusOK, map[string][]scheduleBody{"schedules": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	info, err := h.store.Get(r.Context(), r.PathValue("name"))
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
		return
	}
	if err != nil {
		h.internal(w, r, "read schedule", err)
		return
	}
	h.write(w, r, http.StatusOK, scheduleOf(info))
}

func (h *Handler) teamOnCall(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("name")
	onCall, err := h.store.TeamOnCall(r.Context(), team)
	if errors.Is(err, ErrTeamNotFound) {
		h.fail(w, r, http.StatusNotFound, "", "no such team")
		return
	}
	if err != nil {
		h.internal(w, r, "read who is on call for the team", err)
		return
	}
	out := make([]onCallBody, 0, len(onCall))
	for _, o := range onCall {
		out = append(out, onCallOf(o))
	}
	h.write(w, r, http.StatusOK, map[string]any{"team": team, "on_call": out})
}

// shiftBody is one shift of a timeline, as the API answers.
type shiftBody struct {
	User       string    `json:"user"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	Kind       string    `json:"kind"`
	OverrideID string    `json:"override_id,omitempty"`
	Planned    bool      `json:"planned"`
}

func (h *Handler) shifts(w http.ResponseWriter, r *http.Request) {
	from := h.store.now()
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, "from", "must be a time in RFC 3339 form, such as 2026-12-01T00:00:00Z")
			return
		}
		from = t
	}
	to := from.Add(DefaultTimelineRange)
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, "to", "must be a time in RFC 3339 form, such as 2026-12-29T00:00:00Z")
			return
		}
		to = t
	}
	name := r.PathValue("name")
	timeline, err := h.store.Timeline(r.Context(), name, from, to)
	var bad *Error
	switch {
	case errors.Is(err, ErrNotFound):
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
		return
	case errors.As(err, &bad):
		h.fail(w, r, http.StatusBadRequest, bad.Field, bad.Msg)
		return
	case err != nil:
		h.internal(w, r, "read shifts", err)
		return
	}
	out := make([]shiftBody, 0, len(timeline))
	for _, s := range timeline {
		out = append(out, shiftBody{User: s.User, StartsAt: s.StartsAt, EndsAt: s.EndsAt, Kind: s.Kind, OverrideID: s.OverrideID, Planned: s.Planned})
	}
	h.write(w, r, http.StatusOK, map[string]any{"schedule": name, "from": from.UTC(), "to": to.UTC(), "shifts": out})
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
	page, next, field, problem, ok := api.Slice(overrides, r.URL.Query())
	if !ok {
		h.fail(w, r, http.StatusBadRequest, field, problem)
		return
	}
	h.write(w, r, http.StatusOK, struct {
		Overrides  []Override `json:"overrides"`
		NextCursor string     `json:"next_cursor,omitempty"`
	}{page, next})
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
	var bad *Error
	switch {
	case errors.Is(err, ErrNotFound):
		h.fail(w, r, http.StatusNotFound, "", "no such schedule")
	case errors.As(err, &bad):
		h.fail(w, r, http.StatusBadRequest, bad.Field, bad.Msg)
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
