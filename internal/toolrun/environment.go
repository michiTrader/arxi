package toolrun

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/workspace"
)

var environmentNames = map[string][]string{
	"linux":   {"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR"},
	"windows": {"ComSpec", "LANG", "PATH", "PATHEXT", "SystemDrive", "SystemRoot", "TEMP", "TMP", "USERPROFILE"},
}

func commandEnvironment(profile workspace.CommandProfile, root string) ([]string, error) {
	if profile.EnvironmentVersion != workspace.EnvironmentAllowlistV1 {
		return nil, fmt.Errorf("toolrun: command environment policy %q is unsupported", profile.EnvironmentVersion)
	}
	names, ok := environmentNames[runtime.GOOS]
	if !ok {
		return nil, fmt.Errorf("toolrun: command environment allowlist is unavailable on %s", runtime.GOOS)
	}
	allowed := make(map[string]string, len(names))
	for _, name := range names {
		allowed[environmentKey(name)] = name
	}
	values := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		key := environmentKey(name)
		if canonical, permitted := allowed[key]; permitted {
			values[canonical] = value
		}
	}
	if runtime.GOOS == "windows" {
		values["TEMP"] = root
		values["TMP"] = root
	} else {
		values["HOME"] = root
		values["TMPDIR"] = root
	}
	out := make([]string, 0, len(values))
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

func environmentKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
