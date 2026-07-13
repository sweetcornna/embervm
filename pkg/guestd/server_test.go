package guestd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
)

func newTestServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(opts))
	t.Cleanup(srv.Close)
	return srv
}

func getHealth(t *testing.T, srv *httptest.Server) guestapi.HealthResponse {
	t.Helper()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	var h guestapi.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return h
}

func doExec(t *testing.T, srv *httptest.Server, req guestapi.ExecRequest) (int, guestapi.ExecResponse, guestapi.ErrorResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal exec request: %v", err)
	}
	resp, err := http.Post(srv.URL+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read exec response: %v", err)
	}
	var out guestapi.ExecResponse
	var errOut guestapi.ErrorResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode exec response %q: %v", raw, err)
		}
	} else {
		if err := json.Unmarshal(raw, &errOut); err != nil {
			t.Fatalf("decode exec error %q: %v", raw, err)
		}
	}
	return resp.StatusCode, out, errOut
}

func TestHealthSeqIncrements(t *testing.T) {
	srv := newTestServer(t, Options{Version: "test-1"})

	h1 := getHealth(t, srv)
	h2 := getHealth(t, srv)

	if !h1.OK || !h2.OK {
		t.Errorf("ok = %v, %v, want true, true", h1.OK, h2.OK)
	}
	if h1.Seq != 1 || h2.Seq != 2 {
		t.Errorf("seq = %d, %d, want 1, 2", h1.Seq, h2.Seq)
	}
	if h1.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", h1.PID, os.Getpid())
	}
	if h1.Version != "test-1" {
		t.Errorf("version = %q, want %q", h1.Version, "test-1")
	}
}

func postResumed(t *testing.T, srv *httptest.Server) guestapi.ResumedResponse {
	t.Helper()
	resp, err := http.Post(srv.URL+"/resumed", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /resumed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /resumed status = %d, want 200", resp.StatusCode)
	}
	var ack guestapi.ResumedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatalf("decode resumed ack: %v", err)
	}
	return ack
}

func TestResumedCounter(t *testing.T) {
	srv := newTestServer(t, Options{})
	if h := getHealth(t, srv); h.Resumes != 0 {
		t.Fatalf("fresh server resumes = %d, want 0", h.Resumes)
	}
	ack := postResumed(t, srv)
	if ack.Resumes != 1 || ack.HookRan {
		t.Fatalf("first resumed ack = %+v, want resumes=1 hook_ran=false", ack)
	}
	if ack = postResumed(t, srv); ack.Resumes != 2 {
		t.Fatalf("second resumed ack = %+v", ack)
	}
	if h := getHealth(t, srv); h.Resumes != 2 {
		t.Fatalf("healthz resumes = %d, want 2", h.Resumes)
	}
}

func TestExecStdout(t *testing.T) {
	srv := newTestServer(t, Options{})
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{Cmd: "sh", Args: []string{"-c", "printf hi"}})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
	if got := string(out.Stdout); got != "hi" {
		t.Errorf("stdout = %q, want %q", got, "hi")
	}
}

func TestExecExitCode(t *testing.T) {
	srv := newTestServer(t, Options{})
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{Cmd: "sh", Args: []string{"-c", "exit 3"}})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", out.ExitCode)
	}
}

func TestExecStdin(t *testing.T) {
	srv := newTestServer(t, Options{})
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "cat"}, Stdin: []byte("ping"),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := string(out.Stdout); got != "ping" {
		t.Errorf("stdout = %q, want %q", got, "ping")
	}
}

func TestExecCwd(t *testing.T) {
	srv := newTestServer(t, Options{})
	dir := t.TempDir()
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{Cmd: "sh", Args: []string{"-c", "pwd"}, Cwd: dir})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// The shell may report a logical or physical path (macOS symlinks
	// /var → /private/var); resolve both sides before comparing.
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out.Stdout)))
	if err != nil {
		t.Fatalf("EvalSymlinks(pwd output): %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	srv := newTestServer(t, Options{})
	start := time.Now()
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "sleep 5"}, TimeoutS: 1,
	})
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !out.TimedOut {
		t.Errorf("timed_out = false, want true")
	}
	if out.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", out.ExitCode)
	}
	// Well under the sleep duration: proves the group was killed, not waited.
	if elapsed >= 4*time.Second {
		t.Errorf("elapsed = %v, want < 4s (group kill failed?)", elapsed)
	}
}

