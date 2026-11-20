package mailer

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildEncodesVietnameseAndRefusesHeaderInjection(t *testing.T) {
	from := &mail.Address{Name: "Nhật Duy SGN", Address: "sales@nhatduy.vn"}
	raw, err := Build(from, Message{ToName: "Trần Huy", ToEmail: "huy@khach.vn", ReplyToName: "Khoa", ReplyToEmail: "khoa@nhatduy.vn",
		Subject: "Giới thiệu sản phẩm mới", Text: "Kính gửi Anh/Chị Trần Huy,\n\nVan cầu DN80"}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Reply-To"); !strings.Contains(got, "khoa@nhatduy.vn") {
		t.Fatalf("reply-to: %q", got)
	}
	if strings.Contains(msg.Header.Get("Subject"), "Giới") {
		t.Fatal("subject must be encoded, not raw UTF-8")
	}
	body := make([]byte, 4096)
	n, _ := msg.Body.Read(body)
	text, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(body[:n]), "\r\n", ""))
	if err != nil || !strings.Contains(string(text), "Van cầu DN80") {
		t.Fatalf("body: %q %v", text, err)
	}
	if _, err := Build(from, Message{ToEmail: "a@b.vn", Subject: "x\r\nBcc: evil@x.vn"}, time.Now()); err == nil {
		t.Fatal("a line break in the subject must be refused")
	}
}

func TestNewIsOffUntilConfigured(t *testing.T) {
	if s, err := New(Config{}); err != nil || s.Enabled() {
		t.Fatalf("empty config: enabled=%v err=%v", s.Enabled(), err)
	}
	if _, err := New(Config{Host: "smtp.x.vn"}); err == nil {
		t.Fatal("host without From must be refused")
	}
	if s, err := New(Config{Host: "smtp.x.vn", From: "Nhật Duy <sales@x.vn>"}); err != nil || !s.Enabled() || s.From() != "sales@x.vn" {
		t.Fatalf("configured: %v %v", s, err)
	}
}

// IPv4-mapped loopback is deliberately outside isLocal's automatic exceptions:
// the same plaintext SMTP exchange must be rejected unless explicitly allowed.
func TestPlaintextSMTPRequiresOptIn(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					received <- ""
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprint(conn, "220 capture ESMTP\r\n")
				reader := bufio.NewReader(conn)
				var body strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						received <- body.String()
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						fmt.Fprint(conn, "250 capture\r\n")
					case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
						fmt.Fprint(conn, "250 OK\r\n")
					case line == "DATA\r\n":
						fmt.Fprint(conn, "354 Send message\r\n")
						for {
							line, err = reader.ReadString('\n')
							if err != nil || line == ".\r\n" {
								break
							}
							body.WriteString(line)
						}
						fmt.Fprint(conn, "250 Captured\r\n")
					case line == "QUIT\r\n":
						fmt.Fprint(conn, "221 Bye\r\n")
						received <- body.String()
						return
					default:
						received <- body.String()
						return
					}
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			sender, err := New(Config{Host: "::ffff:127.0.0.1", Port: port, From: "navo@example.test", AllowInsecure: allow})
			if err != nil {
				t.Fatal(err)
			}
			err = sender.Send(context.Background(), Message{ToEmail: "customer@example.test", Subject: "Mailpit smoke test", Text: "Hello"})
			raw := <-received
			if allow {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(raw, "Subject: Mailpit smoke test") {
					t.Fatalf("message not captured: %q", raw)
				}
			} else if err == nil || !strings.Contains(err.Error(), "does not offer TLS") || raw != "" {
				t.Fatalf("plaintext must be rejected before DATA: err=%v message=%q", err, raw)
			}
		})
	}
}
