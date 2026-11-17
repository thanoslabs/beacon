package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/thanoslabs/beacon/internal/config"
)

// CallbackPath is where the identity provider sends people back. The
// redirect URI to register with the provider is the public URL followed by
// this path.
const CallbackPath = "/auth/callback"

// providerTimeout bounds every request to the identity provider.
const providerTimeout = 10 * time.Second

// provider is an identity provider whose discovery document has been read.
// The protocol work is done by go-oidc and oauth2; this type only joins them
// to Beacon's settings.
type provider struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// discover reads the provider's discovery document and prepares to sign
// people in with it.
func discover(ctx context.Context, cfg config.OIDC, redirectURL string) (*provider, error) {
	client := &http.Client{Timeout: providerTimeout}
	// The provider keeps this context to fetch signing keys later, so it
	// must outlive the request that triggered discovery. The client's
	// timeout bounds each request instead.
	ctx = oidc.ClientContext(context.WithoutCancel(ctx), client)
	discovered, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the discovery document of %s: %w", cfg.Issuer, err)
	}
	return &provider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret.Reveal(),
			Endpoint:     discovered.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       cfg.RequestedScopes(),
		},
		// The verifier checks the signature against the provider's keys, the
		// issuer, that the token was issued to this client, and its expiry.
		verifier: discovered.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		client:   client,
	}, nil
}

// authURL is where the browser is sent to sign in.
func (p *provider) authURL(f flow) string {
	return p.oauth.AuthCodeURL(f.state, oidc.Nonce(f.nonce), oauth2.S256ChallengeOption(f.verifier))
}

// claims are the parts of the ID token that Beacon reads.
type claims struct {
	Email             string `json:"email"`
	EmailVerified     *flag  `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// flag is a boolean that some providers send as the string "true".
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	*f = flag(strings.Trim(string(b), `"`) == "true")
	return nil
}

// errRejected marks a sign-in that the provider completed but Beacon does
// not accept. Its message is safe to show to the person.
type errRejected struct{ reason string }

func (e *errRejected) Error() string { return e.reason }

// exchange trades the authorisation code for tokens, verifies the ID token
// and returns who signed in, with all the token's claims.
func (p *provider) exchange(ctx context.Context, code string, f flow) (Identity, map[string]any, error) {
	ctx = oidc.ClientContext(ctx, p.client)
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		// The provider's answer is not passed on: only its error code is
		// known to be safe to log.
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return Identity{}, nil, fmt.Errorf("the provider refused the authorisation code: status %d, error %q", re.Response.StatusCode, re.ErrorCode)
		}
		return Identity{}, nil, fmt.Errorf("exchange the authorisation code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, nil, errors.New("the provider's answer has no ID token")
	}
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, nil, fmt.Errorf("verify the ID token: %w", err)
	}
	// The nonce ties the token to the sign-in that this browser started.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(f.nonce)) != 1 {
		return Identity{}, nil, errors.New("verify the ID token: the nonce does not match the sign-in")
	}

	var c claims
	all := map[string]any{}
	if err := idToken.Claims(&c); err != nil {
		return Identity{}, nil, fmt.Errorf("read the ID token: %w", err)
	}
	if err := idToken.Claims(&all); err != nil {
		return Identity{}, nil, fmt.Errorf("read the ID token: %w", err)
	}
	email := config.NormalizeEmail(c.Email)
	switch {
	case email == "":
		return Identity{}, nil, &errRejected{"the identity provider did not share an email address; Beacon needs the email scope and claim"}
	case c.EmailVerified != nil && !bool(*c.EmailVerified):
		// Users are matched to teams and schedules by email address, so an
		// address that anyone could claim must not be trusted.
		return Identity{}, nil, &errRejected{"the identity provider has not verified this email address"}
	}
	name := c.Name
	if name == "" {
		name = c.PreferredUsername
	}
	return Identity{Issuer: idToken.Issuer, Subject: idToken.Subject, Email: email, Name: name}, all, nil
}
