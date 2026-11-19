package identity

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
)

// The routes of the signed-in user's identities.
const (
	// Path lists them.
	Path = "/api/v1/me/identities"
	// IntegrationPath is the identity of the user with one integration.
	IntegrationPath = Path + "/{integration}"
	// LinkCodePath issues a code with which the user links their account in
	// the chat service of an integration.
	LinkCodePath = IntegrationPath + "/link-code"
)

// Handler serves the identities API.
type Handler struct {
	logger *slog.Logger
	store  *Store
	// integrations returns the integrations through which people have
	// accounts, by name, as the configuration has them now.
	integrations func() map[string]Integration
}

// NewHandler returns a handler that reads and writes through store.
// integrations tells it which integrations exist; with nil there are none.
func NewHandler(logger *slog.Logger, store *Store, integrations func() map[string]Integration) *Handler {
	if integrations == nil {
		integrations = func() map[string]Integration { return nil }
	}
	return &Handler{logger: logger, store: store, integrations: integrations}
}

// Routes are the routes of the identities. A person sees and manages their
// own, and nobody else's.
func (h *Handler) Routes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + Path, Level: access.Member, Handler: http.HandlerFunc(h.list)},
		{Pattern: "POST " + LinkCodePath, Level: access.Member, Handler: http.HandlerFunc(h.linkCode)},
		{Pattern: "DELETE " + IntegrationPath, Level: access.Member, Handler: http.HandlerFunc(h.unlink)},
	}
}

// list answers the chat accounts of whoever asks. An API token is not a
// person and has none.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	identities := []Identity{}
	if p, _ := access.FromContext(r.Context()); p.UserID != "" {
		found, err := h.store.Identities(r.Context(), p.UserID)
		if err != nil {
			h.internal(w, r, "list identities", err)
			return
		}
		identities = append(identities, found...)
	}
	h.write(w, r, http.StatusOK, map[string][]Identity{"identities": identities})
}

// linkCodeBody is the answer to a request for a link code.
type linkCodeBody struct {
	Integration string `json:"integration"`
	Provider    string `json:"provider"`
	// Code is shown this once: only its hash is stored.
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	// Command is what the person types in the chat service to use the code.
	Command string `json:"command"`
}

// selfLinked returns the integration with the name in the path, when it is
// one in which people link their accounts themselves, and the person who
// asks. It answers the request when there is no such integration or nobody
// to link.
func (h *Handler) selfLinked(w http.ResponseWriter, r *http.Request) (Integration, string, bool) {
	p, _ := access.FromContext(r.Context())
	in, known := h.integrations()[r.PathValue("integration")]
	switch {
	case !known:
		h.fail(w, r, http.StatusNotFound, "integration", "there is no chat integration with this name")
	case in.Lookup != nil:
		// Beacon would only find the account again by the address.
		h.fail(w, r, http.StatusConflict, "integration", "accounts of this integration are linked by email address, not by the person")
	case p.UserID == "":
		h.fail(w, r, http.StatusForbidden, "", "an API token is not a person and has no chat account; sign in as the person")
	default:
		return in, p.UserID, true
	}
	return Integration{}, "", false
}

// linkCode gives whoever asks a one-time code with which they prove, from
// inside the chat service, that an account there is theirs.
func (h *Handler) linkCode(w http.ResponseWriter, r *http.Request) {
	in, userID, ok := h.selfLinked(w, r)
	if !ok {
		return
	}
	// The bootstrap admin is an account, not a person: it is never paged
	// and has no address.
	person, err := h.store.IsPerson(r.Context(), userID)
	if err != nil {
		h.internal(w, r, "read user", err)
		return
	}
	if !person {
		h.fail(w, r, http.StatusForbidden, "", "this account is not a person who signed in through the identity provider, and cannot link a chat account")
		return
	}
	code, expires, err := h.store.StartLink(r.Context(), userID, in.ID)
	if err != nil {
		h.internal(w, r, "start link", err)
		return
	}
	// The code itself is never logged.
	h.logger.InfoContext(r.Context(), "link code issued", slog.String("user_id", userID), slog.String("integration", in.Name))
	shown := FormatCode(code)
	h.write(w, r, http.StatusCreated, linkCodeBody{Integration: in.Name, Provider: in.Provider, Code: shown, ExpiresAt: expires, Command: in.LinkCommand + " " + shown})
}

// unlink removes the account that whoever asks linked themselves.
func (h *Handler) unlink(w http.ResponseWriter, r *http.Request) {
	in, userID, ok := h.selfLinked(w, r)
	if !ok {
		return
	}
	found, err := h.store.Unlink(r.Context(), userID, in.ID)
	if err != nil {
		h.internal(w, r, "unlink identity", err)
		return
	}
	if !found {
		h.fail(w, r, http.StatusNotFound, "integration", "you have no linked account with this integration")
		return
	}
	h.logger.InfoContext(r.Context(), "chat account unlinked by the user", slog.String("user_id", userID), slog.String("integration", in.Name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// IsPerson reports whether a user is a person who signed in through the
// identity provider, and is enabled: someone who can have a chat account.
func (s *Store) IsPerson(ctx context.Context, userID string) (bool, error) {
	var person bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = $1 AND `+people+`)`, userID).Scan(&person)
	return person, err
}

// FormatCode writes a link code in groups of four, which is easier to read
// and to type. A code is accepted with or without the hyphens.
func FormatCode(code string) string {
	var out []byte
	for i := 0; i < len(code); i++ {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, code[i])
	}
	return string(out)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, field, msg string) {
	body := map[string]string{"error": msg}
	if field != "" {
		body["field"] = field
	}
	h.write(w, r, status, body)
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.logger.ErrorContext(r.Context(), what, slog.Any("error", err))
	h.write(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
