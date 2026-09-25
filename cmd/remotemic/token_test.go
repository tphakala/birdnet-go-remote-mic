//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtcert"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
)

const (
	tokenOld = "existingtoken123"
	tokenNew = "replacement-token-456"
	// sentNone is the sentinel a fake appliance's recorded token holds before any
	// PATCH, distinct from the empty token a clear sends.
	sentNone = "UNSET"
)

func seedConfigWithToken(t *testing.T, path, token string) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Token = token
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
}

func loadToken(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg.Auth.Token
}

// stubStdin swaps the terminal seams: in is what stdin yields, tty whether it
// is a terminal, and secrets the successive hidden-prompt answers.
func stubStdin(t *testing.T, in string, tty bool, secrets ...string) {
	t.Helper()
	prevIn, prevTTY, prevRead := stdin, stdinIsTerminal, readSecret
	stdin = strings.NewReader(in)
	stdinIsTerminal = func() bool { return tty }
	readSecret = func() ([]byte, error) {
		if len(secrets) == 0 {
			return nil, errors.New("no more secrets")
		}
		s := secrets[0]
		secrets = secrets[1:]
		return []byte(s), nil
	}
	t.Cleanup(func() { stdin, stdinIsTerminal, readSecret = prevIn, prevTTY, prevRead })
}

// runCLI dispatches args and returns exit code, stdout, stderr.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = dispatch(args, &out, &errb)
	return code, out.String(), errb.String()
}

func tempConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml")
}

// --- token get ---

func TestTokenGetPrintsBareToken(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	code, out, errOut := runCLI("token", "get", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != tokenOld+"\n" || errOut != "" {
		t.Fatalf("stdout %q stderr %q, want only the bare token on stdout", out, errOut)
	}
}

func TestTokenGetNoTokenFails(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, "")
	code, out, errOut := runCLI("token", "get", flagConfig, path)
	if code != 1 || out != "" || !strings.Contains(errOut, "no access token") {
		t.Fatalf("exit %d stdout %q stderr %q, want exit 1, no stdout, a no-token error", code, out, errOut)
	}
}

func TestTokenGetMissingConfigFails(t *testing.T) {
	code, out, errOut := runCLI("token", "get", flagConfig, tempConfig(t))
	if code != 1 || out != "" || !strings.Contains(errOut, "no config file") {
		t.Fatalf("exit %d stdout %q stderr %q, want exit 1 and a missing-config error", code, out, errOut)
	}
}

// TestTokenGetUsesConfigEnv asserts $REMOTEMIC_CONFIG supplies the path when
// --config is absent.
func TestTokenGetUsesConfigEnv(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	t.Setenv(configEnv, path)
	code, out, errOut := runCLI("token", "get")
	if code != 0 || out != tokenOld+"\n" {
		t.Fatalf("exit %d stdout %q stderr %q, want the token from $%s", code, out, errOut, configEnv)
	}
}

// --- token generate ---

func TestTokenGenerateCreatesConfig(t *testing.T) {
	path := tempConfig(t)
	code, out, errOut := runCLI("token", "generate", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	token := strings.TrimSpace(out)
	if msg := auth.ValidToken(token); msg != "" {
		t.Fatalf("printed token invalid: %s", msg)
	}
	if got := loadToken(t, path); got != token {
		t.Fatalf("config token %q != printed token %q", got, token)
	}
	if !strings.Contains(errOut, "next starts") {
		t.Fatalf("stderr %q does not say the change applies at next start", errOut)
	}
}

func TestTokenGenerateQuietPrintsOnlyToken(t *testing.T) {
	code, out, errOut := runCLI("token", "generate", flagConfig, tempConfig(t), "-quiet")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr %q, want exit 0 and silent stderr", code, errOut)
	}
	if strings.Count(out, "\n") != 1 || auth.ValidToken(strings.TrimSpace(out)) != "" {
		t.Fatalf("quiet stdout is not one bare token: %q", out)
	}
}

func TestTokenGenerateRefusesExistingWithoutForce(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	code, out, errOut := runCLI("token", "generate", flagConfig, path)
	if code != 1 || out != "" || !strings.Contains(errOut, "--force") {
		t.Fatalf("exit %d stdout %q stderr %q, want a refusal pointing at --force", code, out, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("existing token modified: %q", got)
	}
}

func TestTokenGenerateForceReplaces(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	code, out, errOut := runCLI("token", "generate", flagConfig, path, "-force")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	token := strings.TrimSpace(out)
	if token == tokenOld || loadToken(t, path) != token {
		t.Fatalf("--force did not store the new token %q", token)
	}
}

func TestTokenGeneratePreservesDevices(t *testing.T) {
	path := tempConfig(t)
	cfg := config.Default()
	cfg.Devices = []config.Device{{
		Name: "m1", Device: "hw:1,0", Rate: 48000, Format: testFmtS16,
		Streams: []config.Stream{{Path: "/m1", Mode: config.ModeOpus, Channels: []int{1}}},
	}}
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if code, _, errOut := runCLI("token", "generate", flagConfig, path); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].Name != "m1" {
		t.Fatalf("devices not preserved: %+v", loaded.Devices)
	}
}

