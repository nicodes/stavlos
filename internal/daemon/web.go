package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/statefile"
	"github.com/nicodes/stavlos/internal/web"
)

// webState is whether the human turned the web UI on, kept in the data
// directory so it comes back with the daemon.
type webState struct {
	Enabled bool `json:"enabled"`
}

func (d *Daemon) webStatePath() string { return filepath.Join(d.DataDir, "web.json") }

// startWeb builds the web server from the global configuration and brings it
// up when it was on the last time the daemon ran.
func (d *Daemon) startWeb(ctx context.Context) {
	o := web.Options{Serve: func(ctx context.Context, nc net.Conn) { d.handleConn(ctx, nc, true) }, Sheet: d.webSheet}
	if cfg, err := config.LoadGlobal(); err == nil && cfg.Web != nil {
		o.Port, o.Hosts = cfg.Web.Port, cfg.Web.Hosts
	}
	d.webMu.Lock()
	defer d.webMu.Unlock()
	d.web, d.webCtx = web.New(o), ctx
	var st webState
	if b, err := os.ReadFile(d.webStatePath()); err == nil && json.Unmarshal(b, &st) == nil && st.Enabled {
		if err := d.web.Enable(ctx); err != nil {
			log.Printf("web: %v", err)
		} else {
			log.Printf("web UI on %s", d.web.URL())
		}
	}
}

// Web runs one of web.status, web.enable, web.disable and web.open.
func (d *Daemon) Web(method string) (protocol.WebStatus, error) {
	d.webMu.Lock()
	defer d.webMu.Unlock()
	if d.web == nil {
		return protocol.WebStatus{}, errors.New("the web UI is unavailable in this daemon")
	}
	var open string
	var err error
	switch method {
	case protocol.MWebEnable:
		if err = d.web.Enable(d.webCtx); err == nil {
			d.saveWebState(true)
			open, err = d.web.OpenURL()
		}
	case protocol.MWebDisable:
		d.web.Disable()
		d.saveWebState(false)
	case protocol.MWebOpen:
		open, err = d.web.OpenURL()
	}
	st := d.web.Status()
	if method == protocol.MWebEnable || method == protocol.MWebDisable {
		d.changed(protocol.ChangedWeb)
	}
	return protocol.WebStatus{Enabled: st.Enabled, URL: st.URL, OpenURL: open, Error: st.Error}, err
}

func (d *Daemon) saveWebState(enabled bool) {
	b, _ := json.Marshal(webState{Enabled: enabled})
	if err := statefile.WriteAtomic(d.webStatePath(), b, 0o600, false); err != nil {
		log.Printf("web: saving state: %v", err)
	}
}

func webRoute(m protocol.Method[protocol.None, protocol.WebStatus]) routeEntry {
	return route(m, func(_ context.Context, c *conn, _ protocol.None) (protocol.WebStatus, error) { return c.d.Web(m.Name) })
}

// webSheet hands the web server one sheet's page.
func (d *Daemon) webSheet(channel, id string) (web.Sheet, error) {
	s, err := d.channel(channel)
	if err != nil {
		return web.Sheet{}, err
	}
	info, page, err := s.Sheet(id)
	if err != nil {
		return web.Sheet{}, err
	}
	return web.Sheet{Title: info.Title, HTML: page}, nil
}
