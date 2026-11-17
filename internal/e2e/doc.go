// Package e2e holds the end-to-end tests. They build the beacon binary, run
// it as a separate process against a real Postgres and a fake Slack, and use
// it only from the outside: over HTTP, as a source and as Slack would.
//
// The tests need BEACON_TEST_DATABASE_URL, like every database test; see
// package postgrestest.
package e2e
