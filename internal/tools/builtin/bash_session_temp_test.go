package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"reasonix/internal/base/testenv"
	"reasonix/internal/safety/sandbox"
	"reasonix/internal/state/sessiontemp"
)

func TestBashSharesSessionTempAcrossCalls(t *testing.T) {
	type shellCase struct {
		name  string
		shell sandbox.Shell
	}
	shells := []shellCase{
		{name: "default"},
	}
	if runtime.GOOS == "windows" {
		// The default on Windows prefers Git Bash when installed. Exercise native
		// PowerShell explicitly as well so both supported shell modes prove the
		// same session-temp contract.
		powerShell := sandbox.ResolveShell("powershell", "", nil)
		if powerShell.Kind != sandbox.ShellPowerShell {
			t.Fatal("PowerShell is required for the Windows session-temp regression")
		}
		shells = append(shells, shellCase{name: "powershell", shell: powerShell})
	}

	for _, tc := range shells {
		t.Run(tc.name, func(t *testing.T) {
			m := sessiontemp.NewWithRoot(testenv.TempDir(t))
			m.Retain()
			defer m.Release()

			b := bash{
				sb:          sandbox.Spec{Mode: "off"},
				shell:       tc.shell,
				workDir:     testenv.TempDir(t),
				sessionTemp: m,
			}
			// Pin the lazily resolved default so command syntax follows the shell
			// actually selected, rather than assuming every Windows host uses
			// PowerShell (Git Bash is preferred when present).
			b.shell = b.resolved()

			marker := "reasonix-session-temp-share"
			writeCmd := `test "$TMPDIR" = "$TMP" && test "$TMPDIR" = "$TEMP" && printf '%s' shared > "${TMPDIR:?}/` + marker + `"`
			readCmd := `cat "${TMPDIR:?}/` + marker + `"`
			if b.shell.Kind == sandbox.ShellPowerShell {
				writeCmd = `if (($env:TMPDIR -ne $env:TMP) -or ($env:TMPDIR -ne $env:TEMP)) { throw 'temporary environment variables differ' }; Set-Content -Path (Join-Path $env:TEMP '` + marker + `') -Value 'shared' -NoNewline`
				readCmd = `Get-Content -Raw (Join-Path $env:TEMP '` + marker + `')`
			}
			if _, err := b.Execute(context.Background(), argsJSON(t, map[string]any{"command": writeCmd})); err != nil {
				t.Fatalf("write: %v", err)
			}

			out, err := b.Execute(context.Background(), argsJSON(t, map[string]any{"command": readCmd}))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !strings.Contains(out, "shared") {
				t.Fatalf("second bash call did not see first temp file: %q", out)
			}

			dir := m.Dir()
			if dir == "" {
				t.Fatal("manager has no generation after use")
			}
			body, err := os.ReadFile(filepath.Join(dir, marker))
			if err != nil || string(body) != "shared" {
				t.Fatalf("host private dir content = %q err=%v", body, err)
			}
		})
	}
}

// MSYS resolves /tmp once from the first process's TEMP/TMP and keeps it in a
// mount table shared for the runtime's lifetime, so a shell that starts on a
// session directory keeps pointing /tmp at a deleted path after the session
// ends (#15023). The shell process must start on the host temp instead, and the
// command must still see the session directory.
func TestWindowsBashStartsOnHostTemp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the MSYS /tmp mount is Windows-only")
	}
	m := sessiontemp.NewWithRoot(testenv.TempDir(t))
	m.Retain()
	defer m.Release()

	b := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     testenv.TempDir(t),
		sessionTemp: m,
	}
	b.shell = b.resolved()
	if b.shell.Kind != sandbox.ShellBash {
		t.Skip("no bash on this host")
	}

	prepared, _, lease, err := b.prepareLaunch(context.Background(), b.shell, bashParams{Command: "echo ok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	for _, kv := range prepared.EnvOverrides {
		key, _, _ := strings.Cut(kv, "=")
		if key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			t.Fatalf("bash process env still carries a session temp entry: %q", kv)
		}
	}
	dir := lease.Dir()
	posix := "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	script := prepared.Argv[len(prepared.Argv)-1]
	prelude := "export TMPDIR='" + posix + "' TMP='" + posix + "' TEMP='" + posix + "'\n"
	if !strings.HasPrefix(script, prelude) {
		t.Fatalf("script does not re-export the session temp first:\n%s", script)
	}
	if !strings.Contains(script, "echo ok") {
		t.Fatalf("script lost the command:\n%s", script)
	}
}

