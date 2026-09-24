package tui

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/stavlos/internal/oauth"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// loginFlow tracks one device-code sign-in. cancel aborts the pending
// provider.login.wait; id is the login the wait belongs to (results for any
// other id are stale and dropped).
type loginFlow struct {
	provider, name string
	method         string // "" = provider default
	id             string
	cancel         context.CancelFunc
}

// reset cancels any pending wait and forgets the flow.
func (l *loginFlow) reset() {
	if l.cancel != nil {
		l.cancel()
	}
	*l = loginFlow{}
}

// onListed handles list and sign-in results: providers, logins, roles,
// variants, channels, a resumed channel and models.
func (m *Model) onListed(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case providersMsg:
		return m.onProviders(msg)
	case loginStartMsg:
		return m.onLoginStart(msg)
	case loginDoneMsg:
		return m.onLoginDone(msg)
	case rolesMsg:
		if !m.accepts(msg.scope) {
			return nil
		}
		if msg.err == nil {
			m.roles = msg.roles
		}
		if !msg.quiet {
			return m.onRoles(msg)
		}
	case variantsMsg:
		if !m.accepts(msg.scope) {
			return nil
		}
		return m.onVariants(msg)
	case channelsMsg:
		return m.onChannelsListed(msg)
	case switchedMsg:
		m.switching = false
		if msg.err != nil {
			m.dirsNext = false
			return m.setStatus("could not switch channel: "+msg.err.Error(), true)
		}
		fromNav, hovered := m.focus == focusSidebar, m.hoverFocus
		cmd := m.bindChannel(cleanChannel(msg.info))
		m.opened = true // switched to from inside the TUI: the channel's chat, not the splash
		if fromNav {
			// picked in the nav: the nav keeps focus, its cursor (and the
			// background that marks it) on the channel just opened, the row
			// the pointer is still on
			cmd = tea.Batch(cmd, m.setFocus(focusSidebar))
			m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbHere})
			m.followSidebarCursor()
			if hovered { // moving off the nav gives the input its focus back
				m.hoverFocus, m.hoverFrom = true, focusInput
			}
		}
		if m.dirsNext { // reached through its gear: its dirs dialog
			m.dirsNext = false
			return tea.Batch(cmd, m.openConfigEditor(false))
		}
		if open, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(cmd, open)
		}
		return cmd
	case modelsMsg:
		if !m.accepts(msg.scope) {
			return nil
		}
		return m.onModels(msg)
	}
	return nil
}

// onChannelsListed routes a channel list to what asked for it.
func (m *Model) onChannelsListed(msg channelsMsg) tea.Cmd {
	if msg.purpose != channelsNav && !m.accepts(msg.scope) {
		return nil
	}
	msg.channels = cleanChannels(msg.channels)
	switch msg.purpose {
	case channelsNav:
		if msg.err == nil {
			row, ok := m.sidebarAt(m.sbCursor)
			selectedChannel := ""
			if ok && (row.kind == sbOther || row.kind == sbOtherAgent) {
				selectedChannel = m.navChannels[row.k].ID
			}
			for _, s := range msg.channels {
				if s.ID == m.channelID && s.Dir == m.channel.Dir {
					m.channel.DirError = s.DirError
				}
			}
			m.navChannels = resumable(msg.channels, m.channelID)
			m.pruneTrees()
			if selectedChannel != "" {
				found := false
				for k, s := range m.navChannels {
					if s.ID == selectedChannel {
						row.k = k
						found = true
						break
					}
				}
				if !found {
					row = sidebarRow{kind: sbHere}
				}
			}
			if ok {
				m.sbCursor = m.sidebarIndex(row)
				m.followSidebarCursor()
			}
		}
	case channelsPicker:
		return m.onChannels(msg)
	}
	return nil
}

// openOverlay replaces any open overlay and blurs the main input. A sign-in
// shown by the replaced overlay is abandoned.
func (m *Model) openOverlay(o *overlay) tea.Cmd {
	if m.ov != nil && m.ov.mode == overlayLogin && o.mode != overlayLogin {
		m.login.reset()
	}
	if m.ov == nil {
		m.dialogFrom = m.focus // one overlay replacing another keeps the original origin
	}
	m.ov = o
	m.input.Blur()
	return o.input.Focus()
}

