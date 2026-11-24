package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The test alert: always the same one, so that sending it twice refreshes
// it and --resolve ends it.
const (
	testAlertName        = "BeaconTestAlert"
	testAlertFingerprint = "beacon-test-alert"
	testAlertTimeout     = 15 * time.Second
)

// runTestAlert sends a test alert to a running Beacon the way a source
// does: through the ingest endpoint of a webhook source, with the source's
// token. What happens to it is what happens to a real alert, which is the
// point: it is routed, grouped, and paged through an escalation policy.
func runTestAlert(ctx context.Context, e env, args []string) error {
	var (
		address, public, source, token, policy, severity string
		resolve                                          bool
	)
	fs := newFlagSet(e, "test-alert", "Send a test alert to a running Beacon, to see a page arrive. "+
		"It goes in through a webhook source, like a real alert, and is paged by the escalation policy that routing gives it, or by the one named with --policy")
	stringSetting(fs, e, &address, "url", "BEACON_URL", "http://127.0.0.1:8080", "address of the running Beacon")
	stringSetting(fs, e, &public, "public-url", "BEACON_PUBLIC_URL", "", "address at which your browser reaches Beacon, for the link that is printed; the same as --url unless set")
	fs.StringVar(&source, "source", "default", "name of a source of type webhook in the configuration")
	secretSetting(fs, e, &token, "token", "BEACON_SOURCE_TOKEN", "token of that source")
	fs.StringVar(&policy, "policy", "", "name of the escalation policy to page through; without it, routing decides as for any alert")
	fs.StringVar(&severity, "severity", "critical", "severity label of the alert")
	fs.BoolVar(&resolve, "resolve", false, "resolve the test alert instead of firing it")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("--url must be an http or https address such as http://127.0.0.1:8080, got %q", address)
	}
	if token == "" {
		return errors.New("the token of the source is required: set --token or BEACON_SOURCE_TOKEN to the token the source has in the configuration")
	}
	if source == "" || strings.ContainsAny(source, "/?# ") {
		return fmt.Errorf("--source must be the name of a source, got %q", source)
	}

	// One test alert per policy, so that tests of two policies do not share
	// a group or resolve each other.
	fingerprint := testAlertFingerprint
	if policy != "" {
		fingerprint += "/" + policy
	}
	payload := map[string]any{"fingerprint": fingerprint}
	if resolve {
		payload["status"] = "resolved"
	} else {
		payload["labels"] = map[string]string{"alertname": testAlertName, "severity": severity}
		payload["annotations"] = map[string]string{
			"summary":     "Test alert from beacon test-alert",
			"description": "Somebody sent this alert by hand to check that pages arrive. Nothing is wrong. Resolve it, or run `beacon test-alert --resolve`.",
		}
		if policy != "" {
			payload["escalation_policy"] = policy
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, testAlertTimeout)
	defer cancel()
	target := base.JoinPath("api", "v1", "ingest", source)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The error of the client holds the address, which is no secret,
		// and never the token.
		return fmt.Errorf("could not reach Beacon at %s: %w; is it running, and is --url (or BEACON_URL) its address?", base.Redacted(), err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about it
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var answer struct {
		Result  string `json:"result"`
		AlertID string `json:"alert_id"`
		Error   string `json:"error"`
		Field   string `json:"field"`
	}
	_ = json.Unmarshal(raw, &answer)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("Beacon refused the request: it has no source named %q, or the token is not that source's. "+ //nolint:staticcheck // the name of the product starts the sentence
			"Check the name with --source, and set --token or BEACON_SOURCE_TOKEN to the token the source has in the configuration", source)
	case resp.StatusCode == http.StatusBadRequest && answer.Field == "escalation_policy":
		return fmt.Errorf("the configuration has no escalation policy named %q; `beacon validate --config <path>` counts the policies, and the configuration files name them", policy)
	case resp.StatusCode == http.StatusBadRequest && answer.Error != "":
		return fmt.Errorf("Beacon did not accept the alert: %s; the source %q must be of type webhook", strings.TrimSpace(answer.Field+" "+answer.Error), source) //nolint:staticcheck // as above
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated:
		return fmt.Errorf("Beacon answered %s; its log says why", resp.Status) //nolint:staticcheck // as above
	}

	// The link is for a browser, which may reach Beacon at another address
	// than this command does: from inside a container, for one.
	shown := base
	if public != "" {
		if shown, err = url.Parse(public); err != nil || shown.Host == "" {
			return fmt.Errorf("--public-url must be an http or https address such as http://localhost:8080, got %q", public)
		}
	}
	page := shown.JoinPath("alerts", answer.AlertID).String()
	switch answer.Result {
	case "created":
		fmt.Fprintf(e.stdout, "The test alert is firing: %s\n", page)
		if policy != "" {
			fmt.Fprintf(e.stdout, "It is paged through the escalation policy %q once the wait of its group is over (group_wait: 30s unless the configuration says otherwise).\n", policy)
		} else {
			fmt.Fprintln(e.stdout, "It is paged through the escalation policy that routing gives it, once the wait of its group is over (group_wait: 30s unless the configuration says otherwise). A configuration without a routing block pages nobody.")
		}
		fmt.Fprintln(e.stdout, "To end it: beacon test-alert --resolve"+policyFlag(policy))
	case "updated":
		fmt.Fprintf(e.stdout, "The test alert was already firing and has been refreshed: %s\nAn alert that is open pages once. To page again, end it first: beacon test-alert --resolve%s\n", page, policyFlag(policy))
	case "resolved":
		fmt.Fprintf(e.stdout, "The test alert is resolved: %s\n", page)
	case "not_found":
		fmt.Fprintln(e.stdout, "There is no open test alert to resolve.")
	default:
		fmt.Fprintf(e.stdout, "Beacon accepted the request: %s\n", strings.TrimSpace(string(raw)))
	}
	return nil
}

func policyFlag(policy string) string {
	if policy == "" {
		return ""
	}
	return " --policy " + policy
}
