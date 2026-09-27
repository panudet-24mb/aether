package edgeinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testImage = "ghcr.io/panudet-24mb/aether-edge:0.1.0"
	testZ2M   = "ghcr.io/koenkk/zigbee2mqtt:2.14.1"
	testCode  = "ABCDEFGHJKLMNPQRSTUVWXYZ23"
)

func render(t *testing.T) string {
	t.Helper()
	s, e := Script("https://aether.example.com", testImage, testZ2M)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "install.sh")
	if e := os.WriteFile(path, []byte(s), 0o600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestScript(t *testing.T) {
	s, e := Script("https://aether.example.com", testImage, testZ2M)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"#!/bin/sh", "ORIGIN='https://aether.example.com'", "IMAGE='" + testImage + "'", "Z2M_IMAGE='" + testZ2M + "'",
		"set -eu", "--cap-drop ALL", "</dev/tty"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "__") {
		t.Fatal("placeholder left in the script")
	}
	digest := testImage + "@sha256:" + strings.Repeat("a", 64)
	if _, e := Script("https://aether.example.com", digest, testZ2M); e != nil {
		t.Fatalf("digest-pinned image refused: %v", e)
	}
	for _, bad := range []struct{ origin, image string }{
		{"https://aether.example.com/path", testImage},
		{"https://aether.example.com'; rm -rf /; '", testImage},
		{"ftp://aether.example.com", testImage},
		{"https://aether.example.com", "ghcr.io/x/y"},
		{"https://aether.example.com", "ghcr.io/x/y:1'"},
		{"https://aether.example.com", testImage + "@sha256:short"},
	} {
		if _, e := Script(bad.origin, bad.image, testZ2M); e == nil {
			t.Fatalf("accepted %q %q", bad.origin, bad.image)
		}
	}
	if out, e := exec.Command("sh", "-n", render(t)).CombinedOutput(); e != nil {
		t.Fatalf("sh -n: %v\n%s", e, out)
	}
}

// Argument handling, run for real without Docker: bad input is refused before Docker is touched.
func TestScriptArguments(t *testing.T) {
	path := render(t)
	run := func(args ...string) string {
		cmd := exec.Command("sh", append([]string{path}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		if _, e := exec.LookPath("docker"); e == nil {
			cmd.Env = []string{"PATH=/nonexistent-docker-free:/usr/bin:/bin"}
		}
		cmd.Dir = t.TempDir()
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	cases := map[string]string{
		"--zigbee":                              "usage",
		testCode:                                "not given on the command line",
		"--zigbee -rf":                          "--zigbee must be",
		"--zigbee-ui 192.168.1.5":               "needs --zigbee",
		"--zigbee 10.0.0.9 --zigbee-ui 0.0.0.0": "not 0.0.0.0",
		"--dir relative":                        "absolute path",
		"--dir /opt/x;rm":                       "unsupported characters",
		"--dir /":                               "not a safe place",
		"--dir /etc":                            "system directory",
		"--dir /etc/aether":                     "system directory",
		"--dir /usr/lib/x":                      "system directory",
		"--dir /var":                            "not a safe place",
		"--dir /root":                           "not a safe place",
		"--dir /home":                           "not a safe place",
		"--dir /home/":                          "not a safe place",
		"--dir /opt/../etc":                     "must not contain ..",
		"--dir /boot/efi":                       "system directory",
		"--dir /dev/shm":                        "system directory",
	}
	for args, want := range cases {
		if out := run(strings.Fields(args)...); !strings.Contains(out, want) {
			t.Fatalf("%q: got %q, want %q", args, out, want)
		}
	}
	// Accepted directories get as far as the Docker check.
	for _, dir := range []string{"/opt/aether-edge", "/srv/edge", "/usr/local/aether-edge", "/home/pi/aether-edge"} {
		if out := run("--dir", dir); !strings.Contains(out, "Docker is not installed") {
			t.Fatalf("--dir %s: %q", dir, out)
		}
	}
}

// With a fake docker: the image is pulled before the code is read, the code reaches `docker run` only through the
// environment (never an argument), and --update moves both pinned images in .env.
func TestScriptFlowWithFakeDocker(t *testing.T) {
	path := render(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "docker.log")
	fake := `#!/bin/sh
{ printf 'ARGS:'; for a in "$@"; do printf ' [%s]' "$a"; done; printf ' CODE_ENV=%s\n' "${AETHER_INSTALL_CODE:-}"; } >>"` + log + `"
exit 0
`
	if e := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(t.TempDir(), "edge")
	cmd := exec.Command("sh", path, "--dir", dir, "--zigbee", "192.168.1.40", "--zigbee-ui", "192.168.1.5")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "AETHER_INSTALL_CODE=" + testCode}
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("install: %v\n%s", e, out)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pull, run := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "[pull] ["+testImage+"]") && pull < 0 {
			pull = i
		}
		if strings.Contains(l, "[run]") {
			run = i
			if strings.Contains(strings.SplitN(l, "CODE_ENV=", 2)[0], testCode) {
				t.Fatalf("the code is an argument of docker run: %s", l)
			}
			if !strings.HasSuffix(l, "CODE_ENV="+testCode) {
				t.Fatalf("docker run does not get the code in its environment: %s", l)
			}
			for _, want := range []string{"[--zigbee] [192.168.1.40]", "[--zigbee-ui] [192.168.1.5]", "[--z2m-image] [" + testZ2M + "]", "[--cap-drop] [ALL]"} {
				if !strings.Contains(l, want) {
					t.Fatalf("docker run lacks %s: %s", want, l)
				}
			}
		}
		if !strings.Contains(l, "[run]") && strings.Contains(l, testCode) {
			t.Fatalf("the code reached another docker call: %s", l)
		}
	}
	if pull < 0 || run < 0 || pull > run {
		t.Fatalf("expected pull before run:\n%s", b)
	}
	// --update rewrites both images in .env.
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("EDGE_IMAGE=old:1\nZ2M_IMAGE=old-z2m:1\nGATEWAY_ID=x\n"), 0o600)
	cmd = exec.Command("sh", path, "--update", "--dir", dir)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("update: %v\n%s", e, out)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	for _, want := range []string{"EDGE_IMAGE=" + testImage, "Z2M_IMAGE=" + testZ2M, "GATEWAY_ID=x"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Fatalf(".env after update lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "old") {
		t.Fatalf("old image kept:\n%s", env)
	}
}
