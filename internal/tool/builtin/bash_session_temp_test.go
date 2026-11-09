package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
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
			ctx := sandbox.WithPermissionPreset(t.Context(), "danger-full-access")
			m := sessiontemp.NewWithRoot(t.TempDir())
			m.Retain()
			defer m.Release()

			b := bash{
				sb:          sandbox.Spec{Mode: "off"},
				shell:       tc.shell,
				workDir:     t.TempDir(),
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
			if _, err := b.Execute(ctx, argsJSON(t, map[string]any{"command": writeCmd})); err != nil {
				t.Fatalf("write: %v", err)
			}

			out, err := b.Execute(ctx, argsJSON(t, map[string]any{"command": readCmd}))
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
	want := []any{"command"}
	if (bash{}).resolved().Kind == sandbox.ShellPowerShell {
		want = []any{"command", "description"}
	}
	if !slices.Equal(req, want) {
		t.Fatalf("required = %v, want %v for the current platform shell", req, want)
	}
}

func TestBashFailsWhenSessionTempUnavailable(t *testing.T) {
	// Create-failure path: manager is owned but cannot create the directory.
	m := sessiontemp.NewWithRoot(t.TempDir())
	m.Retain()
	defer m.Release()
	m.SetMkDirForTest(func(string) (string, error) {
		return "", os.ErrPermission
	})
	b := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     t.TempDir(),
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
	sealed := sessiontemp.NewWithRoot(t.TempDir())
	sealed.Retain()
	sealed.Release()
	b2 := bash{
		sb:          sandbox.Spec{Mode: "off"},
		workDir:     t.TempDir(),
		sessionTemp: sealed,
	}
	_, err = b2.Execute(context.Background(), argsJSON(t, map[string]any{"command": "true"}))
	if err == nil {
		t.Fatal("want failure against sealed session temp manager")
	}
}

func TestBashBackgroundLeaseSurvivesRotate(t *testing.T) {
	m := sessiontemp.NewWithRoot(t.TempDir())
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

func TestWindowsBashStartsOnHostTemp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSYS /tmp binding is Windows-only")
	}
	sh := sandbox.Shell{Kind: sandbox.ShellBash, Path: "bash"}
	m := sessiontemp.NewWithRoot(t.TempDir())
	m.Retain()
	defer m.Release()
	b := bash{sb: sandbox.Spec{Mode: "off"}, shell: sh, workDir: t.TempDir(), sessionTemp: m}

	prepared, lease, err := b.prepareLaunch(fullAccessBashTestContext(t.Context()), sh, `printf '%s' "$TMPDIR"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	dir := lease.Dir()
	posix := "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	for _, kv := range prepared.EnvOverrides {
		key, _, _ := strings.Cut(kv, "=")
		if key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			t.Fatalf("bash process env still carries a session temp entry: %q", kv)
		}
	}
	if !slices.ContainsFunc(prepared.EnvOverrides, func(kv string) bool { return strings.HasPrefix(kv, "GOCACHE=") }) {
		t.Fatalf("session-private cache keys must stay in the environment: %v", prepared.EnvOverrides)
	}
	joined := strings.Join(prepared.Argv, " ")
	if !strings.Contains(joined, "export TMPDIR='"+posix+"'") {
		t.Fatalf("argv does not export the session temp in POSIX form: %v", prepared.Argv)
	}
	if !strings.Contains(joined, "printf") {
		t.Fatalf("argv lost the requested command: %v", prepared.Argv)
	}
}

func TestWindowsBashSessionTempSurvivesQuotedPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSYS /tmp binding is Windows-only")
	}
	sh, ok := sandbox.ResolveExplicitBash("")
	if !ok {
		t.Skip("Git Bash is required for the native Windows regression")
	}
	root := filepath.Join(t.TempDir(), "it's private")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	m := sessiontemp.NewWithRoot(root)
	m.Retain()
	defer m.Release()
	b := bash{sb: sandbox.Spec{Mode: "off"}, shell: sh, workDir: t.TempDir(), sessionTemp: m}

	out, err := b.Execute(fullAccessBashTestContext(t.Context()), argsJSON(t, map[string]any{"command": `printf '%s' "$TMPDIR"`}))
	if err != nil {
		t.Fatalf("printf: %v (%q)", err, out)
	}
	dir := m.Dir()
	want := "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	if out != want {
		t.Fatalf("$TMPDIR = %q, want %q", out, want)
	}
}

func TestWindowsBashPersistentShellKeepsSessionTemp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSYS /tmp binding is Windows-only")
	}
	sh, ok := sandbox.ResolveExplicitBash("")
	if !ok {
		t.Skip("Git Bash is required for the native Windows regression")
	}
	m := sessiontemp.NewWithRoot(t.TempDir())
	m.Retain()
	defer m.Release()
	b := persistentBash(t, t.TempDir())
	b.shell = sh
	b.sessionTemp = m
	ctx := fullAccessBashTestContext(t.Context())

	out, err := b.Execute(ctx, argsJSON(t, map[string]any{"command": `printf '%s' "$TMPDIR"`}))
	if err != nil {
		t.Fatalf("first command: %v (%q)", err, out)
	}
	dir := m.Dir()
	posix := "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	if out != posix {
		t.Fatalf("persistent $TMPDIR = %q, want %q", out, posix)
	}
	out, err = b.Execute(ctx, argsJSON(t, map[string]any{"command": `test -d "$TMPDIR" && test "$TMPDIR" = "$TEMP" && printf alive`}))
	if err != nil || !strings.Contains(out, "alive") {
		t.Fatalf("session temp not usable in the persistent shell: %q error=%v", out, err)
	}
}
