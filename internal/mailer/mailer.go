// Package mailer sends plain-text email through the company's SMTP server. It is off
// until the server and the From address are configured, like the AI drafts.
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Message is one email to one person.
type Message struct {
	ToName  string
	ToEmail string
	// ReplyToName/ReplyToEmail is the person who wrote it, so answers reach them directly.
	ReplyToName  string
	ReplyToEmail string
	Subject      string
	Text         string
}

// Sender delivers messages. Enabled is false when email is not set up.
type Sender interface {
	Enabled() bool
	// From is the address customers see, for display.
	From() string
	Send(ctx context.Context, m Message) error
}

// Config is the SMTP account Navo sends from.
type Config struct {
	Host     string
	Port     string
	Username string
	Password string
	// From is "Company name <address@company.vn>" or a bare address.
	From string
}

// SMTP sends through one SMTP account.
type SMTP struct {
	cfg  Config
	from *mail.Address
}

// New returns a sender; it is disabled unless host and a valid From address are set.
func New(cfg Config) (*SMTP, error) {
	s := &SMTP{cfg: cfg}
	if cfg.Host == "" && cfg.From == "" {
		return s, nil
	}
	if cfg.Host == "" || cfg.From == "" {
		return s, errors.New("NAVO_SMTP_HOST and NAVO_MAIL_FROM must be set together")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return s, fmt.Errorf("NAVO_MAIL_FROM: %w", err)
	}
	s.from = from
	if s.cfg.Port == "" {
		s.cfg.Port = "587"
	}
	return s, nil
}

// Enabled reports whether email is set up.
func (s *SMTP) Enabled() bool { return s != nil && s.from != nil }

// From is the sending address.
func (s *SMTP) From() string {
	if !s.Enabled() {
		return ""
	}
	return s.from.Address
}

// Send delivers one message, with TLS (implicit on 465, STARTTLS otherwise).
func (s *SMTP) Send(ctx context.Context, m Message) error {
	if !s.Enabled() {
		return errors.New("email is not set up")
	}
	raw, err := Build(s.from, m, time.Now())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.cfg.Host, s.cfg.Port)
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if s.cfg.Port == "465" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(45 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.cfg.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if !isLocal(s.cfg.Host) {
			return errors.New("the mail server does not offer TLS")
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.ToEmail); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func isLocal(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// Build renders the message as RFC 5322 text: UTF-8, base64 body, no line breaks in headers.
func Build(from *mail.Address, m Message, now time.Time) ([]byte, error) {
	for _, v := range []string{m.ToName, m.ToEmail, m.ReplyToName, m.ReplyToEmail, m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("line break in an email header")
		}
	}
	if _, err := mail.ParseAddress(m.ToEmail); err != nil {
		return nil, fmt.Errorf("recipient address: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	var b strings.Builder
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", (&mail.Address{Name: m.ToName, Address: m.ToEmail}).String())
	if m.ReplyToEmail != "" {
		header("Reply-To", (&mail.Address{Name: m.ReplyToName, Address: m.ReplyToEmail}).String())
	}
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return []byte(b.String()), nil
}