// closeOverlay drops the overlay and gives focus back to what had it when
// the overlay opened (the input, the meta row…). The section's focus was
// never changed by the overlay; only the blurred input needs refocusing.
func (m *Model) closeOverlay() tea.Cmd {
	m.ov = nil
	from := m.dialogFrom
	if isTab(from) || !m.focusAvailable(from) {
		from = focusInput
	}
	if from != m.focus {
		return m.setFocus(from)
	}
	if from == focusInput {
		return m.input.Focus()
	}
	return nil
}

// overlayKey routes a key while an overlay is open.
func (m *Model) overlayKey(msg tea.KeyMsg) tea.Cmd {
	o := m.ov
	if o.mode == overlayLogin {
		return m.loginKey(msg)
	}
	if o.mode == overlayInput { // a text field: space types, enter submits
		switch {
		case key.Matches(msg, keys.OvClose):
			return m.closeOverlay()
		case key.Matches(msg, keys.OvSelect):
			return m.overlaySubmit(false)
		}
		return o.update(msg)
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeOverlay()
	case key.Matches(msg, keys.Select):
		return m.overlaySubmit(false)
	case key.Matches(msg, keys.OvAlt):
		return m.overlaySubmit(true)
	case key.Matches(msg, keys.OvRemove):
		return m.overlayRemove()
	case o.handleNav(msg):
		return nil
	}
	return o.update(msg)
}

// loginKey handles keys while the overlay shows a sign-in: esc cancels,
// o re-opens the browser, enter retries after an error.
func (m *Model) loginKey(msg tea.KeyMsg) tea.Cmd {
	o := m.ov
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.cancelLogin()
	case key.Matches(msg, keys.OvOpen) && !o.login.key: // "o" is a character to type in the field
		return openBrowserCmd(o.login.url)
	case key.Matches(msg, keys.OvSelect):
		if o.login.key {
			return m.submitLoginKey()
		}
		if o.login.err == "" {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == m.login.provider {
				return m.startLogin(p, m.login.method)
			}
		}
		return m.startLogin(protocol.ProviderInfo{ID: m.login.provider, Name: m.login.name}, m.login.method)
	}
	if o.login.key {
		return o.update(msg) // the field takes everything else
	}
	return nil
}

// submitLoginKey hands what was typed to the waiting login. The overlay
// stays open: provider.login.wait is what reports success or failure, the
// same as for a code the user entered elsewhere.
func (m *Model) submitLoginKey() tea.Cmd {
	o := m.ov
	k := strings.TrimSpace(o.input.Value())
	if k == "" {
		o.setLoginError("paste the key from the console first")
		o.login.key = true // the field stays: the error is about what was typed
		return nil
	}
	o.input.Reset()
	id := m.login.id
	ctx, c := m.ctx, m.c
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		return resultMsg{err: call(ctx, c, protocol.ProviderLoginKey, protocol.LoginKeyParams{ID: id, Key: k})}
	})
}

// cancelLogin abandons the pending wait and closes the overlay.
func (m *Model) cancelLogin() tea.Cmd {
	m.login.reset()
	return tea.Batch(m.closeOverlay(), m.setStatus("login cancelled", false))
}

