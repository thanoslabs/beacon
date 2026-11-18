package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
)

// Config holds the pipeline settings. Zero values take the defaults.
type Config struct {
	// Workers is how many deliveries one process makes at a time.
	Workers int
	// PollInterval is how often an idle worker looks for due notifications.
	PollInterval time.Duration
	// MaxAttempts is how many times a notification is tried before it is
	// marked failed.
	MaxAttempts int
	// SendTimeout bounds one attempt.
	SendTimeout time.Duration
	// BackoffBase is the wait after the first failure. Each further failure
	// doubles it, up to BackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// Defaults for Config. With them a notification that keeps failing is tried
// eight times over about ten minutes.
const (
	DefaultWorkers      = 4
	DefaultPollInterval = time.Second
	DefaultMaxAttempts  = 8
	DefaultSendTimeout  = 15 * time.Second
	DefaultBackoffBase  = 5 * time.Second
	DefaultBackoffMax   = 5 * time.Minute
)

// claimMargin is how much longer than SendTimeout a claim lasts, to leave
// time to record the attempt.
const claimMargin = 30 * time.Second

// maxErrorLength bounds the error text stored for an attempt.
const maxErrorLength = 500

// Pipeline is the notification queue and the workers that deliver from it.
type Pipeline struct {
	db     *pgxpool.Pool
	alerts *alert.Store
	logger *slog.Logger
	cfg    Config
	now    func() time.Time
	// channels and policies follow the configuration, and are replaced as a
	// whole when it is reloaded.
	channels atomic.Pointer[map[string]Channel]
	policies atomic.Pointer[map[string]Policy]
}

// New returns a pipeline. Register its channels before calling Run.
func New(db *pgxpool.Pool, alerts *alert.Store, logger *slog.Logger, cfg Config) *Pipeline {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = DefaultBackoffBase
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = DefaultBackoffMax
	}
	p := &Pipeline{db: db, alerts: alerts, logger: logger, cfg: cfg, now: time.Now}
	p.SetChannels(nil)
	p.SetPolicies(nil)
	return p
}

// SetClock replaces the clock, so tests can move time.
func (p *Pipeline) SetClock(now func() time.Time) { p.now = now }

// Register makes ch the channel for the integration with this identifier.
// Call it before Run; to change channels while the pipeline runs, use
// SetChannels.
func (p *Pipeline) Register(integrationID string, ch Channel) {
	channels := maps.Clone(*p.channels.Load())
	channels[integrationID] = ch
	p.channels.Store(&channels)
}

// SetChannels replaces every channel, by integration identifier. It is safe
// while the pipeline runs. A notification already queued for an integration
// that no longer has a channel fails permanently when it is tried.
func (p *Pipeline) SetChannels(channels map[string]Channel) {
	channels = maps.Clone(channels)
	if channels == nil {
		channels = map[string]Channel{}
	}
	p.channels.Store(&channels)
}

// SetPolicies replaces the escalation policies, by the identifier of their
// rows. It is safe while alerts change. A group whose policy is not among
// them notifies nobody when its first notification falls due.
func (p *Pipeline) SetPolicies(policies map[string]Policy) {
	policies = maps.Clone(policies)
	if policies == nil {
		policies = map[string]Policy{}
	}
	p.policies.Store(&policies)
}

// Enqueue queues n for delivery through tx, so the notification exists if and
// only if the change that caused it commits. Only GroupID, IntegrationID,
// Destination and Reason are read.
func (p *Pipeline) Enqueue(ctx context.Context, tx pgx.Tx, n Notification) error {
	now := p.now()
	_, err := tx.Exec(ctx, `
		INSERT INTO notifications (alert_group_id, integration_id, destination, reason, next_attempt_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $5)`, n.GroupID, n.IntegrationID, n.Destination, n.Reason, now)
	if err != nil {
		return fmt.Errorf("queue notification: %w", err)
	}
	return nil
}

// Target is a shared destination, such as a chat channel, reached through an
// integration.
type Target struct {
	IntegrationID string
	Destination   string
}

// Run delivers queued notifications until ctx is cancelled. Deliveries that
// are in progress then finish, bounded by SendTimeout, before Run returns:
// abandoning a request the service may already have accepted would send the
// message twice.
func (p *Pipeline) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range p.cfg.Workers {
		wg.Go(func() { p.work(ctx) })
	}
	wg.Wait()
}

func (p *Pipeline) work(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// Drain the queue, then sleep.
		for ctx.Err() == nil {
			n, ok, err := p.claim(ctx)
			if err != nil && ctx.Err() == nil {
				p.logger.ErrorContext(ctx, "claim notification", slog.Any("error", err))
			}
			if err != nil || !ok {
				break
			}
			p.deliver(ctx, n)
		}
		timer.Reset(p.cfg.PollInterval)
	}
}

// claim takes the next due notification, if there is one, and hides it from
// other workers until the claim runs out. That is what makes the queue safe
// with several workers and replicas, and what returns a notification to the
// queue when the process that claimed it dies.
//
// Notifications about one group to one destination are delivered one at a
// time, oldest first: a notification waits while an older one is pending.
// Otherwise an update could overtake the message it is meant to update.
func (p *Pipeline) claim(ctx context.Context) (n Notification, ok bool, err error) {
	now := p.now()
	err = p.db.QueryRow(ctx, `
		UPDATE notifications SET attempts = attempts + 1, next_attempt_at = $2
		WHERE id = (
			SELECT n.id FROM notifications n
			WHERE n.status = 'pending' AND n.next_attempt_at <= $1
				AND NOT EXISTS (
					SELECT 1 FROM notifications older
					WHERE older.alert_group_id = n.alert_group_id AND older.integration_id = n.integration_id
						AND older.destination = n.destination AND older.status = 'pending'
						AND (older.created_at, older.id) < (n.created_at, n.id))
			ORDER BY n.next_attempt_at, n.created_at
			LIMIT 1 FOR UPDATE OF n SKIP LOCKED)
		RETURNING id::text, alert_group_id::text, integration_id::text, destination, reason, attempts, created_at`,
		now, now.Add(p.cfg.SendTimeout+claimMargin)).
		Scan(&n.ID, &n.GroupID, &n.IntegrationID, &n.Destination, &n.Reason, &n.Attempts, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, nil
	}
	return n, err == nil, err
}