func TestTokenGenerateRejectsPositional(t *testing.T) {
	path := tempConfig(t)
	if code, _, _ := runCLI("token", "generate", flagConfig, path, "stray"); code != 2 {
		t.Fatalf("exit %d, want 2 (usage)", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config written despite rejected args (stat err = %v)", err)
	}
}

// --- token set ---

func TestTokenSetFromPipe(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, tokenNew+"\n", false)
	code, out, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
}

func TestTokenSetRejectsPositionalToken(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	// A valid token on stdin, so only the positional guard can stop the write.
	stubStdin(t, tokenNew+"\n", false)
	code, _, errOut := runCLI("token", "set", flagConfig, path, tokenNew)
	if code != 2 || !strings.Contains(errOut, "shell history") {
		t.Fatalf("exit %d stderr %q, want exit 2 and a refusal pointing at stdin", code, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

// TestTokenSetReturnsOnFirstLine proves token set reads only the first line of a
// piped token and returns on the newline, rather than consuming stdin to EOF. A
// reader that yields the line then blocks forever (an interactive ssh pipe held
// open after Enter) would hang a read-to-EOF; the deadline turns that into a
// failure instead of a hang.
func TestTokenSetReturnsOnFirstLine(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)

	pr, pw := io.Pipe()
	// Deliver the token line and then leave the writer open (never EOF).
	go func() { _, _ = io.WriteString(pw, tokenNew+"\n") }()

	prevIn, prevTTY := stdin, stdinIsTerminal
	stdin = pr
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdin, stdinIsTerminal = prevIn, prevTTY; _ = pr.Close() })

	done := make(chan int, 1)
	go func() { done <- runToken([]string{"set", flagConfig, path}, io.Discard, io.Discard) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("token set exit %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("token set blocked on stdin; it must return on the first line, not wait for EOF")
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
}

// TestTokenSetFromPipeNoTrailingNewline sets a token piped without a trailing
// newline (for example `printf %s tok | remote-mic token set`), which reaches
// EOF carrying data and no delimiter; the first-line read must still accept it.
func TestTokenSetFromPipeNoTrailingNewline(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, tokenNew, false) // no trailing newline
	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q, want 0", code, errOut)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
}

func TestTokenSetRejectsInvalid(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	for _, in := range []string{"short\n", "", "has space in it\n"} {
		stubStdin(t, in, false)
		if code, _, errOut := runCLI("token", "set", flagConfig, path); code != 1 {
			t.Errorf("input %q: exit %d stderr %q, want 1", in, code, errOut)
		}
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

func TestTokenSetPromptOnTerminal(t *testing.T) {
	path := tempConfig(t)
	stubStdin(t, "", true, tokenNew, tokenNew)
	if code, _, errOut := runCLI("token", "set", flagConfig, path); code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
}

func TestTokenSetPromptMismatch(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, "", true, tokenNew, tokenNew+"x")
	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "do not match") {
		t.Fatalf("exit %d stderr %q, want a mismatch error", code, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

// TestTokenSetPromptEmptyRejected asserts that empty or whitespace-only input at
// the hidden terminal prompt is refused rather than silently clearing the token
// (empty is a valid config state, so ValidToken does not catch it; token clear
// is the deliberate path to remove a token).
func TestTokenSetPromptEmptyRejected(t *testing.T) {
	for _, secret := range []string{"", "   "} {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		stubStdin(t, "", true, secret, secret)
		code, _, errOut := runCLI("token", "set", flagConfig, path)
		if code != 1 || !strings.Contains(errOut, "no token entered") {
			t.Errorf("secret %q: exit %d stderr %q, want exit 1 pointing at token clear", secret, code, errOut)
		}
		if got := loadToken(t, path); got != tokenOld {
			t.Errorf("secret %q: token changed to %q, want unchanged", secret, got)
		}
	}
}

// --- token clear ---

func TestTokenClearWithYes(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, "", false)
	if code, _, errOut := runCLI("token", "clear", flagConfig, path, "-yes"); code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if got := loadToken(t, path); got != "" {
		t.Fatalf("token = %q, want cleared", got)
	}
}

func TestTokenClearNonTerminalNeedsYes(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, "y\n", false)
	code, _, errOut := runCLI("token", "clear", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("exit %d stderr %q, want a refusal pointing at --yes", code, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

func TestTokenClearTerminalConfirm(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   string
		code   int
	}{
		{"y\n", "", 0},
		{"\n", tokenOld, 1},
		{"no\n", tokenOld, 1},
	} {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		stubStdin(t, tc.answer, true)
		code, _, errOut := runCLI("token", "clear", flagConfig, path)
		if code != tc.code {
			t.Errorf("answer %q: exit %d stderr %q, want %d", tc.answer, code, errOut, tc.code)
		}
		if got := loadToken(t, path); got != tc.want {
			t.Errorf("answer %q: token = %q, want %q", tc.answer, got, tc.want)
		}
	}
}

// lockProbeReader answers a prompt with reply, and the instant it is first read
// it checks whether the run lock for path is free, recording the result in freed.
type lockProbeReader struct {
	path  string
	freed *bool
	reply string
	done  bool
}

func (r *lockProbeReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if l, err := runlock.TryAcquire(runlock.PathFor(r.path)); err == nil {
		*r.freed = true
		if rerr := l.Release(); rerr != nil {
			return 0, rerr
		}
	}
	r.done = true
	return copy(p, r.reply), nil
}

// TestTokenClearPromptsWithoutHoldingLock asserts token clear runs its y/N
// confirmation before taking the run lock, so an appliance starting during the
// prompt is not blocked and does not falsely report "already running".
func TestTokenClearPromptsWithoutHoldingLock(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)

	var lockFreeDuringPrompt bool
	prevIn, prevTTY, prevRead := stdin, stdinIsTerminal, readSecret
	stdin = &lockProbeReader{path: path, freed: &lockFreeDuringPrompt, reply: "y\n"}
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdin, stdinIsTerminal, readSecret = prevIn, prevTTY, prevRead })

	code, _, errOut := runCLI("token", "clear", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if !lockFreeDuringPrompt {
		t.Fatal("run lock was held while the clear prompt was reading; a starting appliance would falsely see 'already running'")
	}
	if got := loadToken(t, path); got != "" {
		t.Fatalf("token = %q, want cleared", got)
	}
}

func TestTokenClearAlreadyOpenSucceeds(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, "")
	stubStdin(t, "", false)
	code, _, errOut := runCLI("token", "clear", flagConfig, path)
	if code != 0 || !strings.Contains(errOut, "already open") {
		t.Fatalf("exit %d stderr %q, want success noting it is already open", code, errOut)
	}
}

// TestTokenClearOpenMessage asserts token clear frames the security consequence
// by when it takes effect: a file edit with no live appliance says the surfaces
// open at the next start, while a live change says they are open now.
func TestTokenClearOpenMessage(t *testing.T) {
	t.Run("file edit says at next start", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		stubStdin(t, "", false)
		if code, _, errOut := runCLI("token", "clear", flagConfig, path, "-yes"); code != 0 {
			t.Fatalf("exit %d stderr %q", code, errOut)
		} else if !strings.Contains(errOut, "will be open once the appliance") {
			t.Fatalf("stderr %q, want the deferred-open message", errOut)
		}
	})
	t.Run("live change says now open", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		var sent string
		st := fakeAppliance(t, tokenOld, &sent)
		holdLock(t, path, &st)
		stubStdin(t, "", false)
		if code, _, errOut := runCLI("token", "clear", flagConfig, path, "-yes"); code != 0 {
			t.Fatalf("exit %d stderr %q", code, errOut)
		} else if !strings.Contains(errOut, "are now open to anyone") {
			t.Fatalf("stderr %q, want the now-open message", errOut)
		}
	})
}

