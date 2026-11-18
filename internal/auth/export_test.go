package auth

// HashToken exposes hashToken to tests.
func HashToken(token string) string { return hashToken(token) }