// overlaySubmit is Enter (alt=false) or ctrl+s (alt=true) in the overlay.
func (m *Model) overlaySubmit(alt bool) tea.Cmd {
	o := m.ov
	switch o.kind {
	case ovDiscord, ovWeb, ovSandbox:
		return m.submitService(o.kind)
	case ovProviders:
		it := o.selected()
		if it == nil {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == it.id {
				if len(p.Methods) > 1 {
					return m.openMethodMenu(p)
				}
				return m.startLogin(p, "")
			}
		}
		return nil

	case ovMethods:
		it := o.selected()
		if it == nil {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == m.login.provider {
				return m.startLogin(p, it.id)
			}
		}
		return nil

	case ovRoles:
		it := o.selected()
		if it == nil {
			return nil
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return tea.Batch(m.closeOverlay(), pickRoleCmd(m.ctx, m.c, agent, it.id))
	case ovMode:
		it := o.selected()
		if it == nil {
			return nil
		}
		return tea.Batch(m.closeOverlay(), setModeCmd(m.ctx, m.c, m.channelID, it.id))
	case ovNewChannel, ovNewChannelDir:
		return m.submitNewChannel()
	case ovChannels:
		it := o.selected()
		if it == nil {
			return nil
		}
		if it.id == m.channelID {
			return tea.Batch(m.closeOverlay(), m.setStatus("already in this channel", false))
		}
		return tea.Batch(m.closeOverlay(), m.switchTo(it.id))
	case ovVariants:
		it := o.selected()
		if it == nil {
			return nil
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return tea.Batch(m.closeOverlay(), pickVariantCmd(m.ctx, m.c, agent, it.id))
	case ovModels:
		it := o.selected()
		if it == nil {
			return nil
		}
		if alt {
			return tea.Batch(m.closeOverlay(), pickChannelModelCmd(m.ctx, m.c, m.channelID, it.id))
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected (ctrl+s sets the channel default)", true)
		}
		return tea.Batch(m.closeOverlay(), pickAgentModelCmd(m.ctx, m.c, agent, it.id))
	}
	return nil
}

func (m *Model) submitNewChannel() tea.Cmd {
	o := m.ov
	value := strings.TrimSpace(o.input.Value())
	if value == "" {
		return nil
	}
	if o.kind == ovNewChannel {
		next := newOverlay(ovNewChannelDir, overlayInput, "Default directory for #"+value)
		next.newName = value
		next.input.SetValue(m.channel.Dir)
		next.input.CursorEnd()
		return m.openOverlay(next)
	}
	if m.switching {
		return nil
	}
	m.switching = true
	return tea.Batch(m.closeOverlay(), m.setStatus("creating a channel", false), createChannelCmd(m.ctx, m.c, m.channelID, m.channel.Dir, value, o.newName))
}

// openMethodMenu shows the provider's sign-in methods (opencode's "Login
// method" step), default first.
func (m *Model) openMethodMenu(p protocol.ProviderInfo) tea.Cmd {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	m.login.reset()
	m.login.provider, m.login.name = p.ID, name
	items := make([]overlayItem, 0, len(p.Methods))
	for _, me := range p.Methods {
		items = append(items, overlayItem{id: me.ID, label: me.Label})
	}
	ov := newOverlay(ovMethods, overlayList, "Login method")
	ov.setItems(items)
	return m.openOverlay(ov)
}

// startLogin switches the overlay to "Sign in to <Name>" and asks the
// daemon for a device code. Any earlier sign-in is abandoned.
func (m *Model) startLogin(p protocol.ProviderInfo, method string) tea.Cmd {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	m.login.reset()
	m.login.provider, m.login.name, m.login.method = p.ID, name, method
	var cmd tea.Cmd
	if m.ov == nil {
		cmd = m.openOverlay(newOverlay(ovProviders, overlayLogin, ""))
	}
	m.ov.switchLogin(name)
	return tea.Batch(cmd, loginStartCmd(m.ctx, m.c, p.ID, method))
}

// onLoginStart shows the URL and code, opens the browser once and starts
// the cancellable wait. A result for a sign-in that was cancelled is dropped.
func (m *Model) onLoginStart(msg loginStartMsg) tea.Cmd {
	if m.ov == nil || m.ov.mode != overlayLogin || msg.provider != m.login.provider {
		return nil
	}
	if msg.err != nil {
		m.ov.setLoginError(msg.err.Error())
		return nil
	}
	m.ov.setLogin(msg.res.URL, msg.res.Code, msg.res.Instructions, msg.res.Method)
	ctx, cancel := context.WithCancel(m.ctx)
	m.login.id, m.login.cancel = msg.res.ID, cancel
	wait := loginWaitCmd(ctx, m.c, msg.res.ID)
	if msg.res.Method == oauth.MethodAPIKey {
		// nothing to poll and nothing to open: the wait is for the key the
		// field is about to take, and opening the console unasked would
		// steal the window from someone pasting a key they already have
		return wait
	}
	return tea.Batch(openBrowserCmd(msg.res.URL), wait)
}

// onLoginDone finishes the sign-in: close the overlay, refresh providers and
// offer a model when none is set; on failure show the error for a retry.
func (m *Model) onLoginDone(msg loginDoneMsg) tea.Cmd {
	if msg.id == "" || msg.id != m.login.id {
		return nil // cancelled or superseded
	}
	name := m.login.name
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			return nil
		}
		if m.login.cancel != nil {
			m.login.cancel()
			m.login.cancel = nil
		}
		m.login.id = ""
		if m.ov != nil && m.ov.mode == overlayLogin {
			m.ov.setLoginError(msg.err.Error())
			return nil
		}
		return m.setStatus("sign in to "+name+": "+msg.err.Error(), true)
	}
	if msg.info.Name != "" {
		name = msg.info.Name
	}
	m.login.reset()
	for i, p := range m.providers {
		if p.ID == msg.info.ID {
			m.providers[i] = msg.info
		}
	}
	var cmds []tea.Cmd
	if m.ov != nil && m.ov.mode == overlayLogin {
		cmds = append(cmds, m.closeOverlay())
	}
	cmds = append(cmds, m.setStatus("connected "+name+" ✓", false), providersCmd(m.ctx, m.c, providersMsg{refresh: true}))
	if m.channel.Model == "" && (len(m.agents) == 0 || m.agents[0].Model == "") {
		cmds = append(cmds, modelsCmd(m.ctx, m.c, m.requestScope()))
	}
	return tea.Batch(cmds...)
}

