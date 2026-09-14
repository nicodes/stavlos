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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/proc"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// MCP servers are agent-level: an agent whose role lists a server starts
// its own process at its next turn, calls its tools as mcp__<server>__<tool>,
// and takes the process down when it is killed or its role stops listing
// the server. Nothing is shared between agents, so a stateful server (a
// browser, a filesystem view) belongs to one agent. Processes die with the
// daemon; recovery starts them again at the agent's next turn.

const (
	mcpStartTimeout = 30 * time.Second
	mcpCallTimeout  = 5 * time.Minute
	mcpNamePrefix   = "mcp__"
)

// MCPIdleAfter is how long an agent may sit idle before its MCP servers
// are stopped; they start again at its next turn. Agents are never killed
// by the model, so this is what keeps an idle child cheap. A variable so
// tests can shorten it.
var MCPIdleAfter = 10 * time.Minute

// mcpServer is one running (or failed) server owned by an agent.
type mcpServer struct {
	name    string
	state   string // starting | connected | failed | stopped
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

// ensureMCP starts every server the role lists that is not running yet.
// It blocks the turn for at most mcpStartTimeout per server; a server that
// fails is logged and skipped, and the turn goes on without it.
func (a *Agent) ensureMCP(ctx context.Context, cfg *config.Effective) {
	a.mu.Lock()
	if a.mcps == nil {
		a.mcps = map[string]*mcpServer{}
	}
	listed := append([]string(nil), a.preset.MCP...)
	var start []string
	for _, name := range listed {
		if s, ok := a.mcps[name]; !ok || s.state == "stopped" {
			start = append(start, name)
		}
	}
	// Servers the role no longer lists go down.
	var stop []string
	for name := range a.mcps {
		if !contains(listed, name) {
			stop = append(stop, name)
		}
	}
	a.mu.Unlock()
	for _, name := range stop {
		a.stopMCP(name)
	}
	for _, name := range start {
		a.startMCP(ctx, cfg, name)
	}
}

// startMCP launches one server and lists its tools.
func (a *Agent) startMCP(ctx context.Context, cfg *config.Effective, name string) {
	s := &mcpServer{name: name, state: "starting", started: time.Now(), tools: map[string]*mcp.Tool{}}
	a.mu.Lock()
	a.mcps[name] = s
	a.mu.Unlock()
	fail := func(err error) {
		a.mu.Lock()
		s.state, s.err = "failed", err.Error()
		a.mu.Unlock()
		_, _ = a.record(context.Background(), event.MCPFailed, event.MCPFailedPayload{Server: name, Error: err.Error()})
	}
	def, ok := cfg.MCP[name]
	if !ok {
		fail(fmt.Errorf("server %q is not defined under mcp in stavlos.json", name))
		return
	}
	if def.Command == "" {
		fail(fmt.Errorf("server %q has no command (remote servers are not supported yet)", name))
		return
	}
	cmd := exec.CommandContext(a.ctx, config.ExpandEnv(def.Command), expandAll(def.Args)...)
	cmd.Dir = a.s.Dir
	cmd.Env = proc.Env(cfg.PassEnv) // scrubbed like a shell command's; the definition's env: adds what the server needs
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+config.ExpandEnv(v))
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
		a.mu.Lock()
		if _, dup := s.tools[n]; !dup {
			s.order = append(s.order, n)
		}
		s.tools[n] = t
		a.mu.Unlock()
		names = append(names, n)
	}
	a.mu.Lock()
	s.session, s.state = session, "connected"
	a.mu.Unlock()
	_, _ = a.record(context.Background(), event.MCPStarted, event.MCPStartedPayload{Server: name, Tools: names})
	// A server that exits on its own is reported once, so the tab and the
	// chat show it; the agent's next turn starts it again.
	go func() {
		err := session.Wait()
		a.mu.Lock()
		if s.state != "connected" {
			a.mu.Unlock()
			return
		}
		s.state, s.session = "stopped", nil
		s.err = "exited"
		if err != nil {
			s.err = err.Error()
		}
		a.mu.Unlock()
		_, _ = a.record(context.Background(), event.MCPFailed, event.MCPFailedPayload{Server: name, Error: "server exited: " + s.err})
	}()
}

