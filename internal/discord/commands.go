package discord

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// registerLinkCommand creates the link command of an application, as a
// global command. Discord overwrites a command of the same name, so doing it
// again is harmless, and the application's other commands are left alone.
func (c *Client) registerLinkCommand(ctx context.Context, applicationID string) error {
	path := "/applications/" + url.PathEscape(applicationID) + "/commands"
	return c.do(ctx, http.MethodPost, path, "register the link command", "POST "+path, map[string]any{
		// 1 is a slash command, and 3 an option that takes text.
		"type":        1,
		"name":        LinkCommand,
		"description": "Connect your Discord account to your Beacon user",
		"options": []map[string]any{{
			"type": 3, "name": linkOption, "description": "The link code that Beacon gave you", "required": true,
		}},
	}, nil)
}

// Application is a Discord application whose link command Beacon keeps
// registered.
type Application struct {
	// Integration is the name of the integration, for the log.
	Integration string
	ID          string
	Client      *Client
}

// DefaultCommandInterval is how often a process looks for applications
// whose link command it has not registered.
const DefaultCommandInterval = time.Minute

// Commands registers the link command with Discord for every Discord
// integration, so that an operator does not have to. It does so in the
// background: Beacon starts, and delivers, whether or not Discord answers.
//
// Which applications this process has registered is remembered in the
// process only. Registering is an overwrite with the same content, so a
// restart, or another replica, that does it again changes nothing.
type Commands struct {
	logger *slog.Logger
	// applications follows the configuration, and is replaced as a whole
	// when it is reloaded.
	applications atomic.Pointer[[]Application]
	kick         chan struct{}

	now func() time.Time

	mu sync.Mutex
	// done holds the applications that were registered, by ID, and failed
	// the ones that could not be, with when to try again.
	done   map[string]bool
	failed map[string]retry
}

// retry is when an application whose registration failed is tried again,
// and how long it waited for that.
type retry struct {
	at   time.Time
	wait time.Duration
}

// maxCommandRetry bounds the wait between two attempts to register. A
// token that is wrong stays wrong, and Discord counts refused requests
// against the sender.
const maxCommandRetry = time.Hour

// NewCommands returns a registrar that knows no application yet.
func NewCommands(logger *slog.Logger) *Commands {
	c := &Commands{logger: logger, kick: make(chan struct{}, 1), now: time.Now, done: map[string]bool{}, failed: map[string]retry{}}
	c.SetApplications(nil)
	return c
}

// SetClock replaces the clock, so tests can move time.
func (c *Commands) SetClock(now func() time.Time) { c.now = now }

// SetApplications replaces the applications and makes Run look at them
// now: a reload is a reason to try again what failed. It is safe while Run
// runs, and never blocks.
func (c *Commands) SetApplications(applications []Application) {
	c.applications.Store(&applications)
	c.mu.Lock()
	clear(c.failed)
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run registers what is not registered, every interval and whenever the
// applications change, until ctx is cancelled.
func (c *Commands) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.kick:
		}
		c.Register(ctx)
	}
}

// Register registers the link command of every application that this
// process has not registered yet, and reports how many it registered. A
// failure is logged and tried again later: after a minute, then after twice
// as long each time, up to an hour.
func (c *Commands) Register(ctx context.Context) (registered int) {
	for _, app := range *c.applications.Load() {
		now := c.now()
		c.mu.Lock()
		skip := c.done[app.ID] || c.failed[app.ID].at.After(now)
		c.mu.Unlock()
		if skip || ctx.Err() != nil {
			continue
		}
		if err := app.Client.registerLinkCommand(ctx, app.ID); err != nil {
			if ctx.Err() != nil {
				continue
			}
			c.mu.Lock()
			wait := min(max(2*c.failed[app.ID].wait, DefaultCommandInterval), maxCommandRetry)
			c.failed[app.ID] = retry{at: now.Add(wait), wait: wait}
			c.mu.Unlock()
			c.logger.ErrorContext(ctx, "register the link command with Discord; people cannot link their Discord accounts until it is registered",
				slog.String("integration", app.Integration), slog.String("command", "/"+LinkCommand), slog.String("next_attempt_in", wait.String()), slog.Any("error", err))
			continue
		}
		c.mu.Lock()
		c.done[app.ID] = true
		delete(c.failed, app.ID)
		c.mu.Unlock()
		registered++
		c.logger.InfoContext(ctx, "link command registered with Discord", slog.String("integration", app.Integration), slog.String("command", "/"+LinkCommand))
	}
	return registered
}
