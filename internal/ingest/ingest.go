// Package ingest receives alerts from sources over HTTP and hands them to the
// alert model. A source's type decides which payload its endpoint reads:
// docs/webhook.md describes the generic one, docs/alertmanager.md
// Alertmanager's and docs/grafana.md Grafana's.
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

// maxBodyBytes bounds the body of a request to a webhook source, which holds
// one alert. maxGroupBodyBytes bounds a notification about a group of alerts.
const (
	maxBodyBytes      = 1 << 20
	maxGroupBodyBytes = 4 << 20
)

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
	// grouping says which group a new alert joins. It follows the
	// configuration like the sources do.
	grouping atomic.Pointer[config.Grouping]
}

// New returns a handler for the given sources.
func New(logger *slog.Logger, alerts Alerts, sources []Source) *Handler {
	h := &Handler{logger: logger, alerts: alerts}
	h.SetSources(sources)
	h.SetGrouping((*config.Routing)(nil).Grouping())
	return h
}

// SetGrouping replaces the grouping of new alerts. It is safe while requests
// are being handled. Alerts that are open stay in the group they have.
func (h *Handler) SetGrouping(g config.Grouping) {
	h.grouping.Store(&g)
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

// response is the body of a successful request to a webhook source.
type response struct {
	// Result is created, updated, resolved or not_found.
	Result  string `json:"result"`
	AlertID string `json:"alert_id,omitempty"`
	Status  string `json:"status,omitempty"`
}

// The values of response.Result.
const (
	resultCreated  = "created"
	resultUpdated  = "updated"
	resultResolved = "resolved"
	resultNotFound = "not_found"
)

// groupResponse is the body of a successful request to a source that sends
// several alerts at once: how many it held and what became of them.
type groupResponse struct {
	// Result is always accepted.
	Result   string `json:"result"`
	Received int    `json:"received"`
	Created  int    `json:"created"`
	Updated  int    `json:"updated"`
	Resolved int    `json:"resolved"`
	NotFound int    `json:"not_found"`
}

// errorResponse is the body of a failed request.
type errorResponse struct {
	Error string `json:"error"`
	// Field names the part of the payload at fault, when there is one.
	Field string `json:"field,omitempty"`
}

// delivery is one alert as a source reports it, whatever payload it came in.
type delivery struct {
	// Resolved is true when the source says the alert is over.
	Resolved bool
	// Fingerprint is the identity the source gave the alert. When it is
	// empty the identity is computed from the labels.
	Fingerprint string
	Labels      map[string]string
	Annotations map[string]string
	Links       map[string]string
	// StartsAt and EndsAt are zero when the source did not say.
	StartsAt time.Time
	EndsAt   time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	src, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="beacon"`)
		h.write(w, r, http.StatusUnauthorized, errorResponse{Error: "unknown source or wrong token; send the source's token as a bearer token"})
		return
	}

	limit := int64(maxBodyBytes)
	if src.Type != config.SourceTypeWebhook {
		limit = maxGroupBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.write(w, r, http.StatusRequestEntityTooLarge, errorResponse{Error: fmt.Sprintf("the body is larger than %d bytes", limit)})
			return
		}
		h.write(w, r, http.StatusBadRequest, errorResponse{Error: "could not read the body"})
		return
	}

	switch src.Type {
	case config.SourceTypeAlertmanager:
		g, err := parseAlertmanager(body)
		h.serveGroup(w, r, src, g, err, AlertmanagerVersion)
	case config.SourceTypeGrafana:
		g, err := parseGrafana(body)
		h.serveGroup(w, r, src, g, err, GrafanaVersion)
	default:
		p, err := parsePayload(body)
		h.serveOne(w, r, src, p, err)
	}
}

// serveOne handles the generic webhook: one alert per request.
func (h *Handler) serveOne(w http.ResponseWriter, r *http.Request, src Source, p payload, err error) {
	if err != nil {
		h.invalid(w, r, err)
		return
	}
	d := delivery{
		Resolved: p.Status == statusResolved, Fingerprint: p.Fingerprint,
		Labels: p.Labels, Annotations: p.Annotations, StartsAt: p.StartsAt, EndsAt: p.EndsAt,
	}
	res, err := h.apply(r.Context(), src, d)
	if err != nil {
		h.failed(w, r, src, err)
		return
	}
	h.logger.InfoContext(r.Context(), "alert received",
		slog.String("source", src.Name), slog.String("result", res.Result), slog.String("alert_id", res.AlertID))
	status := http.StatusOK
	if res.Result == resultCreated {
		status = http.StatusCreated
	}
	h.write(w, r, status, res)
}

// serveGroup handles a notification about a group of alerts. Each alert is
// stored on its own. If one cannot be stored the request fails as a whole
// and the sender delivers it again, which is safe: the alerts that were
// stored are recognised by their fingerprints.
func (h *Handler) serveGroup(w http.ResponseWriter, r *http.Request, src Source, g alertGroup, err error, wantVersion string) {
	if err != nil {
		h.invalid(w, r, err)
		return
	}
	ctx := r.Context()
	if g.Version != wantVersion {
		h.logger.WarnContext(ctx, "the payload has another version than the one Beacon reads; it is read all the same",
			slog.String("source", src.Name), slog.String("type", src.Type), slog.String("version", g.Version), slog.String("expected", wantVersion))
	}
	if g.Truncated > 0 {
		h.logger.WarnContext(ctx, "the sender left alerts out of the notification, so Beacon does not know about them; raise or remove its limit on alerts per notification",
			slog.String("source", src.Name), slog.Int("truncated", g.Truncated))
	}
	out := groupResponse{Result: "accepted", Received: len(g.Deliveries)}
	for _, d := range g.Deliveries {
		res, err := h.apply(ctx, src, d)
		if err != nil {
			h.failed(w, r, src, err)
			return
		}
		switch res.Result {
		case resultCreated:
			out.Created++
		case resultUpdated:
			out.Updated++
		case resultResolved:
			out.Resolved++
		case resultNotFound:
			out.NotFound++
		}
	}
	h.logger.InfoContext(ctx, "alerts received", slog.String("source", src.Name), slog.Int("received", out.Received),
		slog.Int("created", out.Created), slog.Int("updated", out.Updated), slog.Int("resolved", out.Resolved), slog.Int("not_found", out.NotFound))
	h.write(w, r, http.StatusOK, out)
}

func (h *Handler) invalid(w http.ResponseWriter, r *http.Request, err error) {
	var fe *fieldError
	errors.As(err, &fe)
	h.write(w, r, http.StatusBadRequest, errorResponse{Error: fe.msg, Field: fe.field})
}

func (h *Handler) failed(w http.ResponseWriter, r *http.Request, src Source, err error) {
	h.logger.ErrorContext(r.Context(), "store alert", slog.String("source", src.Name), slog.Any("error", err))
	h.write(w, r, http.StatusInternalServerError, errorResponse{Error: "internal error"})
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

// apply hands one alert to the alert model.
func (h *Handler) apply(ctx context.Context, src Source, d delivery) (response, error) {
	fingerprint := d.Fingerprint
	if fingerprint == "" {
		fingerprint = Fingerprint(d.Labels)
	}

	if d.Resolved {
		a, found, err := h.alerts.ResolveFromSource(ctx, src.ID, fingerprint, d.EndsAt)
		if err != nil {
			return response{}, err
		}
		if !found {
			return response{Result: resultNotFound}, nil
		}
		return response{Result: resultResolved, AlertID: a.ID, Status: string(a.Status)}, nil
	}

	labels := make(map[string]string, len(d.Labels)+len(src.Labels))
	for k, v := range src.Labels {
		labels[k] = v
	}
	for k, v := range d.Labels {
		labels[k] = v
	}
	// Groups are formed from the labels as they are stored, with the
	// source's own, and across sources: the same problem seen by two
	// sources is one group.
	grouping := *h.grouping.Load()
	key, groupLabels := alert.GroupKey("", grouping.By, labels)
	a, created, err := h.alerts.Fire(ctx, alert.Firing{
		SourceID:    src.ID,
		Fingerprint: fingerprint,
		Severity:    src.Severity.Resolve(labels),
		Labels:      labels,
		Annotations: d.Annotations,
		Links:       d.Links,
		StartsAt:    d.StartsAt,
		Group:       alert.Grouping{Key: key, Labels: groupLabels, Wait: grouping.Wait, Interval: grouping.Interval},
	})
	if err != nil {
		return response{}, err
	}
	if created {
		return response{Result: resultCreated, AlertID: a.ID, Status: string(a.Status)}, nil
	}
	return response{Result: resultUpdated, AlertID: a.ID, Status: string(a.Status)}, nil
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
