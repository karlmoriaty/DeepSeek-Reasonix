package builtin

import (
	"runtime"
	"strings"

	"reasonix/internal/sandbox"
)

// bashNeedsHostTemp reports whether the shell must start without the session
// temporary directory in its environment. MSYS reads TEMP/TMP once, when the
// first process of its shared runtime starts, and binds /tmp there for every
// later process, so a session directory would leave later commands without
// /tmp once deleted (#15023). WSL bash is resolved away on Windows.
func bashNeedsHostTemp(sh sandbox.Shell, sessionDir string) bool {
	return runtime.GOOS == "windows" && sh.Kind == sandbox.ShellBash && sessionDir != ""
}

// bashSessionTempPrelude re-exports the session temporary directory inside the
// shell, after MSYS has resolved /tmp from the stable host temp. The value is
// POSIX, as MSYS itself presents a Windows TEMP to bash. Placed on its own line
// so a command that starts with a comment cannot absorb it.
func bashSessionTempPrelude(dir string) string {
	quoted := "'" + strings.ReplaceAll(bashTempPath(dir), "'", `'\''`) + "'"
	return "export TMPDIR=" + quoted + " TMP=" + quoted + " TEMP=" + quoted + "\n"
}

// bashTempPath renders a Windows directory the way MSYS exposes it to bash, so
// exporting it inside the shell reads the same as the environment MSYS would
// have converted at startup.
func bashTempPath(dir string) string {
	if len(dir) >= 3 && dir[1] == ':' && (dir[2] == '\\' || dir[2] == '/') {
		return "/" + strings.ToLower(dir[:1]) + strings.ReplaceAll(dir[2:], `\`, "/")
	}
	if strings.HasPrefix(dir, `\\`) {
		return strings.ReplaceAll(dir, `\`, "/")
	}
	return dir
}

// withoutSessionTempKeys drops only the three temporary-directory variables
// from the child environment; their values come from the prelude. The cache
// keys in sandbox.SessionTempEnvKeys stay, so session-private caches keep
// working.
func withoutSessionTempKeys(overrides []string) []string {
	kept := overrides[:0]
	for _, kv := range overrides {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "TMPDIR", "TMP", "TEMP":
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}