// deliver makes one attempt and records it.
func (p *Pipeline) deliver(ctx context.Context, n Notification) {
	// Not cancelled by shutdown; see Run.
	ctx = context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithTimeout(ctx, p.cfg.SendTimeout)
	ref, sendErr := p.send(sendCtx, n)
	cancel()

	recordCtx, cancel := context.WithTimeout(ctx, claimMargin/2)
	defer cancel()
	if err := p.record(recordCtx, n, ref, sendErr); err != nil {
		// The claim runs out and the notification is tried again. If this
		// attempt was delivered, that sends the message a second time.
		p.logger.ErrorContext(ctx, "record notification attempt", slog.String("notification_id", n.ID), slog.Any("error", err))
	}
}

func (p *Pipeline) send(ctx context.Context, n Notification) (ref string, err error) {
	ch, ok := (*p.channels.Load())[n.IntegrationID]
	if !ok {
		return "", Permanent(errors.New("no channel is configured for the integration"))
	}
	m := Message{Notification: n}
	if m.Group, err = p.alerts.Group(ctx, n.GroupID); err != nil {
		return "", fmt.Errorf("load group: %w", err)
	}
	if m.Alerts, err = p.alerts.GroupAlerts(ctx, n.GroupID); err != nil {
		return "", fmt.Errorf("load alerts: %w", err)
	}
	if m.Events, err = p.alerts.GroupTimeline(ctx, n.GroupID); err != nil {
		return "", fmt.Errorf("load timeline: %w", err)
	}
	err = p.db.QueryRow(ctx, `SELECT external_ref FROM notifications
		WHERE alert_group_id = $1 AND integration_id = $2 AND destination = $3 AND status = 'sent' AND external_ref <> ''
		ORDER BY sent_at DESC LIMIT 1`, n.GroupID, n.IntegrationID, n.Destination).Scan(&m.PreviousRef)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("load previous message: %w", err)
	}

	// A channel that panics must not take the worker down with it.
	defer func() {
		if r := recover(); r != nil {
			ref, err = "", fmt.Errorf("channel panicked: %v", r)
		}
	}()
	return ch.Send(ctx, m)
}

// record writes the outcome of an attempt: the new state of the notification,
// the attempt row and the timeline event, in one transaction.
func (p *Pipeline) record(ctx context.Context, n Notification, ref string, sendErr error) error {
	now := p.now()
	outcome, status, errText := OutcomeSuccess, "sent", ""
	var nextAttempt, sentAt *time.Time
	if sendErr == nil {
		sentAt = &now
	} else {
		errText = truncate(sendErr.Error(), maxErrorLength)
		permanent, retryAfter := classify(sendErr)
		switch {
		case permanent:
			outcome, status = OutcomePermanentError, "failed"
		case n.Attempts >= p.cfg.MaxAttempts:
			outcome, status = OutcomeRetryableError, "failed"
		default:
			outcome, status = OutcomeRetryableError, "pending"
			at := now.Add(max(p.backoff(n.Attempts), retryAfter))
			nextAttempt = &at
		}
	}

	return pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		// The attempts check makes sure this worker still holds the claim.
		tag, err := tx.Exec(ctx, `
			UPDATE notifications SET status = $3, sent_at = $4, external_ref = NULLIF($5, ''),
				next_attempt_at = COALESCE($6, next_attempt_at)
			WHERE id = $1 AND status = 'pending' AND attempts = $2`,
			n.ID, n.Attempts, status, sentAt, ref, nextAttempt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("the claim ran out before the attempt finished")
		}
		_, err = tx.Exec(ctx, `INSERT INTO notification_attempts (notification_id, attempted_at, outcome, error)
			VALUES ($1, $2, $3, NULLIF($4, ''))`, n.ID, now, outcome, errText)
		if err != nil {
			return err
		}

		data := map[string]any{
			"notification_id": n.ID,
			"integration_id":  n.IntegrationID,
			"destination":     n.Destination,
			"reason":          n.Reason,
			"attempt":         n.Attempts,
			"outcome":         outcome,
		}
		typ := EventSent
		if sendErr != nil {
			typ = EventFailed
			data["error"] = errText
			data["will_retry"] = status == "pending"
		}
		return alert.AppendEvent(ctx, tx, &alert.Event{GroupID: n.GroupID, Type: typ, Actor: alert.System(), OccurredAt: now, Data: data})
	})
}

// backoff returns how long to wait after the given number of failed attempts:
// BackoffBase doubled for each failure after the first, capped at BackoffMax,
// and shortened by up to a fifth at random so that notifications which failed
// together do not all come back together.
func (p *Pipeline) backoff(attempts int) time.Duration {
	d := p.cfg.BackoffBase
	for i := 1; i < attempts && d < p.cfg.BackoffMax; i++ {
		d *= 2
	}
	d = min(d, p.cfg.BackoffMax)
	return d - time.Duration(rand.Int64N(int64(d)/5+1))
}

// truncate shortens s to at most n bytes of text that Postgres accepts.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > n {
		s = s[:n]
	}
	// Cutting may have split a character.
	return strings.ToValidUTF8(s, "")
}
