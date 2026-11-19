package e2e_test

import (
	"net/http"
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
