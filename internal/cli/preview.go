package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// The longest time ahead that a preview looks.
const maxPreviewWeeks = 104

func runPreview(ctx context.Context, e env, args []string) error {
	var (
		path    string
		dbCfg   postgres.Config
		weeks   int
		only    string
		format  string
		fail    bool
		envErrs []error
	)
	fs := newFlagSet(e, "preview", "Show which upcoming on-call shifts a configuration would move, without applying it")
	configSetting(fs, e, &path)
	databaseSettings(fs, e, &dbCfg, &envErrs)
	fs.IntVar(&weeks, "weeks", 8, "how many weeks ahead to look")
	fs.StringVar(&only, "schedule", "", "show only the schedule with this name")
	fs.StringVar(&format, "format", "text", "output format: text or json")
	fs.BoolVar(&fail, "fail-on-moves", false, "exit with status 1 when the configuration would move an upcoming shift, for use as a check in CI")
	if err := parseFlags(fs, args, envErrs...); err != nil {
		return err
	}
	if path == "" {
		return errors.New("the configuration path is required: set --config or BEACON_CONFIG")
	}
	if err := checkDatabaseSettings(dbCfg); err != nil {
		return err
	}
	if weeks < 1 || weeks > maxPreviewWeeks {
		return fmt.Errorf("--weeks must be between 1 and %d, got %d", maxPreviewWeeks, weeks)
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("--format must be text or json, got %q", format)
	}
	// Like validate, the preview needs no secret: it reads the schedules.
	cfg, err := checkConfig(e, path, config.LoadOptions{})
	if err != nil {
		return err
	}

	pool, err := postgres.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	now := time.Now()
	until := now.AddDate(0, 0, 7*weeks)
	previews, err := schedule.NewStore(pool).Preview(ctx, cfg.Schedules, until)
	if err != nil {
		return fmt.Errorf("%w; the preview reads the database of a running Beacon, whose schema must be up to date", err)
	}
	if only != "" {
		previews = slices.DeleteFunc(previews, func(p schedule.Preview) bool { return p.Schedule != only })
		if len(previews) == 0 {
			return fmt.Errorf("no schedule is named %q, in the configuration or running", only)
		}
	}

	if format == "json" {
		err = writePreviewJSON(e.stdout, previews, now, until)
	} else {
		writePreviewText(e.stdout, previews, weeks)
	}
	if err != nil {
		return err
	}
	moved := 0
	for _, p := range previews {
		if len(p.Moves) > 0 || p.Status == schedule.PreviewRemoved {
			moved++
		}
	}
	if fail && moved > 0 {
		return fmt.Errorf("%s would have upcoming shifts moved or ended", plural(moved, "schedule"))
	}
	return nil
}

// zone returns the time zone to show a schedule's times in.
func zone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil && name != "" {
		return loc
	}
	return time.UTC
}

const previewTime = "Mon 2006-01-02 15:04 MST"

func person(email string) string {
	if email == "" {
		return "nobody"
	}
	return email
}

func writePreviewText(w io.Writer, previews []schedule.Preview, weeks int) {
	if len(previews) == 0 {
		fmt.Fprintln(w, "the configuration has no schedules, and none is running")
		return
	}
	for i, p := range previews {
		if i > 0 {
			fmt.Fprintln(w)
		}
		loc := zone(p.TimeZone)
		when := func(t time.Time) string { return t.In(loc).Format(previewTime) }
		takesOver := "at the next handoff, " + when(p.TakesOverAt)
		if p.Stated {
			takesOver = "at the stated time, " + when(p.TakesOverAt)
		}
		switch p.Status {
		case schedule.PreviewNew:
			fmt.Fprintf(w, "schedule %s: new. Its rotation starts when the configuration is applied.\n", p.Schedule)
		case schedule.PreviewRemoved:
			fmt.Fprintf(w, "schedule %s: removed. Its shift in progress ends when the configuration is applied; recorded shifts are kept.\n", p.Schedule)
			continue
		case schedule.PreviewUnchanged:
			fmt.Fprintf(w, "schedule %s: unchanged.\n", p.Schedule)
		case schedule.PreviewPending:
			fmt.Fprintf(w, "schedule %s: this definition is applied already and takes over %s.\n", p.Schedule, takesOver)
		case schedule.PreviewChanged:
			fmt.Fprintf(w, "schedule %s: changed. The new definition takes over %s.\n", p.Schedule, takesOver)
		}
		for _, email := range p.Skipped {
			fmt.Fprintf(w, "  passed over: %s is not a user yet\n", email)
		}
		if p.Status == schedule.PreviewChanged {
			if len(p.Moves) == 0 {
				fmt.Fprintf(w, "  no shift moves in the next %s\n", plural(weeks, "week"))
			} else {
				fmt.Fprintf(w, "  the person on call would change in %s in the next %s:\n", plural(len(p.Moves), "period"), plural(weeks, "week"))
			}
			for _, m := range p.Moves {
				fmt.Fprintf(w, "    %s to %s: %s instead of %s\n", when(m.Start), when(m.End), person(m.Becomes), person(m.Was))
			}
		}
		if p.Status != schedule.PreviewUnchanged && len(p.Upcoming) > 0 {
			fmt.Fprintln(w, "  shifts with this configuration:")
			for _, s := range p.Upcoming {
				note := ""
				if s.Override {
					note = " (override)"
				}
				fmt.Fprintf(w, "    %s to %s: %s%s\n", when(s.Start), when(s.End), person(s.Member), note)
			}
		}
	}
}

func writePreviewJSON(w io.Writer, previews []schedule.Preview, from, until time.Time) error {
	type move struct {
		Start   time.Time `json:"start"`
		End     time.Time `json:"end"`
		Was     string    `json:"was"`
		Becomes string    `json:"becomes"`
	}
	type shift struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
		User  string    `json:"user"`
		// Kind is rotation or override.
		Kind string `json:"kind"`
	}
	type item struct {
		Schedule    string     `json:"schedule"`
		Status      string     `json:"status"`
		TakesOverAt *time.Time `json:"takes_over_at,omitempty"`
		Stated      bool       `json:"stated,omitempty"`
		Skipped     []string   `json:"skipped"`
		Moves       []move     `json:"moves"`
		Shifts      []shift    `json:"shifts"`
	}
	out := struct {
		From      time.Time `json:"from"`
		Until     time.Time `json:"until"`
		Schedules []item    `json:"schedules"`
	}{From: from.UTC(), Until: until.UTC(), Schedules: []item{}}
	for _, p := range previews {
		it := item{Schedule: p.Schedule, Status: p.Status, Stated: p.Stated, Skipped: append([]string{}, p.Skipped...), Moves: []move{}, Shifts: []shift{}}
		if !p.TakesOverAt.IsZero() {
			at := p.TakesOverAt.UTC()
			it.TakesOverAt = &at
		}
		for _, m := range p.Moves {
			it.Moves = append(it.Moves, move{Start: m.Start.UTC(), End: m.End.UTC(), Was: m.Was, Becomes: m.Becomes})
		}
		for _, s := range p.Upcoming {
			kind := schedule.KindRotation
			if s.Override {
				kind = schedule.KindOverride
			}
			it.Shifts = append(it.Shifts, shift{Start: s.Start.UTC(), End: s.End.UTC(), User: s.Member, Kind: kind})
		}
		out.Schedules = append(out.Schedules, it)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