func (m *Model) onProviders(msg providersMsg) tea.Cmd {
	if msg.err != nil {
		if msg.refresh {
			return nil
		}
		return m.setStatus("could not list the providers: "+msg.err.Error(), true)
	}
	m.providers = msg.res.Providers
	if msg.refresh {
		return nil
	}
	if msg.jump != "" {
		for _, p := range msg.res.Providers {
			if p.ID == msg.jump || strings.ToLower(p.Name) == msg.jump {
				if len(p.Methods) > 1 {
					return m.openMethodMenu(p)
				}
				return m.startLogin(p, "")
			}
		}
	}
	o := newOverlay(ovProviders, overlayList, "Providers")
	o.setItems(providerItems(msg.res.Providers))
	cmd := m.openOverlay(o)
	switch {
	case msg.jump != "":
		return tea.Batch(cmd, m.setStatus("unknown provider "+msg.jump, true))
	case msg.status != "":
		return tea.Batch(cmd, m.setStatus(msg.status, false))
	}
	return cmd
}

// overlayRemove is ctrl+d in a list overlay: in the providers dialog it
// signs out of the selected provider.
func (m *Model) overlayRemove() tea.Cmd {
	o := m.ov
	if o == nil || o.kind != ovProviders {
		return nil
	}
	it := o.selected()
	if it == nil {
		return nil
	}
	for _, p := range m.providers {
		if p.ID == it.id {
			if !p.Connected {
				return m.setStatus(it.label+" is not signed in", true)
			}
			return disconnectProviderCmd(m.ctx, m.c, p.ID)
		}
	}
	return nil
}

