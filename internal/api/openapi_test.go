package api_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/api/apitest"
)

var update = flag.Bool("update", false, "rewrite the published copies of the OpenAPI description")

// reference writes the page of the documentation that lists every route,
// from the description.
func reference(spec *apitest.Spec) string {
	var b strings.Builder
	b.WriteString(`---
title: API reference
tags: [api, reference]
date: 2026-11-19
---

Every route of Beacon's HTTP API, with who may call it. This page is generated from the OpenAPI description of the API, which a running Beacon serves at ` + "`/api/v1/openapi.json` and `/api/v1/openapi.yaml`" + ` and which is published beside this page as [openapi.yaml](openapi.yaml). The description holds the parameters and the shape of every answer; [[api]] describes how the API behaves as a whole.

Do not edit this page. Change ` + "`internal/api/openapi.yaml`, then run `go test ./internal/api -run TestPublishedDescription -update`" + `.

"Public" needs no credential, "member" a session or an API token with either role, and "admin" the admin role.
`)
	titles := map[string]string{"alerts": "Alerts", "silences": "Silences", "schedules": "Schedules", "configuration": "Configuration", "access": "Access", "machines": "For other systems"}
	for _, tag := range []string{"alerts", "silences", "schedules", "configuration", "access", "machines"} {
		fmt.Fprintf(&b, "\n## %s\n\n| Route | Who | What it does |\n|---|---|---|\n", titles[tag])
		for _, op := range spec.Operations() {
			if op.Tag == tag {
				path := strings.NewReplacer("{", "<", "}", ">").Replace(op.Path)
				fmt.Fprintf(&b, "| `%s %s` | %s | %s |\n", op.Method, path, strings.ToUpper(op.Access[:1])+op.Access[1:], op.Summary)
			}
		}
	}
	return b.String()
}

// TestPublishedDescription keeps the copies of the description that are
// published with the documentation equal to the one in the binary: the YAML
// itself, and the page that lists the routes.
func TestPublishedDescription(t *testing.T) {
	spec := apitest.Load(t)
	for _, op := range spec.Operations() {
		if op.Tag == "" || op.Summary == "" || (op.Access != "public" && op.Access != "member" && op.Access != "admin") {
			t.Errorf("%s: tag %q, access %q, summary %q", op.Key(), op.Tag, op.Access, op.Summary)
		}
	}
	for path, want := range map[string][]byte{"../../docs/openapi.yaml": api.OpenAPIYAML(), "../../docs/api-reference.md": []byte(reference(spec))} {
		if *update {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is not what the description in the binary makes it (%v); run go test ./internal/api -run TestPublishedDescription -update", path, err)
		}
	}
	// The JSON form is the same document.
	raw, err := api.OpenAPIJSON()
	var doc map[string]any
	if err != nil || json.Unmarshal(raw, &doc) != nil || doc["openapi"] != "3.0.3" {
		t.Errorf("the JSON form of the description: %v", err)
	}
}
