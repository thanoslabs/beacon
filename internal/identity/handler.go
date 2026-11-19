package identity

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/thanoslabs/beacon/internal/access"
)

// Path is the route of the signed-in user's identities.
const Path = "/api/v1/me/identities"

// Handler serves the identities API.
type Handler struct {
	logger *slog.Logger
	store  *Store
}

// NewHandler returns a handler that reads through store.
func NewHandler(logger *slog.Logger, store *Store) *Handler {
	return &Handler{logger: logger, store: store}
}

// Routes are the routes of the identities. A person sees their own.
func (h *Handler) Routes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + Path, Level: access.Member, Handler: http.HandlerFunc(h.list)},
	}
}

// list answers the chat accounts of whoever asks. An API token is not a
// person and has none.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	identities := []Identity{}
	if p, _ := access.FromContext(r.Context()); p.UserID != "" {
		found, err := h.store.Identities(r.Context(), p.UserID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "list identities", slog.Any("error", err))
			h.write(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		identities = append(identities, found...)
	}
	h.write(w, r, http.StatusOK, map[string][]Identity{"identities": identities})
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