// --- token group usage ---

func TestTokenUsageErrors(t *testing.T) {
	for _, args := range [][]string{{cmdToken}, {cmdToken, "rotate"}} {
		code, _, errOut := runCLI(args...)
		if code != 2 || !strings.Contains(errOut, "remote-mic token get") {
			t.Errorf("%v: exit %d stderr %q, want 2 with usage", args, code, errOut)
		}
	}
}

// --- running appliance ---

// holdLock takes the run lock for path as a running appliance would and
// publishes st (unless st is nil).
func holdLock(t *testing.T, cfgPath string, st *runlock.State) {
	t.Helper()
	l, err := runlock.TryAcquire(runlock.PathFor(cfgPath))
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Errorf("holdLock Release: %v", err)
		}
	})
	if st != nil {
		if err := l.Publish(*st); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
}

// fakeAppliance serves PATCH /api/v1/config over TLS like a running appliance,
// requiring bearer, and records the token it was sent. It returns the state to
// publish in the run lock.
func fakeAppliance(t *testing.T, bearer string, got *string) runlock.State {
	t.Helper()
	return fakeApplianceBody(t, bearer, got, `{"config":{},"restartRequired":false}`)
}

// fakeApplianceRestart is fakeAppliance but reports that a restart is still
// needed to finish applying the change (a 200 with restartRequired true).
func fakeApplianceRestart(t *testing.T, bearer string, got *string) runlock.State {
	t.Helper()
	return fakeApplianceBody(t, bearer, got, `{"config":{},"restartRequired":true}`)
}

