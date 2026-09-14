package protocol

import (
	"net/url"
	"strings"

	"github.com/nicodes/stavlos/internal/shellcmd"
	"github.com/nicodes/stavlos/internal/toolname"
)

// ToolPrefix is what "allow … for this session" may remember for a tool
// call: the command prefix for shell (shellcmd.Prefix), the host for
// web_fetch, "" for tools without a sensible prefix.
func ToolPrefix(tool, arg string) string {
	switch tool {
	case toolname.Shell:
		return shellcmd.Prefix(arg)
	case toolname.WebFetch:
		return URLHost(arg)
	}
	return ""
}

// ToolPrefixCovers reports whether a remembered prefix covers a call of
// tool with arg: whole-word command prefixes for shell, the host for
// web_fetch.
func ToolPrefixCovers(tool, prefix, arg string) bool {
	switch tool {
	case toolname.Shell:
		return shellcmd.Covers(prefix, arg)
	case toolname.WebFetch:
		return prefix != "" && URLHost(arg) == prefix
	}
	return false
}

// URLHost is the lower-cased host of a URL ("" when it has none); a bare
// "host/path" counts as https.
func URLHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
