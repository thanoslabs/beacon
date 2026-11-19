package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/thanoslabs/beacon/internal/access"
)

// The routes of the OpenAPI description.
const (
	OpenAPIJSONPath = "/api/v1/openapi.json"
	OpenAPIYAMLPath = "/api/v1/openapi.yaml"
)

// openAPIYAML is the description as it is written, in openapi.yaml.
//
//go:embed openapi.yaml
var openAPIYAML []byte

// OpenAPIYAML returns the OpenAPI description of the API as YAML.
func OpenAPIYAML() []byte { return openAPIYAML }

var openAPIJSON = sync.OnceValues(func() ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
		return nil, fmt.Errorf("the OpenAPI description is not YAML: %w", err)
	}
	return json.MarshalIndent(doc, "", "  ")
})

// OpenAPIJSON returns the same description as JSON.
func OpenAPIJSON() ([]byte, error) { return openAPIJSON() }

// OpenAPIRoutes are the routes that serve the description. They are public:
// the description says what is in the repository of an open-source project,
// about no install in particular, and a client that is about to sign in is
// the one that needs it.
func OpenAPIRoutes() []access.Route {
	serve := func(contentType string, body func() ([]byte, error)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			raw, err := body()
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", contentType)
			// The description changes only with the binary.
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(raw)
		})
	}
	return []access.Route{
		{Pattern: "GET " + OpenAPIJSONPath, Level: access.Public, Handler: serve("application/json", OpenAPIJSON)},
		{Pattern: "GET " + OpenAPIYAMLPath, Level: access.Public, Handler: serve("application/yaml", func() ([]byte, error) { return openAPIYAML, nil })},
	}
}