// armMCPIdle schedules the idle stop after a turn ends; disarmMCPIdle
// cancels it when the next turn starts.
func (a *Agent) armMCPIdle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.mcps) == 0 {
		return
	}
	if a.mcpIdle != nil {
		a.mcpIdle.Stop()
	}
	a.mcpIdle = time.AfterFunc(MCPIdleAfter, func() {
		a.mu.Lock()
		idle := a.state == StateIdle && len(a.prompts)+len(a.steers)+len(a.responses) == 0
		a.mu.Unlock()
		if idle {
			a.stopMCP("")
		}
	})
}

func (a *Agent) disarmMCPIdle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mcpIdle != nil {
		a.mcpIdle.Stop()
		a.mcpIdle = nil
	}
}

// hasMCP reports whether the agent has any server entries.
func (a *Agent) hasMCP() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.mcps) > 0
}

// stopMCP stops one server ("" = all) and drops it from the agent.
func (a *Agent) stopMCP(name string) {
	a.mu.Lock()
	var victims []*mcpServer
	for n, s := range a.mcps {
		if name == "" || n == name {
			victims = append(victims, s)
			delete(a.mcps, n)
		}
	}
	a.mu.Unlock()
	for _, s := range victims {
		a.mu.Lock()
		sess, wasUp := s.session, s.state == "connected"
		s.state, s.session = "stopped", nil
		a.mu.Unlock()
		if sess != nil {
			_ = sess.Close()
		}
		if wasUp {
			_, _ = a.record(context.Background(), event.MCPStopped, event.MCPRefPayload{Server: s.name})
		}
	}
}

// mcpDefs is the tool list of every connected server, for the model.
func (a *Agent) mcpDefs() []model.ToolDef {
	a.mu.Lock()
	defer a.mu.Unlock()
	servers := make([]string, 0, len(a.mcps))
	for n := range a.mcps {
		servers = append(servers, n)
	}
	sort.Strings(servers)
	var defs []model.ToolDef
	for _, n := range servers {
		s := a.mcps[n]
		if s.state != "connected" {
			continue
		}
		for _, tn := range s.order {
			t := s.tools[tn]
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
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.mcps {
		if t, ok := s.tools[name]; ok && s.state == "connected" {
			return mcpTool{server: s, tool: t, name: name}, true
		}
	}
	return nil, false
}

// mcpInfoLocked reports every server the role lists (pending when not
// started yet); the caller holds a.mu.
func (a *Agent) mcpInfoLocked() []protocol.MCPInfo {
	var out []protocol.MCPInfo
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		info := protocol.MCPInfo{Name: name, State: "pending"}
		if s, ok := a.mcps[name]; ok {
			info.State, info.Error = s.state, s.err
			info.Tools = append([]string(nil), s.order...)
			if !s.started.IsZero() {
				info.Started = s.started.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, info)
	}
	for _, n := range a.preset.MCP {
		add(n)
	}
	return out
}

// mcpTool adapts one server tool to the tools.Tool interface.
type mcpTool struct {
	server *mcpServer
	tool   *mcp.Tool
	name   string
}

func (t mcpTool) Def() model.ToolDef {
	schema, _ := json.Marshal(t.tool.InputSchema)
	return model.ToolDef{Name: t.name, Description: t.tool.Description, Schema: schema}
}

// PolicyArg is the compact argument JSON, so rules may match on it; most
// rules are on the tool name (mcp__github__*).
func (t mcpTool) PolicyArg(in json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, in); err != nil {
		return string(in)
	}
	return buf.String()
}

func (t mcpTool) Run(ctx context.Context, in json.RawMessage, env *tools.Env) tools.Result {
	sess := t.server.session
	if sess == nil {
		return tools.Result{Output: fmt.Sprintf("MCP server %s is not connected", t.server.name), IsError: true}
	}
	cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	var args any
	if len(in) > 0 {
		if err := json.Unmarshal(in, &args); err != nil {
			return tools.Result{Output: "arguments must be a JSON object: " + err.Error(), IsError: true}
		}
	}
	res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: t.tool.Name, Arguments: args})
	if err != nil {
		return tools.Result{Output: fmt.Sprintf("%s: %v", t.name, err), IsError: true}
	}
	return tools.Result{Output: tools.Clip(mcpResultText(res), env.MaxOutput), IsError: res.IsError}
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
		if len(p) > room {
			p = p[:room]
		}
		w.sb.Write(p)
	}
	return len(p), nil
}
