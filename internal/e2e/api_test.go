package e2e_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAlertsAPI follows an alert through the API of the real binary: it is
// listed, acknowledged by a token and resolved by hand by a person, and what
// they do reaches the timeline and the message in Slack like an action from
// chat.
func TestAlertsAPI(t *testing.T) {
	b, admin, bare, memberToken := signedIn(t)

	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	b.message("the alert in Slack", contains("*Firing*"))
	b.timeline(alertID, 3)

	// Nobody without a credential, and the token of a source is not one.
	for _, token := range []string{"", sourceToken, "bcn_never-issued"} {
		for _, probe := range [][2]string{{"GET", "/api/v1/alerts"}, {"GET", "/api/v1/alerts/" + alertID}, {"POST", "/api/v1/alerts/" + alertID + "/resolve"}} {
			if code, body := b.api(bare, probe[0], probe[1], token, ""); code != http.StatusUnauthorized {
				t.Errorf("%s %s with the token %q = %d %v, want 401", probe[0], probe[1], token, code, body)
			}
		}
	}

	// The list, as a member with a token.
	code, list := b.api(bare, "GET", "/api/v1/alerts?status=open&severity=critical", memberToken, "")
	alerts, _ := list["alerts"].([]any)
	if code != http.StatusOK || len(alerts) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}
	listed := alerts[0].(map[string]any)
	if listed["id"] != alertID || listed["source"] != "e2e" || listed["status"] != "firing" || listed["severity"] != "critical" ||
		listed["labels"].(map[string]any)["host"] != "db1" || listed["annotations"].(map[string]any)["summary"] != "Disk is full on db1" {
		t.Errorf("listed alert = %v", listed)
	}
	if code, list := b.api(bare, "GET", "/api/v1/alerts?status=resolved", memberToken, ""); code != http.StatusOK || len(list["alerts"].([]any)) != 0 {
		t.Errorf("resolved alerts = %d %v, want none", code, list)
	}

	// The token acknowledges. The message in Slack says who.
	code, acked := b.api(bare, "POST", "/api/v1/alerts/"+alertID+"/acknowledge", memberToken, "")
	if code != http.StatusOK || acked["status"] != "acknowledged" {
		t.Fatalf("acknowledge = %d %v", code, acked)
	}
	b.message("the acknowledgement in Slack", contains("*Acknowledged*", "Acknowledged by responder"))
	if code, body := b.api(bare, "POST", "/api/v1/alerts/"+alertID+"/acknowledge", memberToken, ""); code != http.StatusConflict {
		t.Errorf("second acknowledge = %d %v, want 409", code, body)
	}
	b.timeline(alertID, 5)

	// A person resolves by hand, through the group, as the web interface
	// will.
	groupID, _ := acked["group_id"].(string)
	code, group := b.api(admin, "POST", "/api/v1/alert-groups/"+groupID+"/resolve", "", "")
	if code != http.StatusOK || group["status"] != "resolved" {
		t.Fatalf("resolve the group = %d %v", code, group)
	}
	final := b.message("the resolution in Slack", contains("*Resolved*", "Resolved by Bootstrap admin"))
	if strings.Contains(final.JSON(), `"actions"`) {
		t.Errorf("the resolved message has buttons: %s", final.JSON())
	}

	want := []string{
		"fired by source",
		"escalated by system",
		"notification_sent by system",
		"acknowledged by api_token",
		"notification_sent by system",
		"resolved by user",
		"notification_sent by system",
	}
	if got := b.timeline(alertID, len(want)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// The same story through the API.
	code, timeline := b.api(bare, "GET", "/api/v1/alerts/"+alertID+"/timeline", memberToken, "")
	events, _ := timeline["events"].([]any)
	if code != http.StatusOK || len(events) != len(want) {
		t.Fatalf("timeline = %d %v", code, timeline)
	}
	if ack := events[3].(map[string]any); ack["type"] != "acknowledged" || ack["actor"].(map[string]any)["name"] != "responder" || ack["actor"].(map[string]any)["type"] != "api_token" {
		t.Errorf("the acknowledgement on the timeline = %v", ack)
	}
}

