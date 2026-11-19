package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
)

// server is an SMTP server and how Beacon talks to it.
type server struct {
	cfg config.EmailIntegration
	// roots are the certificate authorities that the server's certificate
	// may come from. Nil means the system's, which is what an install uses;
	// tests set their own.
	roots *x509.CertPool
}

// send delivers one message to one recipient: it connects, secures the
// connection the way the configuration says, signs in when it has
// credentials, and hands the message over.
//
// The connection is never downgraded. With starttls, a server that does not
// offer STARTTLS is refused, and the password is only ever sent over TLS,
// or to a server on this machine.
//
// Its errors say whether a retry can help, in the pipeline's terms, and
// never contain the password.
func (s server) send(ctx context.Context, to string, message []byte) error {
	host := s.cfg.Host
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("email: connect to the SMTP server: %w", cause(err))
	}
	// The whole exchange is bounded by the attempt's deadline, and ends at
	// once when the context is cancelled.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close() //nolint:errcheck // closing twice is harmless, and there is nothing to do if it fails

	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: s.roots}
	if s.cfg.TLS == config.EmailTLSImplicit {
		secured := tls.Client(conn, tlsConfig)
		if err := secured.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("email: TLS handshake with the SMTP server: %w", cause(err))
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return stage(ctx, "greeting of the SMTP server", err)
	}
	if err := client.Hello(localName()); err != nil {
		return stage(ctx, "EHLO", err)
	}
	if s.cfg.TLS == config.EmailTLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return notify.Permanent(errors.New("email: the SMTP server does not offer STARTTLS, and Beacon does not fall back to an unencrypted connection; use tls: implicit for a port that speaks TLS from the start"))
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return stage(ctx, "STARTTLS", err)
		}
	}
	if s.cfg.Username != "" {
		auth, err := s.auth(client)
		if err != nil {
			return err
		}
		if err := client.Auth(auth); err != nil {
			return stage(ctx, "sign in to the SMTP server", err)
		}
	}
	if err := client.Mail(s.cfg.From); err != nil {
		return stage(ctx, "MAIL FROM", err)
	}
	if err := client.Rcpt(to); err != nil {
		return stage(ctx, "RCPT TO", err)
	}
	w, err := client.Data()
	if err != nil {
		return stage(ctx, "DATA", err)
	}
	if _, err := w.Write(message); err != nil {
		return stage(ctx, "send the message", err)
	}
	// The server accepts the message when the data is closed.
	if err := w.Close(); err != nil {
		return stage(ctx, "send the message", err)
	}
	// The message is the server's now. Whether the goodbye arrives does not
	// change that.
	_ = client.Quit()
	return nil
}

// auth picks the way to sign in from what the server offers: PLAIN, or else
// LOGIN, which some large providers offer alone. Both send the password as
// it is, so both insist on TLS.
func (s server) auth(client *smtp.Client) (smtp.Auth, error) {
	_, offered := client.Extension("AUTH")
	mechanisms := strings.Fields(strings.ToUpper(offered))
	password := s.cfg.Password.Reveal()
	switch {
	case slices.Contains(mechanisms, "PLAIN"):
		// PlainAuth refuses to send the password over a connection that is
		// not encrypted, unless the server is on this machine.
		return smtp.PlainAuth("", s.cfg.Username, password, s.cfg.Host), nil
	case slices.Contains(mechanisms, "LOGIN"):
		return &loginAuth{username: s.cfg.Username, password: password, host: s.cfg.Host}, nil
	}
	return nil, notify.Permanent(errors.New("email: the SMTP server offers no way to sign in that Beacon knows (PLAIN or LOGIN); remove username and password if it needs none"))
}

// loginAuth is the LOGIN mechanism: the server asks for the user name and
// then for the password. It is not standardised, and common all the same.
type loginAuth struct {
	username, password, host string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// The same rule as PlainAuth: never over a connection that is not
	// encrypted, unless the server is on this machine.
	if !server.TLS && !isLocalhost(server.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch prompt := strings.ToLower(strings.TrimSpace(string(fromServer))); {
	case strings.HasPrefix(prompt, "username"):
		return []byte(a.username), nil
	case strings.HasPrefix(prompt, "password"):
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected question from the server during LOGIN")
}

func isLocalhost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}

var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// localName is what Beacon calls itself in EHLO: the host's name when it is
// one a server will take, and localhost otherwise.
func localName() string {
	if name, err := os.Hostname(); err == nil && hostnameRE.MatchString(name) {
		return name
	}
	return "localhost"
}

// maxReply bounds how much of a server's reply goes into an error.
const maxReply = 200

// stage turns a failure of one step of the exchange into an error that says
// which step, and whether trying again can help: a reply that starts with 5
// means that it cannot.
func stage(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The connection was closed because the attempt ran out of time.
		return fmt.Errorf("email: %s: %w", what, ctxErr)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The connection carries the attempt's deadline, and may notice it
		// a moment before the context does.
		return fmt.Errorf("email: %s: %w", what, context.DeadlineExceeded)
	}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		// The text is the server's. It may name the recipient, and never
		// holds the password, which the server does not send back.
		msg := strings.Join(strings.Fields(reply.Msg), " ")
		if len(msg) > maxReply {
			msg = strings.ToValidUTF8(msg[:maxReply], "") + "…"
		}
		out := fmt.Errorf("email: %s: the SMTP server answered %d %s", what, reply.Code, msg)
		if reply.Code >= 500 {
			return notify.Permanent(out)
		}
		return out
	}
	return fmt.Errorf("email: %s: %w", what, cause(err))
}

// cause strips the address from a network error, which names the server
// well enough by the stage it failed at.
func cause(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err
	}
	return err
}
