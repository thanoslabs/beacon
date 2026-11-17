package notify

import "context"

// Claim exposes claim to tests, which use it to stand in for a worker that
// took a notification and then died.
func (p *Pipeline) Claim(ctx context.Context) (Notification, bool, error) { return p.claim(ctx) }

// Backoff exposes backoff to tests.
func (p *Pipeline) Backoff(attempts int) int64 { return int64(p.backoff(attempts)) }
