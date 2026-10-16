package alerts

import (
	"aether/backend/internal/domain"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf16"
)

// Delivery is a message that is not an alert (a scheduled report, docs/platform/reports.md): one rendering per channel
// kind. Alerts keep their own path (Send); this one shares the channel rules, the webhook signing, the LINE push and
// the SMTP session with it.
type Delivery struct {
	Event      string // X-Aether-Event of a webhook
	Subject    string
	Text       string // LINE, and the plain-text part of an email
	HTML       string // the HTML part of an email
	Attachment *Attachment
	Webhook    any // the JSON body of a webhook
}

// Attachment is one file attached to an email.
type Attachment struct {
	Name, ContentType string
	Data              []byte
}

// MaxLineText is LINE's limit for one text message. LINE counts it in UTF-16 code units, not characters: a Thai
// letter is one, an emoji outside the Basic Multilingual Plane two
// (https://developers.line.biz/en/docs/messaging-api/text-character-count/).
const MaxLineText = 5000

// LineLength is the length of s as LINE counts it (UTF-16 code units).
func LineLength(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++ // an invalid rune is sent as U+FFFD, one unit
		}
	}
	return n
}

// TruncateLine cuts s to at most max UTF-16 code units, never inside a character.
func TruncateLine(s string, max int) string {
	n := 0
	for i, r := range s {
		l := utf16.RuneLen(r)
		if l <= 0 {
			l = 1
		}
		if n+l > max {
			return s[:i]
		}
		n += l
	}
	return s
}

// SendDelivery delivers m to one channel; the returned error is safe to store (no secrets).
func (s *Sender) SendDelivery(ctx context.Context, ch domain.NotificationChannel, secret string, m Delivery) error {
	switch ch.Kind {
	case "webhook":
		body, e := json.Marshal(m.Webhook)
		if e != nil {
			return errors.New("webhook payload invalid")
		}
		return s.postWebhook(ctx, ch.Config["url"], secret, m.Event, body)
	case "line":
		return s.pushLine(ctx, secret, ch.Config["to"], TruncateLine(m.Text, MaxLineText))
	case "email":
		now := time.Now()
		return s.mail(ctx, ch.Config, func(to string) []byte { return BuildMIME(s.SMTP.From, to, now, m) })
	}
	return domain.ErrInvalid
}

// headerText keeps a header value on one line and encodes it for non-ASCII text (RFC 2047).
func headerText(s string) string {
	return mime.BEncoding.Encode("UTF-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
}

// base64Lines writes data as base64 in lines of 76 characters (RFC 2045).
func base64Lines(w *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		w.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	w.WriteString(enc + "\r\n")
}

// BuildMIME renders m as one email: multipart/mixed holding a multipart/alternative (text, then HTML) and the
// attachment, every part base64 so any byte of Thai text or CSV survives 7-bit transport.
func BuildMIME(from, to string, at time.Time, m Delivery) []byte {
	var out bytes.Buffer
	mixed := multipart.NewWriter(&out)
	var head bytes.Buffer
	fmt.Fprintf(&head, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n",
		from, to, headerText("[Aether] "+m.Subject), at.Format(time.RFC1123Z), mixed.Boundary())

	var alt bytes.Buffer
	altw := multipart.NewWriter(&alt)
	for _, part := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		if part.body == "" {
			continue
		}
		w, _ := altw.CreatePart(textproto.MIMEHeader{"Content-Type": {part.kind + "; charset=UTF-8"}, "Content-Transfer-Encoding": {"base64"}})
		var b bytes.Buffer
		base64Lines(&b, []byte(part.body))
		_, _ = w.Write(b.Bytes())
	}
	_ = altw.Close()
	w, _ := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {fmt.Sprintf("multipart/alternative; boundary=%q", altw.Boundary())}})
	_, _ = w.Write(alt.Bytes())
	if a := m.Attachment; a != nil {
		name := mime.QEncoding.Encode("UTF-8", a.Name)
		w, _ := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {fmt.Sprintf("%s; name=%q", a.ContentType, name)},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", name)},
			"Content-Transfer-Encoding": {"base64"},
		})
		var b bytes.Buffer
		base64Lines(&b, a.Data)
		_, _ = w.Write(b.Bytes())
	}
	_ = mixed.Close()
	return append(head.Bytes(), out.Bytes()...)
}