// fakeApplianceBody is the shared fake: it requires bearer, records the token it
// was sent, and returns respBody (a 200 application/json PATCH answer).
func fakeApplianceBody(t *testing.T, bearer string, got *string, respBody string) runlock.State {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/config" {
			http.NotFound(w, r)
			return
		}
		if bearer != "" && r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var reqBody struct {
			Auth struct {
				Token *string `json:"token"`
			} `json:"auth"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil || reqBody.Auth.Token == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*got = *reqBody.Auth.Token
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return runlock.State{PID: 4242, MgmtAddr: srv.Listener.Addr().String(), CertPath: writeCertPEM(t, srv.Certificate().Raw)}
}

func writeCertPEM(t *testing.T, der []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), testCertFile)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return p
}

// TestTokenSetLiveGoesThroughAPI asserts that with an appliance running, the
// change is sent to its API with the current token as bearer, and the file is
// left for the appliance to write (not edited under it).
func TestTokenSetLiveGoesThroughAPI(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	var sent string
	st := fakeAppliance(t, tokenOld, &sent)
	holdLock(t, path, &st)
	stubStdin(t, tokenNew+"\n", false)

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if sent != tokenNew {
		t.Fatalf("API received token %q, want %q", sent, tokenNew)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("CLI edited the file under a running appliance: token %q", got)
	}
	if !strings.Contains(errOut, "applied it immediately") {
		t.Fatalf("stderr %q does not report a live change", errOut)
	}
}

// TestTokenLiveRejectsUnpinnedCertificate asserts the client refuses an API
// presenting a certificate other than the published one.
func TestTokenLiveRejectsUnpinnedCertificate(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	var sent string
	st := fakeAppliance(t, tokenOld, &sent)
	// Every httptest TLS server shares one built-in certificate, so pin a
	// freshly generated one instead.
	dir := t.TempDir()
	st.CertPath = filepath.Join(dir, testCertFile)
	if _, err := mgmtcert.Ensure(st.CertPath, filepath.Join(dir, "mgmt-key.pem"), certHostsFor("", nil)); err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	holdLock(t, path, &st)
	stubStdin(t, tokenNew+"\n", false)

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "presented a certificate other than") {
		t.Fatalf("exit %d stderr %q, want exit 1 naming the certificate mismatch", code, errOut)
	}
	if sent != "" {
		t.Fatalf("token %q reached a server with an unpinned certificate", sent)
	}
}

// TestTokenLiveUnauthorized asserts a 401 (file and appliance disagree) is
// reported and the file is left alone.
func TestTokenLiveUnauthorized(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	var sent string
	st := fakeAppliance(t, "the-token-in-memory", &sent)
	holdLock(t, path, &st)
	stubStdin(t, tokenNew+"\n", false)

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "disagree") {
		t.Fatalf("exit %d stderr %q, want a disagreement error", code, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

// TestTokenRunningWithoutManagementEditsFile asserts that an appliance running
// without its management API (no config writer) gets a file edit plus a
// restart notice.
func TestTokenRunningWithoutManagementEditsFile(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	holdLock(t, path, &runlock.State{PID: 4242})
	stubStdin(t, tokenNew+"\n", false)

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
	if !strings.Contains(errOut, "4242") || !strings.Contains(errOut, "restarts") {
		t.Fatalf("stderr %q lacks the restart notice for pid 4242", errOut)
	}
}

// TestTokenRunningStartingUp asserts a held lock with nothing published yet
// (the appliance is mid-startup) is a retryable error, not a file edit.
func TestTokenRunningStartingUp(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	holdLock(t, path, nil)
	stubStdin(t, tokenNew+"\n", false)

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "starting up, or another token command is using this config") {
		t.Fatalf("exit %d stderr %q, want a starting-up error", code, errOut)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token changed to %q", got)
	}
}

// TestAcquireRunLock covers the serve-side lock helper P2 introduced: a lock
// already held by another appliance is a fatal error naming the lock path, and
// a free lock is taken and returned so run() can load the config under it.
func TestAcquireRunLock(t *testing.T) {
	t.Run("held is fatal", func(t *testing.T) {
		path := tempConfig(t)
		holdLock(t, path, &runlock.State{PID: 4242})
		lock, err := acquireRunLock(path)
		if lock != nil {
			if rerr := lock.Release(); rerr != nil {
				t.Errorf("Release: %v", rerr)
			}
			t.Fatalf("acquireRunLock returned a lock while another holder was active")
		}
		if err == nil || !strings.Contains(err.Error(), runlock.PathFor(path)) {
			t.Fatalf("err = %v, want an error naming the lock path %s", err, runlock.PathFor(path))
		}
	})
	t.Run("free is taken", func(t *testing.T) {
		path := tempConfig(t)
		lock, err := acquireRunLock(path)
		if err != nil {
			t.Fatalf("acquireRunLock on a free lock: %v", err)
		}
		if lock == nil {
			t.Fatal("acquireRunLock returned a nil lock for a free path")
		}
		if err := lock.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	})
}

// TestTokenLiveVetoes asserts that with a live appliance, a command refused
// locally (an unconfirmed clear, or a generate over an existing token without
// --force) never reaches the management API, and a confirmed clear reaches it
// with an empty token and the current token as bearer.
func TestTokenLiveVetoes(t *testing.T) {
	t.Run("clear without --yes on a non-terminal does not PATCH", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		sent := sentNone
		st := fakeAppliance(t, tokenOld, &sent)
		holdLock(t, path, &st)
		stubStdin(t, "", false)
		if code, _, errOut := runCLI("token", "clear", flagConfig, path); code != 1 || !strings.Contains(errOut, "--yes") {
			t.Fatalf("exit %d stderr %q, want a refusal pointing at --yes", code, errOut)
		}
		if sent != sentNone {
			t.Fatalf("a refused clear reached the API (token %q)", sent)
		}
	})
	t.Run("clear --yes PATCHes an empty token with bearer tokenOld", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		sent := sentNone
		st := fakeAppliance(t, tokenOld, &sent)
		holdLock(t, path, &st)
		stubStdin(t, "", false)
		if code, _, errOut := runCLI("token", "clear", flagConfig, path, "-yes"); code != 0 {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
		// fakeAppliance requires bearer tokenOld (a 401 otherwise), so a recorded
		// send proves the bearer was tokenOld; the value is the cleared empty token.
		if sent != "" {
			t.Fatalf("API received token %q, want the empty (cleared) token", sent)
		}
		if got := loadToken(t, path); got != tokenOld {
			t.Fatalf("CLI edited the file under a running appliance: token %q", got)
		}
	})
	t.Run("generate without --force over an existing token does not PATCH", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, tokenOld)
		sent := sentNone
		st := fakeAppliance(t, tokenOld, &sent)
		holdLock(t, path, &st)
		if code, out, errOut := runCLI("token", "generate", flagConfig, path); code != 1 || out != "" || !strings.Contains(errOut, "--force") {
			t.Fatalf("exit %d stdout %q stderr %q, want a refusal pointing at --force", code, out, errOut)
		}
		if sent != sentNone {
			t.Fatalf("a refused generate reached the API (token %q)", sent)
		}
	})
	t.Run("clear when the file token is empty says already open without a PATCH", func(t *testing.T) {
		path := tempConfig(t)
		seedConfigWithToken(t, path, "")
		sent := sentNone
		st := fakeAppliance(t, "", &sent)
		holdLock(t, path, &st)
		stubStdin(t, "", false)
		if code, _, errOut := runCLI("token", "clear", flagConfig, path); code != 0 || !strings.Contains(errOut, "already open") {
			t.Fatalf("exit %d stderr %q, want success noting already open", code, errOut)
		}
		if sent != sentNone {
			t.Fatalf("an already-open clear reached the API (token %q)", sent)
		}
	})
}

// TestPatchError422 asserts a 422 validation problem surfaces each field and
// reason in the operator-facing error.
func TestPatchError422(t *testing.T) {
	errs := []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}{{Field: "auth.token", Reason: "must be 12-128 characters"}}
	resp := &mgmtapi.PatchConfigResponse{
		HTTPResponse:              &http.Response{StatusCode: http.StatusUnprocessableEntity},
		ApplicationproblemJSON422: &mgmtapi.ValidationProblem{Errors: &errs},
	}
	err := patchError(resp)
	if err == nil {
		t.Fatal("patchError returned nil for a 422")
	}
	if msg := err.Error(); !strings.Contains(msg, "auth.token") || !strings.Contains(msg, "must be 12-128 characters") {
		t.Fatalf("patchError = %q, want the field and reason", msg)
	}
}

// TestTokenLiveRestartRequired asserts a 200 whose restartRequired is true makes
// the CLI report that a restart is still needed to finish the change.
func TestTokenLiveRestartRequired(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	var sent string
	st := fakeApplianceRestart(t, tokenOld, &sent)
	holdLock(t, path, &st)
	stubStdin(t, tokenNew+"\n", false)
	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if sent != tokenNew {
		t.Fatalf("API received token %q, want %q", sent, tokenNew)
	}
	if !strings.Contains(errOut, "restart") || !strings.Contains(errOut, "needed to finish") {
		t.Fatalf("stderr %q, want a restart-needed notice", errOut)
	}
}

func TestDialAddr(t *testing.T) {
	const loopback = "127.0.0.1:9443"
	for in, want := range map[string]string{
		"[::]:9443":      loopback,
		"0.0.0.0:9443":   loopback,
		"[::]:9444":      "127.0.0.1:9444",
		":9445":          "127.0.0.1:9445",
		"10.0.0.5:9443":  "10.0.0.5:9443",
		"[::1]:9443":     "[::1]:9443",
		"127.0.0.1:9000": "127.0.0.1:9000",
		// A link-local address keeps its zone, with the "%" percent-escaped so the
		// resulting URL parses (RFC 6874).
		"[fe80::1%eth0]:9443": "[fe80::1%25eth0]:9443",
	} {
		if got := dialAddr(in); got != want {
			t.Errorf("dialAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLockStateAbsoluteCertPath asserts the published certificate path is
// absolute, so a token command run from another directory can read it.
func TestLockStateAbsoluteCertPath(t *testing.T) {
	st := lockState(7, "[::]:8443", testCertFile)
	if !filepath.IsAbs(st.CertPath) || filepath.Base(st.CertPath) != testCertFile {
		t.Fatalf("CertPath = %q, want an absolute path to mgmt-cert.pem", st.CertPath)
	}
	if st.PID != 7 || st.MgmtAddr != "[::]:8443" {
		t.Fatalf("state = %+v, want pid and address carried through", st)
	}
}

// stubEUID makes the write token commands see euid as their effective uid.
func stubEUID(t *testing.T, euid int) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return euid }
	t.Cleanup(func() { geteuid = prev })
}

// assertNoLockFiles fails if the run lock or the edit lock for cfgPath exists.
func assertNoLockFiles(t *testing.T, cfgPath string) {
	t.Helper()
	for _, p := range []string{runlock.PathFor(cfgPath), runlock.EditPathFor(cfgPath)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock file %s: stat err = %v, want it never created", p, err)
		}
	}
}

// TestTokenWriteRefusesOtherOwner asserts generate, set, and clear refuse,
// before creating any lock file or touching the config, when run as an account
// other than the config's owner (the root-owned lock case), naming the owner and
// the command to run instead.
func TestTokenWriteRefusesOtherOwner(t *testing.T) {
	// Each case is the token subcommand and its flags; the subcommand names it.
	cases := [][]string{
		{"generate", "--force"},
		{"set"},
		{"clear", "--yes"},
	}
	for _, tc := range cases {
		t.Run(tc[0], func(t *testing.T) {
			path := tempConfig(t)
			seedConfigWithToken(t, path, tokenOld)
			stubStdin(t, tokenNew+"\n", false)
			stubEUID(t, os.Geteuid()+1)

			args := append(append([]string{cmdToken}, tc...), flagConfig, path)
			code, stdout, errOut := runCLI(args...)
			if code != 1 {
				t.Fatalf("exit %d stderr %q, want 1", code, errOut)
			}
			// The suggested command keeps the operator's own flags (--force and
			// --yes change behavior) and ends with the absolute --config.
			wantCmd := "remote-mic token " + strings.Join(tc, " ") + " --config " + path
			if !strings.Contains(errOut, "is owned by") || !strings.Contains(errOut, ": sudo -u ") || !strings.Contains(errOut, wantCmd) {
				t.Fatalf("stderr %q, want an owner refusal suggesting %q", errOut, wantCmd)
			}
			if stdout != "" {
				t.Fatalf("stdout %q, want nothing", stdout)
			}
			if got := loadToken(t, path); got != tokenOld {
				t.Fatalf("token = %q, want %q unchanged", got, tokenOld)
			}
			assertNoLockFiles(t, path)
		})
	}
}

// TestTokenWriteOwnerMatchProceeds asserts a write command run as the config's
// owner is not refused.
func TestTokenWriteOwnerMatchProceeds(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubStdin(t, tokenNew+"\n", false)
	stubEUID(t, os.Geteuid())

	code, _, errOut := runCLI("token", "set", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q, want 0", code, errOut)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("token = %q, want %q", got, tokenNew)
	}
}

// TestTokenWriteMissingConfigChecksDirOwner asserts that with no config yet, the
// owner check falls back to the directory that will hold it: another account is
// refused without creating the config or a lock, and the directory's owner goes
// ahead and creates it.
func TestTokenWriteMissingConfigChecksDirOwner(t *testing.T) {
	path := tempConfig(t)
	// A group-writable directory, as a umask of 002 makes an ordinary private
	// one: it is still refused (only sticky or world-writable ones are shared).
	if err := os.Chmod(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	stubEUID(t, os.Geteuid()+1)
	code, _, errOut := runCLI("token", "generate", flagConfig, path)
	if code != 1 || !strings.Contains(errOut, "the directory "+filepath.Dir(path)) {
		t.Fatalf("exit %d stderr %q, want a refusal naming the directory", code, errOut)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config stat err = %v, want it not created", err)
	}
	assertNoLockFiles(t, path)

	stubEUID(t, os.Geteuid())
	code, stdout, errOut := runCLI("token", "generate", flagConfig, path)
	if code != 0 {
		t.Fatalf("exit %d stderr %q, want 0 as the directory owner", code, errOut)
	}
	if got := loadToken(t, path); got == "" || got != strings.TrimSpace(stdout) {
		t.Fatalf("saved token %q, printed %q, want the printed token saved", got, stdout)
	}
}

// TestTokenWriteMissingConfigDotDotAfterSymlink pins that the owner check looks
// at the directory the file actually lands in: lk points at real/sub, so
// lk/../config.yaml lands in real (private), not in the shared top directory a
// lexical clean of ".." would name.
func TestTokenWriteMissingConfigDotDotAfterSymlink(t *testing.T) {
	top := t.TempDir()
	if err := os.Chmod(top, 0o777); err != nil { // shared: would be exempt
		t.Fatal(err)
	}
	realDir := filepath.Join(top, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realDir, "sub"), filepath.Join(top, "lk")); err != nil {
		t.Fatal(err)
	}
	stubEUID(t, os.Geteuid()+1)
	// Relative, as typed at a shell, and a string: filepath.Join would clean it.
	t.Chdir(top)
	code, _, errOut := runCLI("token", "generate", flagConfig, "lk/../config.yaml")
	if code != 1 || !strings.Contains(errOut, "the directory "+realDir+" ") {
		t.Fatalf("exit %d stderr %q, want a refusal naming %s", code, errOut, realDir)
	}
}

// TestRerunCommand pins the owner-check hint's suggested command: the
// operator's flags survive in order, every --config spelling is replaced by
// the absolute path, and a word a shell would split or expand is quoted.
func TestRerunCommand(t *testing.T) {
	t.Parallel()
	const cfg = "/etc/rm/config.yaml"
	for _, tc := range []struct {
		name string
		args []string
		cfg  string
		want string
	}{
		{"flags kept", []string{"--force", "--config", cfg}, cfg, "remote-mic token generate --force --config " + cfg},
		{"equals form dropped", []string{"-config=/x.yaml", "-quiet"}, "/x.yaml", "remote-mic token generate -quiet --config /x.yaml"},
		{"double-dash equals form dropped", []string{"--config=/x.yaml", "-force"}, "/x.yaml", "remote-mic token generate -force --config /x.yaml"},
		{"terminator dropped", []string{"-force", "--"}, cfg, "remote-mic token generate -force --config " + cfg},
		{"no config flag", nil, cfg, "remote-mic token generate --config " + cfg},
		{"quoted path", nil, "/tmp/my dir/it's.yaml", `remote-mic token generate --config '/tmp/my dir/it'\''s.yaml'`},
	} {
		if got := rerunCommand("token generate", tc.args, tc.cfg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCheckOwnerParentEdgeCases pins the parent directory the owner check
// examines for a config not created yet at the edges of a path: a config at the
// root examines the root, and a bare name whose working directory is gone
// examines ".", never the root (which would tell the operator to run as root).
func TestCheckOwnerParentEdgeCases(t *testing.T) {
	if _, err := os.Stat("/remote-mic-no-such-config.yaml"); err == nil {
		t.Skip("a file of the test's chosen name exists at /")
	}
	stubEUID(t, os.Geteuid()+1)
	if os.Geteuid()+1 == 0 {
		t.Skip("the stubbed uid would be root's")
	}
	err := checkOwner("/remote-mic-no-such-config.yaml", "token generate", nil)
	if err == nil || !strings.Contains(err.Error(), "the directory / (") {
		t.Errorf("root-level config: err = %v, want a refusal naming /", err)
	}

	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Skip("this platform still resolves a removed working directory")
	}
	// The removed directory can still be examined through ".", so a refusal
	// naming its real owner is fine; naming the root is the defect.
	if err := checkOwner("config.yaml", "token generate", nil); err != nil && strings.Contains(err.Error(), "the directory / (") {
		t.Errorf("bare name with the working directory gone: err = %v, want no fallback to /", err)
	}
}

// TestTokenWriteMissingConfigSharedDirProceeds asserts that with no config yet
// in a shared directory (sticky or world-writable, such as /tmp), the
// directory's owner is not taken as the appliance account, so another account's
// command goes ahead and creates the config.
func TestTokenWriteMissingConfigSharedDirProceeds(t *testing.T) {
	for _, mode := range []os.FileMode{0o777 | os.ModeSticky, 0o700 | os.ModeSticky, 0o707} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatalf("chmod %v: %v", mode, err)
			}
			path := filepath.Join(dir, "config.yaml")
			stubEUID(t, os.Geteuid()+1)

			code, stdout, errOut := runCLI("token", "generate", flagConfig, path)
			if code != 0 {
				t.Fatalf("exit %d stderr %q, want 0 in a shared directory", code, errOut)
			}
			if got := loadToken(t, path); got == "" || got != strings.TrimSpace(stdout) {
				t.Fatalf("saved token %q, printed %q, want the printed token saved", got, stdout)
			}
		})
	}
}

