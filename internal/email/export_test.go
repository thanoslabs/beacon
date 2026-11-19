package email

import "crypto/x509"

// SetRoots makes the channel trust the certificate authorities of a test
// server instead of the system's.
func (c *Channel) SetRoots(roots *x509.CertPool) { c.server.roots = roots }

// WebAddress exposes webAddress to tests.
func WebAddress(raw string) string { return webAddress(raw) }
