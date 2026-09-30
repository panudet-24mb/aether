package cloudkeys

import (
	"aether/backend/internal/security"
	"crypto/rand"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	var priv [32]byte
	if _, e := rand.Read(priv[:]); e != nil {
		t.Fatal(e)
	}
	k, e := NewKey(&priv)
	if e != nil {
		t.Fatal(e)
	}
	return k
}

func TestSealedForTheWorkerOnly(t *testing.T) {
	worker := newKey(t)
	sealed, e := security.SealTuyaCloud(worker.Public(), "tenant-a", "gateway-a", "abcdefgh12345678", "secretsecretsecretsecret12345678")
	if e != nil {
		t.Fatal(e)
	}
	c, e := Open("tenant-a", "gateway-a", sealed, newKey(t), worker)
	if e != nil || c.AccessID != "abcdefgh12345678" || c.AccessSecret != "secretsecretsecretsecret12345678" {
		t.Fatalf("open: %+v %v", c, e)
	}
	// The binding is inside the box: another tenant or gateway (or a shifted separator) does not open.
	for _, bad := range [][2]string{{"tenant-b", "gateway-a"}, {"tenant-a", "gateway-b"}, {"tenant-agateway-a", ""}, {"", "tenant-a\x00gateway-a"}} {
		if _, e := Open(bad[0], bad[1], sealed, worker); e == nil {
			t.Fatalf("opened for %q", bad)
		}
	}
	// Nothing the API holds opens it: the public key itself, and every symmetric key it can derive from its
	// master keys (used as a symmetric key or as an X25519 private key).
	master := []byte("the API's JWT signing key, 32 bytes or more ...")
	apiKeys := [][]byte{worker.Public()[:]}
	for _, purpose := range []string{"tuya-cloud-credentials", "notification-channels", "tuya-local-keys"} {
		apiKeys = append(apiKeys, security.DeriveKey(master, purpose))
	}
	for _, k := range apiKeys {
		if _, e := security.Open(k, sealed); e == nil {
			t.Fatal("opened with a symmetric key")
		}
		var priv [32]byte
		copy(priv[:], k)
		asPrivate, _ := NewKey(&priv)
		if _, e := Open("tenant-a", "gateway-a", sealed, asPrivate); e == nil {
			t.Fatal("opened with a key the API holds")
		}
	}
	for _, junk := range []string{"", "!!", "YWJj", base64.StdEncoding.EncodeToString(make([]byte, 200))} {
		if _, e := Open("tenant-a", "gateway-a", junk, worker); e == nil {
			t.Fatalf("junk %q opened", junk)
		}
	}
	if strings.Contains(sealed, "abcdefgh12345678") {
		t.Fatal("sealed value carries the access id")
	}
	// Without a public key nothing is sealed; keys must be 32 bytes.
	if _, e := security.SealTuyaCloud(nil, "t", "g", "abcdefgh12345678", "x"); e == nil {
		t.Fatal("sealed without a key")
	}
	for _, raw := range []string{"", "c2hvcnQ=", "not base64"} {
		if _, e := ParseKey(raw); e == nil {
			t.Fatalf("key %q accepted", raw)
		}
	}
	round, e := ParseKey(base64.StdEncoding.EncodeToString(worker.private[:]))
	if e != nil || *round.Public() != *worker.Public() {
		t.Fatalf("parsed key: %v", e)
	}
}

// Only the tuya-cloud worker may open credentials: no other binary links this package or the worker.
func TestOnlyTheWorkerImportsTheOpener(t *testing.T) {
	out, e := exec.Command("go", "list", "-f", "{{.ImportPath}}", "aether/backend/cmd/...").Output()
	if e != nil {
		t.Fatal(e)
	}
	checked := 0
	for _, cmd := range strings.Fields(string(out)) {
		if cmd == "aether/backend/cmd/tuya-cloud" {
			continue
		}
		deps, e := exec.Command("go", "list", "-deps", cmd).Output()
		if e != nil {
			t.Fatalf("%s: %v", cmd, e)
		}
		for _, d := range strings.Fields(string(deps)) {
			if strings.HasPrefix(d, "aether/backend/internal/tuyacloudlink") {
				t.Fatalf("%s imports %s", cmd, d)
			}
		}
		checked++
	}
	if checked == 0 || !strings.Contains(string(out), "aether/backend/cmd/api") {
		t.Fatalf("cmd/api not checked: %q", out)
	}
}
