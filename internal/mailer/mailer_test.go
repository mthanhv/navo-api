package mailer

import (
	"encoding/base64"
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
