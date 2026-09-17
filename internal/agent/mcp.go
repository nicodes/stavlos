package agent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/proc"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/sandbox"
	"github.com/nicodes/stavlos/internal/tools"
)

// MCP servers are agent-level: an agent whose role lists a server starts
// its own process at its next turn, calls its tools as mcp__<server>__<tool>,
// and takes the process down when it is killed, idles, or its role stops
// listing the server. Processes are runtime state, not log state: they die
// with the daemon and start again at the agent's next turn.

const (
	mcpStartTimeout = 30 * time.Second
	mcpCallTimeout  = 5 * time.Minute
	mcpNamePrefix   = "mcp__"
)

// MCPIdleAfter is how long an agent may sit idle before its MCP servers
// are stopped; they start again at its next turn. A variable so tests can
// shorten it.
var MCPIdleAfter = 10 * time.Minute

// mcpSet is an agent's servers. Its lock is never held while taking the
// channel's: events are committed after it is released.
type mcpSet struct {
	mu      sync.Mutex
	servers map[string]*mcpServer
	idle    *time.Timer
}

// mcpServer is one running (or failed) server.
type mcpServer struct {
	name    string
	state   protocol.MCPState
	err     string
	started time.Time
	session *mcp.ClientSession
	tools   map[string]*mcp.Tool // model-facing name → tool
	order   []string             // model-facing names, in the server's order
}

// mcpToolName is the model-facing name of a server's tool: mcp__server__tool
// with anything outside [A-Za-z0-9_-] replaced (the ChatGPT backend accepts
// only those), cut to 64 characters with a short hash when longer.
func mcpToolName(server, tool string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	name := mcpNamePrefix + clean(server) + "__" + clean(tool)
	if len(name) > 64 {
		sum := sha1.Sum([]byte(name))
		name = name[:57] + "_" + hex.EncodeToString(sum[:])[:6]
	}
	return name
}

// ensureMCP starts every server listed that is not running and stops the
// ones no longer listed. It blocks the turn for at most mcpStartTimeout per
// server; a server that fails is logged and skipped.
func (a *Agent) ensureMCP(ctx context.Context, cfg *config.Effective, listed []string) {
	a.mcp.mu.Lock()
	if a.mcp.servers == nil {
		a.mcp.servers = map[string]*mcpServer{}
	}
	var start, stop []string
	for _, name := range listed {
		if s, ok := a.mcp.servers[name]; !ok || s.state == protocol.MCPStopped {
			start = append(start, name)
		}
	}
	for name := range a.mcp.servers {
		if !contains(listed, name) {
			stop = append(stop, name)
		}
	}
	a.mcp.mu.Unlock()
	for _, name := range stop {
		a.stopMCP(name, true)
	}
	for _, name := range start {
		a.startMCP(ctx, cfg, name)
	}
}

