// Package ingest receives alerts from sources over HTTP and hands them to the
// alert model. docs/webhook.md describes the endpoint and the payload.
package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
)

// Path is the route of the ingest endpoint. The last segment is the name of
// the source.
const Path = "/api/v1/ingest/{source}"

// maxBodyBytes bounds a request body.
const maxBodyBytes = 1 << 20

// Source is a configured source and the identifier of its row in Postgres.
type Source struct {
	ID string
	config.Source
}

// Alerts is what the handler needs from the alert store.
type Alerts interface {
	Fire(ctx context.Context, f alert.Firing) (a alert.Alert, created bool, err error)
	ResolveFromSource(ctx context.Context, sourceID, fingerprint string, resolvedAt time.Time) (a alert.Alert, found bool, err error)
}

// Handler serves the ingest endpoint.
type Handler struct {
	logger *slog.Logger
	alerts Alerts
	// sources is replaced as a whole when the configuration is reloaded, so
	// a request sees either the old set or the new one.
	sources atomic.Pointer[map[string]Source]
}

// New returns a handler for the given sources.
func New(logger *slog.Logger, alerts Alerts, sources []Source) *Handler {
	h := &Handler{logger: logger, alerts: alerts}
	h.SetSources(sources)
	return h
}

// SetSources replaces the sources that the handler serves. It is safe while
// requests are being handled.
func (h *Handler) SetSources(sources []Source) {
	byName := make(map[string]Source, len(sources))
	for _, s := range sources {
		byName[s.Name] = s
	}
	h.sources.Store(&byName)
}

// Register attaches the endpoint to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST "+Path, h)
}

// response is the body of a successful request.
type response struct {
	// Result is created, updated, resolved or not_found.
	Result  string `json:"result"`
	AlertID string `json:"alert_id,omitempty"`
	Status  string `json:"status,omitempty"`
}

// errorResponse is the body of a failed request.
type errorResponse struct {
	Error string `json:"error"`
	// Field names the part of the payload at fault, when there is one.
	Field string `json:"field,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	src, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="beacon"`)
		h.write(w, r, http.StatusUnauthorized, errorResponse{Error: "unknown source or wrong token; send the source's token as a bearer token"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.write(w, r, http.StatusRequestEntityTooLarge, errorResponse{Error: fmt.Sprintf("the body is larger than %d bytes", maxBodyBytes)})
			return
		}
		h.write(w, r, http.StatusBadRequest, errorResponse{Error: "could not read the body"})
		return
	}
	p, err := parsePayload(body)
	if err != nil {
		var fe *fieldError
		errors.As(err, &fe)
		h.write(w, r, http.StatusBadRequest, errorResponse{Error: fe.msg, Field: fe.field})
		return
	}

	res, status, err := h.apply(r.Context(), src, p)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "store alert", slog.String("source", src.Name), slog.Any("error", err))
		h.write(w, r, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}
	h.logger.InfoContext(r.Context(), "alert received",
		slog.String("source", src.Name), slog.String("result", res.Result), slog.String("alert_id", res.AlertID))
	h.write(w, r, status, res)
}

// authenticate returns the source named in the path when the request carries
// its token. An unknown source and a wrong token are not told apart.
func (h *Handler) authenticate(r *http.Request) (Source, bool) {
	src, known := (*h.sources.Load())[r.PathValue("source")]
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	// Hashing first makes the comparison take the same time whatever the
	// lengths, and it runs for unknown sources too.
	want := sha256.Sum256([]byte(src.Token.Reveal()))
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	match := subtle.ConstantTimeCompare(want[:], got[:]) == 1
	return src, known && src.Token.IsSet() && strings.EqualFold(scheme, "Bearer") && match
}

// apply hands the payload to the alert model.
func (h *Handler) apply(ctx context.Context, src Source, p payload) (response, int, error) {
	fingerprint := p.Fingerprint
	if fingerprint == "" {
		fingerprint = Fingerprint(p.Labels)
	}

	if p.Status == statusResolved {
		a, found, err := h.alerts.ResolveFromSource(ctx, src.ID, fingerprint, p.EndsAt)
		if err != nil {
			return response{}, 0, err
		}
		if !found {
			return response{Result: "not_found"}, http.StatusOK, nil
		}
		return response{Result: "resolved", AlertID: a.ID, Status: string(a.Status)}, http.StatusOK, nil
	}

	labels := make(map[string]string, len(p.Labels)+len(src.Labels))
	for k, v := range src.Labels {
		labels[k] = v
	}
	for k, v := range p.Labels {
		labels[k] = v
	}
	a, created, err := h.alerts.Fire(ctx, alert.Firing{
		SourceID:    src.ID,
		Fingerprint: fingerprint,
		Severity:    src.Severity.Resolve(labels),
		Labels:      labels,
		Annotations: p.Annotations,
		StartsAt:    p.StartsAt,
	})
	if err != nil {
		return response{}, 0, err
	}
	if created {
		return response{Result: "created", AlertID: a.ID, Status: string(a.Status)}, http.StatusCreated, nil
	}
	return response{Result: "updated", AlertID: a.ID, Status: string(a.Status)}, http.StatusOK, nil
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}

// Fingerprint identifies an alert by its labels: the same labels give the
// same fingerprint, in whatever order they were sent.
func Fingerprint(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		// Lengths keep different label sets from producing the same bytes.
		fmt.Fprintf(sum, "%d:%s%d:%s", len(name), name, len(labels[name]), labels[name])
	}
	return hex.EncodeToString(sum.Sum(nil))
}