// TestConfigurationThroughTheAPI reads what the files define through the
// API of the real binary, checks that the answers follow a reload, and that
// no method changes any of it.
func TestConfigurationThroughTheAPI(t *testing.T) {
	b, admin, bare, memberToken := signedIn(t)
	names := func(path, key string) string {
		t.Helper()
		code, body := b.api(bare, "GET", path, memberToken, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %v", path, code, body)
		}
		var out []string
		for _, item := range body[key].([]any) {
			out = append(out, item.(map[string]any)["name"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := names("/api/v1/sources", "sources") + " " + names("/api/v1/integrations", "integrations") + " " + names("/api/v1/escalation-policies", "escalation_policies"); got != "e2e slack default" {
		t.Errorf("sources, integrations and policies = %q", got)
	}
	code, routing := b.api(bare, "GET", "/api/v1/routing", memberToken, "")
	if code != http.StatusOK || routing["group_wait"] != "0s" || routing["group_interval"] != "1s" || routing["default"].(map[string]any)["escalation_policy"] != "default" {
		t.Errorf("routing = %d %v", code, routing)
	}
	code, policy := b.api(bare, "GET", "/api/v1/escalation-policies/default", memberToken, "")
	if target := policy["steps"].([]any)[0].(map[string]any)["notify"].([]any)[0].(map[string]any); code != http.StatusOK || target["kind"] != "channel" || target["channel"] != "C0ALERTS" {
		t.Errorf("policy = %d %v", code, policy)
	}
	code, users := b.api(bare, "GET", "/api/v1/users", memberToken, "")
	if list := users["users"].([]any); code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["email"] != "bootstrap-admin@beacon.invalid" || list[0].(map[string]any)["role"] != "admin" {
		t.Errorf("users = %d %v", code, users)
	}
	if code, methods := b.api(bare, "GET", "/auth/methods", "", ""); code != http.StatusOK || methods["oidc"] != false || methods["bootstrap_admin"] != true {
		t.Errorf("sign-in methods = %d %v", code, methods)
	}
	// No credential and no secret reference in any answer.
	for _, secret := range []string{botToken, signingSecret, sourceToken, "BEACON_SLACK_BOT_TOKEN"} {
		for _, path := range []string{"/api/v1/integrations", "/api/v1/sources"} {
			req, _ := http.NewRequest("GET", b.url+path, nil)
			req.Header.Set("Authorization", "Bearer "+memberToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(raw), secret) {
				t.Errorf("GET %s contains %q: %s", path, secret, raw)
			}
		}
	}

	// A reload changes what is answered.
	second := strings.Replace(bootstrapConfiguration, "sources:\n", "sources:\n  - name: second\n    type: grafana\n    token: {env: BEACON_SOURCE_TOKEN}\n", 1)
	if err := os.WriteFile(filepath.Join(b.dir, "beacon.yaml"), []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := b.api(admin, "POST", "/api/v1/config/reload", "", ""); code != http.StatusOK {
		t.Fatalf("reload = %d %v", code, body)
	}
	if got := names("/api/v1/sources", "sources"); got != "second,e2e" {
		t.Errorf("sources after the reload = %q", got)
	}

	// What the files define cannot be changed here, by an admin either:
	// there is no such route.
	for _, attempt := range [][2]string{
		{"POST", "/api/v1/sources"}, {"PUT", "/api/v1/sources/e2e"}, {"DELETE", "/api/v1/sources/e2e"}, {"PATCH", "/api/v1/integrations/slack"},
		{"DELETE", "/api/v1/escalation-policies/default"}, {"PUT", "/api/v1/routing"}, {"POST", "/api/v1/teams"}, {"DELETE", "/api/v1/users/x"},
	} {
		if code, body := b.api(admin, attempt[0], attempt[1], "", `{"name": "x"}`); code != http.StatusNotFound {
			t.Errorf("%s %s as an admin = %d %v, want no such route", attempt[0], attempt[1], code, body)
		}
	}
	if got := names("/api/v1/sources", "sources"); got != "second,e2e" {
		t.Errorf("sources after the attempts = %q", got)
	}
}
