package api

import (
	"context"
	"crypto/x509"
	"encoding/json/jsontext"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// StatusSource は /status に出す、provisioner 自身の情報（設計概要 §9。段階 3-1 の U8）。
type StatusSource struct {
	Version   string
	StartedAt time.Time
	// AdminClients は管理クライアントの識別名と、ピンを読んだ時刻を返す。
	AdminClients func() ([]string, time.Time)
	// DownstreamsLoadedAt は下流の登録簿を読んだ時刻を返す。
	DownstreamsLoadedAt func() time.Time
	// ClientCertificate は下流に提示するクライアント証明書を返す。
	ClientCertificate func() *x509.Certificate
	// ServerCertificate は API のサーバー証明書を返す。
	ServerCertificate func() *x509.Certificate
	// Poller は下流の状態を集める。
	Poller *Poller
}

// statusValkeyTimeout は /status で Valkey に PING するときの上限。
const statusValkeyTimeout = 2 * time.Second

type statusJSON struct {
	Version              string           `json:"version"`
	StartedAt            time.Time        `json:"startedAt"`
	Valkey               valkeyJSON       `json:"valkey"`
	AdminClients         []string         `json:"adminClients"`
	AdminClientsLoadedAt time.Time        `json:"adminClientsLoadedAt"`
	DownstreamsLoadedAt  time.Time        `json:"downstreamsLoadedAt"`
	ClientCertificate    certJSON         `json:"clientCertificate"`
	ServerCertificate    certJSON         `json:"serverCertificate"`
	Downstreams          []downstreamJSON `json:"downstreams"`
}

type valkeyJSON struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type certJSON struct {
	Subject     string    `json:"subject"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

func certInfo(c *x509.Certificate) certJSON {
	if c == nil {
		return certJSON{}
	}
	return certJSON{Subject: c.Subject.String(), NotAfter: c.NotAfter, Fingerprint: certs.Fingerprint(c)}
}

// downstreamJSON は下流ごとの状態。Status は下流の /status の本文を解釈せずにそのまま入れる（U8）。
type downstreamJSON struct {
	Name              string         `json:"name"`
	URL               string         `json:"url"`
	ServerCertificate certJSON       `json:"serverCertificate"`
	Reachable         bool           `json:"reachable"`
	CheckedAt         time.Time      `json:"checkedAt,omitzero"`
	Error             string         `json:"error,omitempty"`
	Hint              string         `json:"hint,omitempty"`
	Status            jsontext.Value `json:"status,omitempty"`
}

// getStatus は GET /status。下流の状態は Poller が集めたものを返し、要求のたびには下流を呼ばない
// （監視が頻繁に呼んでも下流に負荷をかけない。設計概要 §9）。
func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	s := h.Status
	out := statusJSON{Version: s.Version, StartedAt: s.StartedAt, Downstreams: []downstreamJSON{}}
	ctx, cancel := context.WithTimeout(r.Context(), statusValkeyTimeout)
	defer cancel()
	if err := h.Store.Ping(ctx); err != nil {
		out.Valkey.Error = err.Error()
	} else {
		out.Valkey.Reachable = true
	}
	out.AdminClients, out.AdminClientsLoadedAt = s.AdminClients()
	if out.AdminClients == nil {
		out.AdminClients = []string{}
	}
	out.DownstreamsLoadedAt = s.DownstreamsLoadedAt()
	out.ClientCertificate = certInfo(s.ClientCertificate())
	out.ServerCertificate = certInfo(s.ServerCertificate())
	for _, name := range downstream.Names {
		c := h.Downstream(name)
		d := downstreamJSON{Name: string(name), URL: c.BaseURL(), ServerCertificate: certInfo(c.ServerCertificate())}
		if res, ok := s.Poller.result(name); ok {
			d.Reachable, d.CheckedAt, d.Error, d.Hint, d.Status = res.reachable, res.checkedAt, res.err, res.hint, res.body
		}
		out.Downstreams = append(out.Downstreams, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// Poller は下流の /status を定期に集めて覚えておく（設計概要 §9）。
type Poller struct {
	Downstream func(downstream.Name) Relayer
	Log        *slog.Logger

	mu      sync.Mutex
	results map[downstream.Name]pollResult
}

type pollResult struct {
	checkedAt time.Time
	reachable bool
	err       string
	hint      string
	body      jsontext.Value
}

// Run は ctx が終わるまで、すぐに 1 回と、interval ごとに下流の状態を集める。
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	p.PollOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce は全下流の /status を並べて 1 回ずつ呼ぶ。つながらなくなったとき（DOWNSTREAM_UNREACHABLE）と、
// またつながったとき（DOWNSTREAM_REACHABLE）にログを出す（30 秒ごとに同じログを出さない）。
func (p *Poller) PollOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range downstream.Names {
		wg.Go(func() { p.poll(ctx, name) })
	}
	wg.Wait()
}

func (p *Poller) poll(ctx context.Context, name downstream.Name) {
	traceID := trace.New()
	res := pollResult{checkedAt: time.Now().UTC()}
	resp, err := p.Downstream(name).Relay(trace.With(ctx, traceID), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"status"}})
	switch {
	case err != nil:
		res.err, res.hint = err.Error(), downstream.Diagnose(name, err)
	case resp.Status != http.StatusOK:
		res.err = "status " + strconv.Itoa(resp.Status)
	default:
		res.reachable = true
		if v := jsontext.Value(resp.Body); v.IsValid() {
			res.body = v
		}
	}

	p.mu.Lock()
	prev, seen := p.results[name]
	if p.results == nil {
		p.results = map[downstream.Name]pollResult{}
	}
	p.results[name] = res
	p.mu.Unlock()

	switch {
	case !res.reachable && (!seen || prev.reachable):
		p.Log.Warn("downstream is not available", "event_id", "DOWNSTREAM_UNREACHABLE", "trace_id", traceID,
			"downstream", string(name), "error", res.err, "hint", res.hint)
	case res.reachable && (!seen || !prev.reachable):
		p.Log.Info("downstream is available", "event_id", "DOWNSTREAM_REACHABLE", "trace_id", traceID, "downstream", string(name))
	}
}

func (p *Poller) result(name downstream.Name) (pollResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.results[name]
	return r, ok
}
