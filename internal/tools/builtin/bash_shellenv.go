package builtin

import (
	"maps"
	"runtime"
	"slices"
	"strings"

	"reasonix/internal/safety/sandbox"
)

// shellEnvOverrides renders the configured [tools.shell] env as sorted KEY=value
// pairs. Sorted so identical config yields identical environment bytes; a key
// that cannot appear in an environment entry is dropped rather than mangled.
func shellEnvOverrides(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// bashNeedsHostTemp reports whether the shell must start without the session
// temporary directory in its environment. MSYS reads TEMP/TMP once, when the
// first process of its shared runtime starts, and binds /tmp there for every
// later process, so a session directory would leave every command warning that
// /tmp is missing after deletion (#15023). Launcher shells have no MSYS mount.
func bashNeedsHostTemp(sh sandbox.Shell, sessionDir string) bool {
	return runtime.GOOS == "windows" && sh.Kind == sandbox.ShellBash && len(sh.Args()) == 0 && sessionDir != ""
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

// withoutSessionTempKeys drops the three temporary-directory variables from the
// child environment; on Windows+bash their values come from the prelude. Cache
// keys in SessionTempEnvKeys stay, so session-private caches keep working.
func withoutSessionTempKeys(overrides []string) []string {
	kept := overrides[:0]
	for _, kv := range overrides {
		key, _, _ := strings.Cut(kv, "=")
		if key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// shellEnvKeyEqual is the key identity the fold uses. It is a seam so a test can
// stand in for Windows, whose environment block is case-insensitive.
var shellEnvKeyEqual = envKeyEqual

// shellEnvMap folds the configured shell env under the host-owned session
// variables, which win, so a preset value cannot displace TMPDIR. A session key
// also displaces a preset one differing only in case: the child sees one entry.
func shellEnvMap(shellEnv, sessionTemp map[string]string) map[string]string {
	if len(shellEnv) == 0 {
		return sessionTemp
	}
	merged := maps.Clone(shellEnv)
	for k, v := range sessionTemp {
		for existing := range merged {
			if existing != k && shellEnvKeyEqual(existing, k) {
				delete(merged, existing)
			}
		}
		merged[k] = v
	}
	return merged
}