func (m *Model) onRoles(msg rolesMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("could not list the roles: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovRoles, overlayList, "Change role of "+label)
	// The main agent may take primary or all roles, a subagent subagent or
	// all ones: a role's mode decides where it shows.
	primary := true
	if a := m.selectedAgent(); a != nil && a.Parent != "" {
		primary = false
	}
	items := make([]overlayItem, 0, len(msg.roles))
	for _, r := range msg.roles {
		if (primary && r.Type == "subagent") || (!primary && r.Type == "primary") {
			continue
		}
		items = append(items, overlayItem{id: r.Name, label: r.Name, hint: roleHint(r)})
	}
	if len(items) == 0 {
		o.setEmpty("no role may run here", false)
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// roleHint is a role's one-line summary in the /roles picker: its
// description, then its mode when restricted, its default model when it
// has a whitelist, and what it spawns.
func roleHint(r protocol.RoleInfo) string {
	hint := r.Description
	if r.Type != "" && r.Type != "all" {
		hint += "  · " + r.Type
	}
	if len(r.Models) > 0 {
		short, _ := transcript.SplitModel(r.Models[0].ID)
		hint += "  · " + short
		if len(r.Models) > 1 {
			hint += fmt.Sprintf(" +%d", len(r.Models)-1)
		}
	}
	if len(r.Spawn) > 0 {
		hint += "  · spawns " + strings.Join(r.Spawn, ", ")
	}
	return hint
}

// roleInfo finds a cached role by name.
func (m *Model) roleInfo(name string) *protocol.RoleInfo {
	for i := range m.roles {
		if m.roles[i].Name == name {
			return &m.roles[i]
		}
	}
	return nil
}

// whoStyle is the colour a chat line takes for someone it names: an agent's
// role colour (green when its role sets none), blue for the human.
func (m *Model) whoStyle(name string) lipgloss.Style {
	if name == "user" {
		return lipgloss.NewStyle().Foreground(theme.ColAccent)
	}
	tint := "green"
	for _, a := range m.agents {
		if a.Name == name {
			if r := m.roleInfo(a.Role); r != nil && r.Color != "" {
				tint = r.Color
			}
			break
		}
	}
	return roleStyle(tint)
}

// whoKey changes whenever whoStyle's colours do, so cached chat rows redraw.
func (m *Model) whoKey() string {
	var b strings.Builder
	for _, a := range m.agents {
		b.WriteString(a.Name + "=")
		if r := m.roleInfo(a.Role); r != nil {
			b.WriteString(r.Color)
		}
		b.WriteByte(';')
	}
	return b.String()
}

// selectedRole is the selected agent's role, nil when unknown.
func (m *Model) selectedRole() *protocol.RoleInfo {
	if a := m.selectedAgent(); a != nil {
		return m.roleInfo(a.Role)
	}
	return nil
}

// roleTints maps every cached role to its colour name ("" for none).
func (m *Model) roleTints() map[string]string {
	out := map[string]string{}
	for _, r := range m.roles {
		if r.Color != "" {
			out[r.Name] = r.Color
		}
	}
	return out
}

// roleModelSpec is the whitelist entry of role r that admits model id
// (glob-aware), nil when r has no whitelist or none matches.
func roleModelSpec(r *protocol.RoleInfo, id string) *protocol.ModelSpec {
	if r == nil {
		return nil
	}
	for i := range r.Models {
		if ok, _ := path.Match(r.Models[i].ID, id); ok || r.Models[i].ID == id {
			return &r.Models[i]
		}
	}
	return nil
}

// roleAllowsModel: any model without a whitelist, else a matching entry.
func roleAllowsModel(r *protocol.RoleInfo, id string) bool {
	return r == nil || len(r.Models) == 0 || roleModelSpec(r, id) != nil
}

// onChannels opens the global /channels picker with paths and first prompts.
func (m *Model) onChannels(msg channelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("could not list the channels: "+msg.err.Error(), true)
	}
	o := newOverlay(ovChannels, overlayList, "All channels")
	sortChannels(msg.channels)
	items := make([]overlayItem, 0, len(msg.channels))
	for _, s := range msg.channels {
		items = append(items, channelItem(s, s.ID == m.channelID))
	}
	if len(items) == 0 {
		o.setEmpty("no channels here yet", false)
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// channelItem is one row of the /channels picker: its #name, then its first
// prompt, when it started, its model, cost and live agents.
func channelItem(s protocol.ChannelInfo, current bool) overlayItem {
	var meta []string
	if s.Dir != "" {
		meta = append(meta, format.ShortHome(s.Dir))
	}
	if s.DirError != "" {
		meta = append(meta, "directory unavailable")
	}
	if title := strings.Join(strings.Fields(s.Title), " "); title != "" {
		meta = append(meta, format.Trunc(title, 40))
	}
	if t, err := time.Parse(time.RFC3339, s.Created); err == nil {
		meta = append(meta, format.Elapsed(time.Since(t))+" ago")
	}
	if s.Model != "" {
		meta = append(meta, s.Model)
	}
	if s.CostUSD > 0 {
		meta = append(meta, "$"+format.Cost(s.CostUSD))
	}
	if s.Live > 0 {
		meta = append(meta, fmt.Sprintf("%d live", s.Live))
	}
	if current {
		meta = append(meta, "current")
	}
	return overlayItem{id: s.ID, label: channelLabel(s), hint: strings.Join(meta, " · "), good: current}
}

// channelRef names a channel in a status line: "#name", or its short id
// before it has one.
func channelRef(info protocol.ChannelInfo) string {
	if info.Name == "" {
		return format.ShortID(info.ID)
	}
	return "#" + info.Name
}

// bindChannel rebinds the TUI to another channel: every per-channel
// piece of state starts over and a fresh reconcile replays its history.
func (m *Model) bindChannel(info protocol.ChannelInfo) tea.Cmd {
	m.cfgEditor = nil
	m.configEpoch++
	for i := range m.prompts {
		if m.prompts[i].Channel == "" {
			m.prompts[i].Channel = m.channelID
		}
	}
	m.savePermissionDraft()
	m.permFor, m.permEdit = "", ""
	m.dirInput.Blur()
	m.saveQuestionDraft()
	m.q = questionState{}
	m.promptInput.Blur()
	if m.editors == nil {
		m.editors = map[string]editorState{}
	}
	m.draft = m.input.Value()
	m.editors[m.channelID] = m.editorState
	if prev := m.channel; m.channelID != "" && m.channelID != info.ID {
		prev.ID = m.channelID
		// the nav lists the channel left behind among the others at once,
		// not only at the next catalog refresh
		m.navChannels = resumable(append(slices.Clone(m.navChannels), prev), info.ID)
	}
	m.stash()
	m.generation++
	m.channelState = newChannelState(info.ID, info)
	m.editorState = m.editors[info.ID]
	m.roles = nil
	m.ov = nil
	m.restore(info.ID)
	delete(m.trees, info.ID) // its agents are live again
	m.superChat = true
	m.layout()
	m.input.Placeholder = m.placeholder()
	m.follow = true
	m.input.Reset()
	m.input.SetValue(m.draft)
	m.refreshViewport()
	m.layout()
	return tea.Batch(m.setFocus(focusInput), reconcileCmd(m.ctx, m.c, m.requestScope()), m.setStatus("opened "+channelRef(info), false))
}

// openVariants is /variants: with no argument it opens the picker for the
// selected agent's model; with a name it sets that variant ("default"
// clears it).
func (m *Model) openVariants(arg string) tea.Cmd {
	a := m.selectedAgent()
	if a == nil {
		return m.setStatus("no agent selected", true)
	}
	modelID, current := m.channel.Model, a.Variant
	if a.Model != "" {
		modelID = a.Model
	}
	if modelID == "" {
		return m.setStatus("no model selected — /models first", true)
	}
	if arg == "" {
		return variantsCmd(m.ctx, m.c, m.requestScope(), modelID, current)
	}
	v := strings.ToLower(arg)
	if v == "default" || v == "none" || v == "off" {
		v = ""
	}
	return pickVariantCmd(m.ctx, m.c, a.ID, v)
}

// onVariants opens the /variants picker: the provider default plus every
// variant the model offers, the one in force marked.
func (m *Model) onVariants(msg variantsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("could not list the model's variants: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovVariants, overlayList, "Variant for "+label+" · "+msg.model)
	if len(msg.variants) == 0 {
		o.setEmpty("this model has no variants", false)
	}
	mark := func(id string) string {
		if id == msg.current {
			return "  · current"
		}
		return ""
	}
	// A role that lists variants for this model narrows the picker to them
	// (and drops the provider default, which is then not allowed).
	var allowed []string
	if spec := roleModelSpec(m.selectedRole(), msg.model); spec != nil && len(spec.Variants) > 0 {
		allowed = spec.Variants
	}
	var items []overlayItem
	if allowed == nil {
		items = append(items, overlayItem{id: "", label: "default", hint: "provider default" + mark("")})
	}
	for _, v := range msg.variants {
		if allowed != nil && !containsStr(allowed, v) {
			continue
		}
		items = append(items, overlayItem{id: v, label: v, hint: "reasoning effort" + mark(v)})
	}
	o.setItems(items)
	return m.openOverlay(o)
}

func (m *Model) onModels(msg modelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("could not list the models: "+msg.err.Error(), true)
	}
	o := newOverlay(ovModels, overlayList, "Select a model")
	if len(msg.models) == 0 {
		o.setEmpty("no providers connected — run /provider", true)
	}
	// The selected agent's role may whitelist models: only those are offered.
	models := msg.models
	if r := m.selectedRole(); r != nil && len(r.Models) > 0 {
		o.title = "Select a model · allowed by " + r.Name
		models = nil
		for _, mi := range msg.models {
			if roleAllowsModel(r, mi.ID) {
				models = append(models, mi)
			}
		}
		if len(models) == 0 && len(msg.models) > 0 {
			o.setEmpty("role "+r.Name+" allows none of the connected models", true)
		}
	}
	o.setItems(modelItems(models))
	return m.openOverlay(o)
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
