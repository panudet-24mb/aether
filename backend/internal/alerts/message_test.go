package alerts

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"unicode/utf8"
)

func reportMessage() Delivery {
	return Delivery{Event: "report", Subject: "รายงาน ห้องเย็น · 15 ต.ค. 2569", Text: "สรุป: ห้องเย็น 1 ปกติ", HTML: "<table><tr><td>ห้องเย็น 1</td></tr></table>",
		Attachment: &Attachment{Name: "aether-report-20261015.csv", ContentType: "text/csv; charset=utf-8", Data: []byte("\ufeffเวลา,n\r\n2026-10-15 00:00:00,12\r\n#END,1\r\n")}}
}

// decoded reads a base64 MIME part.
func decoded(t *testing.T, p *multipart.Part) string {
	t.Helper()
	if p.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatalf("part not base64: %v", p.Header)
	}
	b, e := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// A report email, sent through SMTP and parsed back: multipart/mixed with multipart/alternative (text, HTML) and the
// CSV as an attachment, the Thai subject encoded, every member a blind copy as for alerts.
func TestReportEmailStructure(t *testing.T) {
	fake := &fakeSMTP{reject: map[string]bool{}}
	host, port := fake.serve(t)
	s := NewSender(SMTPSettings{Host: host, Port: port, From: "aether@example.test"}, "development")
	ch := domain.NotificationChannel{Kind: "email", Config: map[string]string{"audience": AudienceMembers, "to": "a@example.test,b@example.test"}}
	m := reportMessage()
	if e := s.SendDelivery(context.Background(), ch, "", m); e != nil {
		t.Fatal(e)
	}
	sent := fake.sent()
	if len(sent) != 1 || strings.Join(sent[0].rcpts, ",") != "a@example.test,b@example.test" {
		t.Fatalf("one message to both members: %+v", sent)
	}
	msg, e := mail.ReadMessage(strings.NewReader(sent[0].data))
	if e != nil {
		t.Fatal(e)
	}
	if msg.Header.Get("To") != "undisclosed-recipients:;" {
		t.Fatalf("members are blind copies: %q", msg.Header.Get("To"))
	}
	subject, e := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if e != nil || subject != "[Aether] "+m.Subject {
		t.Fatalf("subject: %q %v", subject, e)
	}
	kind, params, e := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if e != nil || kind != "multipart/mixed" {
		t.Fatalf("top: %s %v", kind, e)
	}
	top := multipart.NewReader(msg.Body, params["boundary"])
	alt, e := top.NextPart()
	if e != nil {
		t.Fatal(e)
	}
	kind, altParams, _ := mime.ParseMediaType(alt.Header.Get("Content-Type"))
	if kind != "multipart/alternative" {
		t.Fatalf("first part: %s", kind)
	}
	inner := multipart.NewReader(alt, altParams["boundary"])
	for _, want := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		p, e := inner.NextPart()
		if e != nil {
			t.Fatal(e)
		}
		if k, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); k != want.kind || decoded(t, p) != want.body {
			t.Fatalf("%s part wrong: %v", want.kind, p.Header)
		}
	}
	if _, e := inner.NextPart(); e != io.EOF {
		t.Fatalf("alternative has more than two parts: %v", e)
	}
	att, e := top.NextPart()
	if e != nil {
		t.Fatal(e)
	}
	if att.FileName() != m.Attachment.Name || !strings.HasPrefix(att.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("attachment: %v", att.Header)
	}
	if csv := decoded(t, att); csv != string(m.Attachment.Data) || !strings.HasSuffix(csv, "#END,1\r\n") {
		t.Fatalf("attachment body: %q", csv)
	}
	if _, e := top.NextPart(); e != io.EOF {
		t.Fatalf("more parts than expected: %v", e)
	}
}

// A report webhook posts its own JSON with the alert path's signature header; a LINE text is cut to 5000 characters
// (not bytes, so Thai text is not cut in the middle of a character).
func TestReportWebhookAndLine(t *testing.T) {
	var got struct {
		event, signature string
		body             map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.event, got.signature = r.Header.Get("X-Aether-Event"), r.Header.Get("X-Aether-Signature")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
	}))
	defer srv.Close()
	s := NewSender(SMTPSettings{}, "development")
	m := reportMessage()
	m.Webhook = map[string]any{"schema": "aether.report.v1", "report": "ห้องเย็น"}
	if e := s.SendDelivery(context.Background(), domain.NotificationChannel{Kind: "webhook", Config: map[string]string{"url": srv.URL}}, "secret", m); e != nil {
		t.Fatal(e)
	}
	if got.event != "report" || !strings.HasPrefix(got.signature, "sha256=") || got.body["schema"] != "aether.report.v1" {
		t.Fatalf("webhook: %+v", got)
	}
	// SMTP not configured: an email is refused before any connection.
	if e := s.SendDelivery(context.Background(), domain.NotificationChannel{Kind: "email", Config: map[string]string{"to": "a@example.test"}}, "", m); e == nil {
		t.Fatal("email sent without SMTP")
	}
	// LINE counts UTF-16 code units: a Thai letter is one, 🍎 is two (and is never cut in half).
	if LineLength("ก🍎a") != 4 || LineLength(strings.Repeat("ก", MaxLineText)) != MaxLineText {
		t.Fatal("line length")
	}
	if cut := TruncateLine(strings.Repeat("ก", MaxLineText-1)+"🍎", MaxLineText); cut != strings.Repeat("ก", MaxLineText-1) {
		t.Fatalf("cut inside an emoji: %d units", LineLength(cut))
	}
	if cut := TruncateLine(strings.Repeat("🍎", MaxLineText), MaxLineText); LineLength(cut) != MaxLineText || !utf8.ValidString(cut) {
		t.Fatalf("emoji text: %d units", LineLength(cut))
	}
}