// The re-export is a shell literal, so a session directory that contains a
// quote must still reach the command unchanged.
func TestWindowsBashSessionTempSurvivesQuotedPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the MSYS /tmp mount is Windows-only")
	}
	root := filepath.Join(testenv.TempDir(t), "it's private")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m := sessiontemp.NewWithRoot(root)
	m.Retain()
	defer m.Release()

	b := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     testenv.TempDir(t),
		sessionTemp: m,
	}
	b.shell = b.resolved()
	if b.shell.Kind != sandbox.ShellBash {
		t.Skip("no bash on this host")
	}
	out, err := b.Execute(context.Background(), argsJSON(t, map[string]any{"command": `printf '%s' "$TMPDIR"`}))
	if err != nil {
		t.Fatal(err)
	}
	// The shell sees the directory in MSYS form, as it did when MSYS converted
	// the process environment at startup, and native children get the Windows
	// form back when MSYS spawns them.
	dir := m.Dir()
	want := "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	if out != want {
		t.Fatalf("TMPDIR = %q, want %q", out, want)
	}
}

func TestBashSchemaUnchangedWithSessionTemp(t *testing.T) {
	plain := bash{}.Schema()
	withTemp := bash{sessionTemp: sessiontemp.New()}.Schema()
	if string(plain) != string(withTemp) {
		t.Fatalf("session temp must not change bash schema\nplain=%s\nwith=%s", plain, withTemp)
	}
	var schema map[string]any
	if err := json.Unmarshal(plain, &schema); err != nil {
		t.Fatal(err)
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "command" {
		t.Fatalf("required = %v", req)
	}
}

func TestBashFailsWhenSessionTempUnavailable(t *testing.T) {
	// Create-failure path: manager is owned but cannot create the directory.
	m := sessiontemp.NewWithRoot(testenv.TempDir(t))
	m.Retain()
	defer m.Release()
	m.SetMkDirForTest(func(string) (string, error) {
		return "", os.ErrPermission
	})
	b := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     testenv.TempDir(t),
		sessionTemp: m,
	}
	_, err := b.Execute(context.Background(), argsJSON(t, map[string]any{"command": "true"}))
	if err == nil {
		t.Fatal("want command failure when session temp cannot be created")
	}
	if !errors.Is(err, sessiontemp.ErrUnavailable) && !strings.Contains(err.Error(), "session temporary") {
		t.Fatalf("error = %v, want session temporary failure", err)
	}

	// Sealed manager (last owner released) must fail closed, not fall back.
	sealed := sessiontemp.NewWithRoot(testenv.TempDir(t))
	sealed.Retain()
	sealed.Release()
	b2 := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     testenv.TempDir(t),
		sessionTemp: sealed,
	}
	_, err = b2.Execute(context.Background(), argsJSON(t, map[string]any{"command": "true"}))
	if err == nil {
		t.Fatal("want failure against sealed session temp manager")
	}
}

func TestBashBackgroundLeaseSurvivesRotate(t *testing.T) {
	m := sessiontemp.NewWithRoot(testenv.TempDir(t))
	m.Retain()
	defer m.Release()

	// Pin the old generation with a lease (simulates a running background job).
	oldLease, err := m.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	oldDir := oldLease.Dir()
	if err := os.WriteFile(filepath.Join(oldDir, "bg.txt"), []byte("still-here"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.Rotate()
	fresh, err := m.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Dir() == oldDir {
		t.Fatal("rotate should yield a new generation for new commands")
	}
	// Background job's generation remains until its lease is released.
	if _, err := os.Stat(filepath.Join(oldDir, "bg.txt")); err != nil {
		t.Fatalf("old generation deleted while background lease held: %v", err)
	}
	oldLease.Release()
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old generation should delete after background lease release: %v", err)
	}
	fresh.Release()
}