// startMCP launches one server and lists its tools.
func (a *Agent) startMCP(ctx context.Context, cfg *config.Effective, name string) {
	srv := &mcpServer{name: name, state: protocol.MCPStarting, started: time.Now(), tools: map[string]*mcp.Tool{}}
	a.mcp.mu.Lock()
	a.mcp.servers[name] = srv
	a.mcp.mu.Unlock()
	fail := func(err error) {
		a.mcp.mu.Lock()
		srv.state, srv.err = protocol.MCPFailed, err.Error()
		a.mcp.mu.Unlock()
		_ = a.record(event.MCPFailed, event.MCPFailedPayload{Server: name, Error: err.Error()})
	}
	def, ok := cfg.MCP[name]
	switch {
	case !ok:
		fail(fmt.Errorf("server %q is not defined under mcp in stavlos.json", name))
		return
	case def.Command == "":
		fail(fmt.Errorf("server %q has no command (remote servers are not supported yet)", name))
		return
	}
	cmd := exec.CommandContext(a.ctx, config.ExpandEnv(def.Command), expandAll(def.Args)...)
	cmd.Dir = a.c.Dir()
	cmd.Env = proc.Env(cfg.PassEnv) // scrubbed like a shell command's; the definition's env: adds what the server needs
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+config.ExpandEnv(v))
	}
	if spec := a.c.sandboxSpec(cfg); spec != nil {
		if _, err := sandbox.Wrap(cmd, *spec); err != nil {
			fail(fmt.Errorf("sandbox: %v", err))
			return
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{sb: &stderr, max: 4096}
	client := mcp.NewClient(&mcp.Implementation{Name: "stavlos", Version: "1"}, nil)
	sctx, cancel := context.WithTimeout(ctx, mcpStartTimeout)
	defer cancel()
	session, err := client.Connect(sctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		if se := strings.TrimSpace(stderr.String()); se != "" {
			err = fmt.Errorf("%v: %s", err, lastLine(se))
		}
		fail(err)
		return
	}
	var names []string
	for t, err := range session.Tools(sctx, nil) {
		if err != nil {
			_ = session.Close()
			fail(fmt.Errorf("tools/list: %v", err))
			return
		}
		n := mcpToolName(name, t.Name)
		a.mcp.mu.Lock()
		if _, dup := srv.tools[n]; !dup {
			srv.order = append(srv.order, n)
		}
		srv.tools[n] = t
		a.mcp.mu.Unlock()
		names = append(names, n)
	}
	a.mcp.mu.Lock()
	srv.session, srv.state = session, protocol.MCPConnected
	a.mcp.mu.Unlock()
	_ = a.record(event.MCPStarted, event.MCPStartedPayload{Server: name, Tools: names})
	// A server that exits on its own is reported once; the agent's next turn
	// starts it again.
	go func() {
		err := session.Wait()
		a.mcp.mu.Lock()
		if srv.state != protocol.MCPConnected {
			a.mcp.mu.Unlock()
			return
		}
		srv.state, srv.session, srv.err = protocol.MCPStopped, nil, "exited"
		if err != nil {
			srv.err = err.Error()
		}
		why := srv.err
		a.mcp.mu.Unlock()
		_ = a.record(event.MCPFailed, event.MCPFailedPayload{Server: name, Error: "server exited: " + why})
	}()
}

// armMCPIdle schedules the idle stop after a turn ends; disarmMCPIdle
// cancels it when the next turn starts.
func (a *Agent) armMCPIdle() {
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	if len(a.mcp.servers) == 0 {
		return
	}
	if a.mcp.idle != nil {
		a.mcp.idle.Stop()
	}
	a.mcp.idle = time.AfterFunc(MCPIdleAfter, func() {
		a.c.mu.Lock()
		st := a.state()
		idle := !st.inTurn && !st.startsTurn()
		a.c.mu.Unlock()
		if idle {
			a.stopMCP("", true)
		}
	})
}

func (a *Agent) disarmMCPIdle() {
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	if a.mcp.idle != nil {
		a.mcp.idle.Stop()
		a.mcp.idle = nil
	}
}

// hasMCP reports whether the agent has any server entries.
func (a *Agent) hasMCP() bool {
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	return len(a.mcp.servers) > 0
}

// stopMCP stops one server ("" = all) and drops it, logging mcp.stopped for
// the ones that were up when log is set.
func (a *Agent) stopMCP(name string, log bool) {
	a.mcp.mu.Lock()
	var victims []*mcpServer
	for n, srv := range a.mcp.servers {
		if name == "" || n == name {
			victims = append(victims, srv)
			delete(a.mcp.servers, n)
		}
	}
	type closed struct {
		name string
		up   bool
		sess *mcp.ClientSession
	}
	var done []closed
	for _, srv := range victims {
		done = append(done, closed{srv.name, srv.state == protocol.MCPConnected, srv.session})
		srv.state, srv.session = protocol.MCPStopped, nil
	}
	a.mcp.mu.Unlock()
	for _, c := range done {
		if c.sess != nil {
			_ = c.sess.Close()
		}
		if c.up && log {
			_ = a.record(event.MCPStopped, event.MCPRefPayload{Server: c.name})
		}
	}
}

// mcpDefs is the tool list of every connected server, for the model.
func (a *Agent) mcpDefs() []model.ToolDef {
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	servers := make([]string, 0, len(a.mcp.servers))
	for n := range a.mcp.servers {
		servers = append(servers, n)
	}
	sort.Strings(servers)
	var defs []model.ToolDef
	for _, n := range servers {
		srv := a.mcp.servers[n]
		if srv.state != protocol.MCPConnected {
			continue
		}
		for _, tn := range srv.order {
			t := srv.tools[tn]
			schema, err := json.Marshal(t.InputSchema)
			if err != nil || t.InputSchema == nil {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			desc := t.Description
			if desc == "" {
				desc = t.Title
			}
			defs = append(defs, model.ToolDef{Name: tn, Description: fmt.Sprintf("[%s] %s", n, desc), Schema: schema})
		}
	}
	return defs
}

// mcpTool resolves a model-facing tool name to a callable tool.
func (a *Agent) mcpTool(name string) (tools.Tool, bool) {
	if !strings.HasPrefix(name, mcpNamePrefix) {
		return nil, false
	}
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	for _, srv := range a.mcp.servers {
		if t, ok := srv.tools[name]; ok && srv.state == protocol.MCPConnected {
			return mcpTool{server: srv.name, session: srv.session, tool: t, name: name}, true
		}
	}
	return nil, false
}

// mcpInfo reports every server listed (pending when not started yet).
func (a *Agent) mcpInfo(listed []string) []protocol.MCPInfo {
	a.mcp.mu.Lock()
	defer a.mcp.mu.Unlock()
	var out []protocol.MCPInfo
	seen := map[string]bool{}
	for _, name := range listed {
		if seen[name] {
			continue
		}
		seen[name] = true
		info := protocol.MCPInfo{Name: name, State: protocol.MCPPending}
		if srv, ok := a.mcp.servers[name]; ok {
			info.State, info.Error = srv.state, srv.err
			info.Tools = append([]string(nil), srv.order...)
			info.Started = srv.started.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	return out
}

// mcpTool adapts one server tool to tools.Tool. It carries the session it
// was resolved with: a server that exits meanwhile fails the call.
type mcpTool struct {
	server  string
	session *mcp.ClientSession
	tool    *mcp.Tool
	name    string
}

func (t mcpTool) Def() model.ToolDef {
	schema, _ := json.Marshal(t.tool.InputSchema)
	return model.ToolDef{Name: t.name, Description: t.tool.Description, Schema: schema}
}

// Subject is the compact argument JSON, so rules may match on it; most
// rules are on the tool name (mcp__github__*).
func (t mcpTool) Subject(in json.RawMessage) policy.Subject {
	var buf bytes.Buffer
	if err := json.Compact(&buf, in); err != nil {
		return policy.Text(string(in))
	}
	return policy.Text(buf.String())
}

func (t mcpTool) Run(ctx context.Context, in json.RawMessage, env *tools.Env) tools.Result {
	if t.session == nil {
		return tools.Result{Output: fmt.Sprintf("MCP server %s is not connected", t.server), IsError: true}
	}
	cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	var args any
	if len(in) > 0 {
		if err := json.Unmarshal(in, &args); err != nil {
			return tools.Result{Output: "arguments must be a JSON object: " + err.Error(), IsError: true}
		}
	}
	res, err := t.session.CallTool(cctx, &mcp.CallToolParams{Name: t.tool.Name, Arguments: args})
	if err != nil {
		return tools.Result{Output: fmt.Sprintf("%s: %v", t.name, err), IsError: true}
	}
	return tools.Result{Output: clip.Middle(mcpResultText(res), env.MaxOutput), IsError: res.IsError}
}

// mcpResultText flattens a tool result: text blocks as they are, other
// content described by type, structured content as JSON when there is no
// text.
func mcpResultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", v.URI))
		case *mcp.EmbeddedResource:
			if v.Resource != nil && v.Resource.Text != "" {
				parts = append(parts, v.Resource.Text)
			} else if v.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource %s]", v.Resource.URI))
			}
		default:
			if b, err := c.MarshalJSON(); err == nil {
				parts = append(parts, string(b))
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

func expandAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = config.ExpandEnv(x)
	}
	return out
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// limitedWriter keeps the first max bytes written (a server's stderr, for
// error messages).
type limitedWriter struct {
	sb  *strings.Builder
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.sb.Len(); room > 0 {
		w.sb.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
