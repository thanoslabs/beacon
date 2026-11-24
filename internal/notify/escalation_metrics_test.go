package notify_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
	"github.com/thanoslabs/beacon/internal/notify"
)

func step(n string, result string) string {
	return `beacon_escalation_steps_total{policy="default",result="` + result + `",step="` + n + `"}`
}

const exhaustedDefault = `beacon_escalations_exhausted_total{policy="default"}`

// TestEscalationMetrics: every step that runs is counted once, by policy
// and by its number, and an escalation that nobody responded to is counted
// when it ends.
func TestEscalationMetrics(t *testing.T) {
	f := newEscalationFixture(t)
	reg := prometheus.NewRegistry()
	f.p.RegisterMetrics(reg)
	// The steps of a policy are there from the start, with zero.
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{
		step("1", "notified"): 0, step("2", "notified"): 0, step("3", "notified"): 0, step("3", "nobody"): 0, exhaustedDefault: 0,
	})

	f.open("db1")
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{step("1", "notified"): 1, step("2", "notified"): 0})
	// Looking again before a step is due runs nothing and counts nothing.
	f.escalate()
	f.after(4 * time.Minute)
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{step("1", "notified"): 1, step("2", "notified"): 0})

	// The policy has three steps and runs twice; then it gives up.
	for _, wait := range []time.Duration{time.Minute, 10 * time.Minute, 15 * time.Minute, 5 * time.Minute, 10 * time.Minute} {
		f.after(wait)
	}
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{
		step("1", "notified"): 2, step("2", "notified"): 2, step("3", "notified"): 2, exhaustedDefault: 0,
	})
	f.after(15 * time.Minute)
	f.after(24 * time.Hour)
	got := metricstest.Values(t, reg)
	metricstest.Expect(t, got, map[string]float64{
		step("1", "notified"): 2, step("2", "notified"): 2, step("3", "notified"): 2, step("1", "nobody"): 0, exhaustedDefault: 1,
	})
	// Three steps, two results, and nothing else: no group, no pass.
	if n := len(metricstest.WithPrefix(got, "beacon_escalation_steps_total")); n != 6 {
		t.Errorf("%d series of steps, want 6: %v", n, metricstest.WithPrefix(got, "beacon_escalation_steps_total"))
	}
}

// TestEscalationStepThatNotifiesNobodyIsCountedAsSuch: a step whose every
// target was skipped ran, and paged no one. That is the series to alert on.
func TestEscalationStepThatNotifiesNobodyIsCountedAsSuch(t *testing.T) {
	f := newEscalationFixture(t)
	reg := prometheus.NewRegistry()
	f.p.RegisterMetrics(reg)
	f.p.SetPeople(&directory{})
	f.p.SetPolicies(map[string]notify.Policy{f.policyID: {Name: "default", Steps: []notify.Step{
		{Timeout: 5 * time.Minute, Targets: []notify.StepTarget{{Kind: notify.TargetSchedule, Name: "primary", ID: "s", Via: []notify.Via{{IntegrationID: f.integrationID, Name: "chat"}}}}},
	}}})
	f.open("db1")
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{step("1", "nobody"): 1, step("1", "notified"): 0})
}

// TestEscalationStepIsNotCountedWhenItRollsBack: a step runs inside the
// transaction that moves its timer. When that transaction fails after the
// step ran, nothing of the step remains, the step runs again later, and it
// is counted once.
func TestEscalationStepIsNotCountedWhenItRollsBack(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	f.p.RegisterMetrics(reg)
	g := f.open("db1")

	// The database refuses to move the timer, which is the last thing the
	// transaction of a step does.
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_timer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.step_index <> OLD.step_index THEN RAISE EXCEPTION 'the timer cannot be moved'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER refuse_timer BEFORE UPDATE ON escalations FOR EACH ROW EXECUTE FUNCTION refuse_timer()`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Advance(5 * time.Minute)
	if _, err := f.p.EscalateDue(ctx); err == nil {
		t.Fatal("EscalateDue succeeded although the timer could not be moved")
	}
	f.check("after the failure", g, "#alerts", "active step 0 pass 0 due in 30s")
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{step("1", "notified"): 1, step("2", "notified"): 0})

	if _, err := f.pool.Exec(ctx, `DROP TRIGGER refuse_timer ON escalations`); err != nil {
		t.Fatal(err)
	}
	f.after(30 * time.Second)
	f.check("tried again", g, "#alerts #team", "active step 1 pass 0 due in 10m0s")
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{step("1", "notified"): 1, step("2", "notified"): 1})
}

// TestEscalationStepsAreCountedOnceAmongReplicas: twelve groups fall due
// while six replicas look at the same moment. Each step is run by one of
// them, and the replicas' counters add up to the steps that ran.
func TestEscalationStepsAreCountedOnceAmongReplicas(t *testing.T) {
	f := newEscalationFixture(t)
	for i := range 12 {
		f.fireInto(i)
	}
	if n := f.tick(); n != 12 {
		t.Fatalf("%d groups notified, want 12", n)
	}
	f.clock.Advance(17 * time.Minute)
	var (
		wg         sync.WaitGroup
		registries []*prometheus.Registry
	)
	for range 6 {
		p := f.replica()
		reg := prometheus.NewRegistry()
		p.RegisterMetrics(reg)
		registries = append(registries, reg)
		wg.Go(func() {
			if _, err := p.EscalateDue(context.Background()); err != nil {
				t.Errorf("EscalateDue: %v", err)
			}
		})
	}
	wg.Wait()
	// Whatever a replica held while another looked is picked up here, by
	// the fixture's own pipeline.
	reg := prometheus.NewRegistry()
	f.p.RegisterMetrics(reg)
	f.escalate()
	total := 0.0
	for _, r := range append(registries, reg) {
		total += metricstest.Values(t, r)[step("2", "notified")]
	}
	if total != 12 {
		t.Errorf("the replicas counted %v second steps for 12 groups", total)
	}
}
