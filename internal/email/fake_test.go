package email_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a small SMTP server for what the real one in compose.yaml
// will not do: TLS in its two forms, a server that offers only LOGIN, and
// failures at a chosen step. It speaks just enough of the protocol for
// Beacon's client. Delivery to a real server is tested against Mailpit.
type fakeSMTP struct {
	t    *testing.T
	ln   net.Listener
	cert tls.Certificate
	// roots trusts the server's certificate.
	roots *x509.CertPool

	// The behaviour, set before the first connection.
	startTLS bool
	implicit bool
	// auth lists the mechanisms offered, such as "PLAIN LOGIN".
	auth string
	// replies maps a command, such as "RCPT", to the reply to give instead
	// of accepting it. "." is the end of the data.
	replies map[string]string
	// stall makes the server stop answering at a command.
	stall string

	mu sync.Mutex
	// commands are the commands received, with whether TLS was on.
	commands []string
	messages []string
	// credentials are the user name and password of each sign-in.
	credentials []string
	release     chan struct{}
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	s := &fakeSMTP{t: t, replies: map[string]string{}, release: make(chan struct{})}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	s.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	s.roots = x509.NewCertPool()
	s.roots.AddCert(parsed)
	return s
}

// start begins to listen and returns the port.
func (s *fakeSMTP) start() int {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	s.ln = ln
	s.t.Cleanup(func() {
		close(s.release)
		_ = ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func (s *fakeSMTP) note(command string, secure bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secure {
		command += " (TLS)"
	}
	s.commands = append(s.commands, command)
}

// seen returns the commands received so far, on one line.
func (s *fakeSMTP) seen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, ", ")
}

func (s *fakeSMTP) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // test server
	secure := false
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12}
	if s.implicit {
		secured := tls.Server(conn, tlsConfig)
		if secured.Handshake() != nil {
			return
		}
		conn, secure = secured, true
	}
	r := bufio.NewReader(conn)
	say := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	read := func() (string, bool) {
		line, err := r.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err == nil
	}
	say("220 fake.example.invalid ESMTP")
	for {
		line, ok := read()
		if !ok {
			return
		}
		command := strings.ToUpper(strings.Fields(line + " x")[0])
		s.note(command, secure)
		if command == s.stall {
			<-s.release
			return
		}
		if reply, fails := s.replies[command]; fails {
			say(reply)
			continue
		}
		switch command {
		case "EHLO":
			lines := []string{"fake.example.invalid"}
			if s.startTLS && !secure {
				lines = append(lines, "STARTTLS")
			}
			if s.auth != "" {
				lines = append(lines, "AUTH "+s.auth)
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				say("250" + sep + l)
			}
		case "STARTTLS":
			say("220 go ahead")
			secured := tls.Server(conn, tlsConfig)
			if secured.Handshake() != nil {
				return
			}
			conn, secure = secured, true
			r = bufio.NewReader(conn)
		case "AUTH":
			fields := strings.Fields(line)
			var user, password string
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				raw, _ := base64.StdEncoding.DecodeString(fields[2])
				parts := strings.Split(string(raw), "\x00")
				user, password = parts[1], parts[2]
			case "LOGIN":
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := read()
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := read()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(p)
				user, password = string(ub), string(pb)
			}
			s.mu.Lock()
			s.credentials = append(s.credentials, user+":"+password)
			s.mu.Unlock()
			say("235 2.7.0 Authentication successful")
		case "MAIL", "RCPT":
			say("250 ok")
		case "DATA":
			say("354 go ahead")
			var data strings.Builder
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				data.WriteString(l + "\n")
			}
			if reply, fails := s.replies["."]; fails {
				say(reply)
				continue
			}
			s.mu.Lock()
			s.messages = append(s.messages, data.String())
			s.mu.Unlock()
			say("250 ok: queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}