// TestTokenGetIgnoresOwner asserts the read-only token get is not refused for
// an account other than the owner (one that can read the file, such as root).
func TestTokenGetIgnoresOwner(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubEUID(t, os.Geteuid()+1)

	code, stdout, errOut := runCLI("token", "get", flagConfig, path)
	if code != 0 || strings.TrimSpace(stdout) != tokenOld {
		t.Fatalf("exit %d stdout %q stderr %q, want the token", code, stdout, errOut)
	}
}

// stubEditLockWait sets how long saveToken waits for the edit lock. Tests that
// call it (or that depend on the default) are not parallel: the wait is global.
func stubEditLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := editLockWait
	editLockWait = d
	t.Cleanup(func() { editLockWait = prev })
}

// TestSaveTokenRefusesWhileEditLockHeld asserts a file edit takes the edit lock:
// with another holder and no wait, it fails with the retry message and leaves
// the config untouched.
func TestSaveTokenRefusesWhileEditLockHeld(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubEditLockWait(t, 0)
	release, err := runlock.LockEdits(path, 0)
	if err != nil {
		t.Fatalf("LockEdits: %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	err = saveToken(path, tokenNew, nil)
	if err == nil || !strings.Contains(err.Error(), "another token command is still editing") {
		t.Fatalf("saveToken while the edit lock is held: err = %v, want the still-editing error", err)
	}
	if got := loadToken(t, path); got != tokenOld {
		t.Fatalf("token = %q, want %q unchanged", got, tokenOld)
	}
}

// TestSaveTokenSerializesEdits asserts two concurrent file edits of one config
// run one after the other: the second's load and check start only after the
// first has saved, so it sees the first's token instead of both reading the old
// one and the last writer silently discarding the other's change.
func TestSaveTokenSerializesEdits(t *testing.T) {
	path := tempConfig(t)
	seedConfigWithToken(t, path, tokenOld)
	stubEditLockWait(t, time.Minute)

	const firstToken = "first-writer-token-1"
	inside := make(chan struct{})
	unblock := make(chan struct{})
	var unblocked atomic.Bool
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- saveToken(path, firstToken, func(string) error {
			close(inside)
			<-unblock
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-firstDone:
		t.Fatalf("first saveToken returned %v before running its check", err)
	}

	type seen struct {
		cur        string
		afterFirst bool
	}
	// Observe the second edit reaching the edit lock. The first already holds
	// it (its check runs inside), so once the second is attempting it, its own
	// check cannot run until the first releases: no timing window is needed. An
	// edit that skipped the lock would never signal and fails below.
	attempting := make(chan struct{})
	realLock := lockEdits
	lockEdits = func(p string, wait time.Duration) (func() error, error) {
		close(attempting)
		return realLock(p, wait)
	}
	t.Cleanup(func() { lockEdits = realLock })

	secondSaw := make(chan seen, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- saveToken(path, tokenNew, func(cur string) error {
			secondSaw <- seen{cur, unblocked.Load()}
			return nil
		})
	}()

	select {
	case <-attempting:
	case s := <-secondSaw:
		close(unblock)
		<-firstDone
		<-secondDone
		t.Fatalf("second edit ran its check (saw %q) without taking the edit lock", s.cur)
	case <-time.After(10 * time.Second): // a failure bound, not synchronization
		close(unblock)
		t.Fatal("second edit never attempted the edit lock")
	}
	unblocked.Store(true)
	close(unblock)

	if err := <-firstDone; err != nil {
		t.Fatalf("first saveToken: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second saveToken: %v", err)
	}
	s := <-secondSaw
	if !s.afterFirst {
		t.Fatal("second edit ran its check before the first was unblocked")
	}
	if s.cur != firstToken {
		t.Fatalf("second edit saw token %q, want the first edit's %q", s.cur, firstToken)
	}
	if got := loadToken(t, path); got != tokenNew {
		t.Fatalf("final token = %q, want the second edit's %q", got, tokenNew)
	}
}