func TestExecOutputTruncation(t *testing.T) {
	srv := newTestServer(t, Options{MaxOutputBytes: 64})
	status, out, _ := doExec(t, srv, guestapi.ExecRequest{
		Cmd: "sh", Args: []string{"-c", "head -c 1000 /dev/zero"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !out.Truncated {
		t.Errorf("truncated = false, want true")
	}
	if len(out.Stdout) != 64 {
		t.Errorf("len(stdout) = %d, want 64", len(out.Stdout))
	}
}

func TestExecMissingCmd(t *testing.T) {
	srv := newTestServer(t, Options{})
	status, _, errOut := doExec(t, srv, guestapi.ExecRequest{})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if errOut.Error == "" {
		t.Errorf("error message empty, want non-empty")
	}
}

func TestExecCmdNotFound(t *testing.T) {
	srv := newTestServer(t, Options{})
	status, _, errOut := doExec(t, srv, guestapi.ExecRequest{Cmd: "/nonexistent-embervm-binary"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if errOut.Error == "" {
		t.Errorf("error message empty, want non-empty")
	}
}

func TestFilesRoundtrip(t *testing.T) {
	srv := newTestServer(t, Options{})
	path := filepath.Join(t.TempDir(), "a", "b", "c.txt")

	req, err := http.NewRequest(http.MethodPut,
		srv.URL+"/files?path="+path+"&mode=0600", strings.NewReader("data"))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /files: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}

	getResp, err := http.Get(srv.URL + "/files?path=" + path)
	if err != nil {
		t.Fatalf("GET /files: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	body, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	if string(body) != "data" {
		t.Errorf("body = %q, want %q", body, "data")
	}
}

func TestFilesErrors(t *testing.T) {
	srv := newTestServer(t, Options{})
	dir := t.TempDir()

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing file", srv.URL + "/files?path=" + filepath.Join(dir, "missing"), http.StatusNotFound},
		{"relative path", srv.URL + "/files?path=relative/x", http.StatusBadRequest},
		{"directory", srv.URL + "/files?path=" + dir, http.StatusBadRequest},
		{"no path", srv.URL + "/files", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			var errOut guestapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errOut); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errOut.Error == "" {
				t.Errorf("error message empty, want non-empty")
			}
		})
	}
}

func TestListDir(t *testing.T) {
	srv := newTestServer(t, Options{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/files?op=list&path=" + dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	var out guestapi.ListDirResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(out.Entries), out.Entries)
	}
	// Directories first ("link" resolves to a directory), then files by name.
	byName := map[string]guestapi.DirEntry{}
	for _, e := range out.Entries {
		byName[e.Name] = e
	}
	if !out.Entries[0].IsDir || !out.Entries[1].IsDir {
		t.Errorf("directories not hoisted: %+v", out.Entries)
	}
	if e := byName["hello.txt"]; e.IsDir || e.Size != 2 || !strings.HasPrefix(e.Mode, "-") {
		t.Errorf("hello.txt = %+v", e)
	}
	if e := byName["sub"]; !e.IsDir {
		t.Errorf("sub = %+v", e)
	}
	if e := byName["link"]; !e.IsDir || e.Symlink != "sub" {
		t.Errorf("link = %+v (want dir via symlink target)", e)
	}
}

func TestListDirErrors(t *testing.T) {
	srv := newTestServer(t, Options{})
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{dir + "/missing", http.StatusNotFound},
		{file, http.StatusBadRequest}, // not a directory
	} {
		resp, err := http.Get(srv.URL + "/files?op=list&path=" + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("list %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	// op absent still reads the file (list must not change /files semantics).
	resp, err := http.Get(srv.URL + "/files?path=" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(raw) != "x" {
		t.Errorf("plain read = %d %q", resp.StatusCode, raw)
	}
}

func TestFilesPutModeInvalid(t *testing.T) {
	srv := newTestServer(t, Options{})
	path := filepath.Join(t.TempDir(), "f")
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/files?path=%s&mode=notoctal", srv.URL, path), strings.NewReader("x"))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /files: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
