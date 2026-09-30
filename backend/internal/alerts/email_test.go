package alerts

import (
	"aether/backend/internal/domain"
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP accepts every recipient except the ones in reject and records each message it receives.
type fakeSMTP struct {
	mu       sync.Mutex
	reject   map[string]bool
	messages []smtpMessage
}

type smtpMessage struct {
	rcpts []string
	data  string
}

func (f *fakeSMTP) serve(t *testing.T) (string, int) {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go f.session(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func (f *fakeSMTP) session(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(line string) { w.WriteString(line + "\r\n"); w.Flush() }
	say("220 fake")
	var cur smtpMessage
	for {
		line, e := r.ReadString('\n')
		if e != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			say("250 fake")
		case strings.HasPrefix(upper, "MAIL FROM"):
			cur = smtpMessage{}
			say("250 ok")
		case strings.HasPrefix(upper, "RCPT TO"):
			addr := strings.Trim(strings.TrimSpace(cmd[len("RCPT TO:"):]), "<>")
			f.mu.Lock()
			rejected := f.reject[addr]
			f.mu.Unlock()
			if rejected {
				say("550 no such user")
				continue
			}
			cur.rcpts = append(cur.rcpts, addr)
			say("250 ok")
		case upper == "RSET":
			cur = smtpMessage{}
			say("250 ok")
		case upper == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, e := r.ReadString('\n')
				if e != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			cur.data = b.String()
			f.mu.Lock()
			f.messages = append(f.messages, cur)
			f.mu.Unlock()
			say("250 queued")
		case upper == "QUIT":
			say("221 bye")
			return
		default:
			say("502 unknown")
		}
	}
}

// sent returns a copy of the recorded messages (the server goroutine writes them).
func (f *fakeSMTP) sent() []smtpMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMessage(nil), f.messages...)
}

func memberPayload() Payload {
	now := time.Now().UTC()
	return Payload{Text: "SOS", Alert: domain.Alert{Title: "SOS · ปุ่ม"}, SentAt: now}
}

// A "members" email is one message with a blind copy per member: the header names nobody, a rejected or
// malformed address is skipped, the others still get it.
func TestMemberEmailIsBlindCopied(t *testing.T) {
	fake := &fakeSMTP{reject: map[string]bool{"gone@example.test": true}}
	host, port := fake.serve(t)
	s := NewSender(SMTPSettings{Host: host, Port: port, From: "aether@example.test"}, "development")
	config := map[string]string{"audience": AudienceMembers, "to": "a@example.test,not an address,gone@example.test,b@example.test"}
	if e := s.sendEmail(context.Background(), config, memberPayload()); e != nil {
		t.Fatal(e)
	}
	messages := fake.sent()
	if len(messages) != 1 {
		t.Fatalf("one message expected: %+v", messages)
	}
	m := messages[0]
	if strings.Join(m.rcpts, ",") != "a@example.test,b@example.test" || !strings.Contains(m.data, "To: undisclosed-recipients:;\r\n") {
		t.Fatalf("the message must reach both members as blind copies: %+v", m)
	}
	for _, addr := range []string{"a@example.test", "b@example.test", "gone@example.test"} {
		if strings.Contains(m.data, addr) {
			t.Fatalf("the message names %s", addr)
		}
	}

	// Nobody accepted: the delivery fails so the worker retries it.
	fake.mu.Lock()
	fake.reject["a@example.test"] = true
	fake.reject["b@example.test"] = true
	fake.mu.Unlock()
	if e := s.sendEmail(context.Background(), config, memberPayload()); e == nil {
		t.Fatal("a members email nobody received was reported as sent")
	}

	// A hand-written list keeps its all-or-nothing behaviour.
	manual := map[string]string{"to": "c@example.test,a@example.test"}
	if e := s.sendEmail(context.Background(), manual, memberPayload()); e == nil {
		t.Fatal("a rejected address on a hand-written list must fail the delivery")
	}
	if got := recipients(map[string]string{"to": "c@example.test,not an address"}); got != nil {
		t.Fatalf("a malformed hand-written list must be refused: %v", got)
	}
}

// The members list is capped; the cap keeps the order the directory returned (owners first).
func TestMemberRecipientsCap(t *testing.T) {
	parts := []string{}
	for i := 0; i < MaxMemberRecipients+5; i++ {
		parts = append(parts, "m"+strconv.Itoa(i)+"@example.test")
	}
	got := recipients(map[string]string{"audience": AudienceMembers, "to": strings.Join(parts, ",")})
	if len(got) != MaxMemberRecipients || got[0] != "m0@example.test" {
		t.Fatalf("cap: %d %v", len(got), got[:1])
	}
}
