// aka-provisioner は、管理面の aka-ossbss から下流（aka-vecgen・aka-radius の管理API）への変更の経路を 1 つにまとめる中継。
// 許した口だけを通し、書き込みでは Idempotency-Key を必須にし、監査ログを残す。設計は docs/design-overview.md を参照。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/api"
	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/config"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/health"
	"github.com/oyaguma3/aka-provisioner/internal/server"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

const (
	// statusInterval は下流の /status を集める間隔（設計概要 §9）。
	statusInterval = 30 * time.Second
	// auditMaintainInterval は監査ログのファイルを圧縮・削除する間隔。
	auditMaintainInterval = time.Hour
)

const usage = `usage: aka-provisioner <command>

commands:
  serve              サーバーを起動する（コマンド省略時の既定）
  check-downstream   下流の登録簿を読み、全下流の /status に接続できるか確かめる
  gen-client-cert    下流に提示するクライアント証明書と秘密鍵を作る
                     （aka-provisioner gen-client-cert -h で使い方を表示）
  server-cert        provisioner の API のサーバー証明書（PEM）を標準出力に出す（なければ自己署名を作る）。
                     管理クライアント（aka-ossbss）に検証用として渡す
  healthcheck        同じコンテナのループバックの healthcheck の口に問い合わせる（compose の healthcheck 用）

設定は環境変数で与える。下流の登録簿・管理クライアントのピン・サーバー証明書は SIGHUP で読み直す。
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx)
	case "check-downstream":
		err = checkDownstream(ctx)
	case "gen-client-cert":
		err = genClientCert(args[1:], os.Stdout, os.Stderr)
	case "server-cert":
		err = serverCert(os.Stdout, os.Stderr)
	case "healthcheck":
		err = healthcheck(ctx)
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// fatal は起動の誤りをログに出し、そのエラーを返す（main が終了コード 1 で終える）。
func fatal(log *slog.Logger, msg, eventID string, err error) error {
	log.Error(msg, "event_id", eventID, "error", err.Error())
	return err
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "event_id", "STARTING", "version", version)

	openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
	st, err := store.Open(openCtx, store.Options{Addr: cfg.ValkeyAddr, Password: cfg.ValkeyPassword})
	cancelOpen()
	if err != nil {
		return fatal(log, "failed to connect to Valkey", "VALKEY_CONN_ERR", err)
	}
	defer st.Close()

	created, err := certs.EnsureFiles(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSHosts)
	if err != nil {
		return fatal(log, "failed to prepare server certificate", "SERVER_CERT_LOAD_ERR", err)
	}
	if created {
		log.Info("generated self-signed server certificate", "cert", cfg.TLSCertFile, "hosts", cfg.TLSHosts)
	}
	serverCert, err := certs.LoadFile(cfg.TLSCertFile, cfg.TLSKeyFile, log)
	if err != nil {
		return fatal(log, "failed to load server certificate", "SERVER_CERT_LOAD_ERR", err)
	}
	logServerCert(log, "loaded server certificate", "SERVER_CERT_LOADED", serverCert)

	pins, err := server.LoadPins(cfg.AdminClientsFile)
	if err != nil {
		return fatal(log, "failed to load admin clients", "ADMIN_CLIENTS_LOAD_ERR", err)
	}
	logPins(log, "loaded admin clients", "ADMIN_CLIENTS_LOADED", pins)

	reg, err := downstream.LoadRegistry(cfg.DownstreamsFile, cfg.DownstreamTimeout, log)
	if err != nil {
		return fatal(log, "failed to load downstreams registry", "DOWNSTREAMS_LOAD_ERR", err)
	}
	logRegistry(log, "loaded downstreams registry", "DOWNSTREAMS_LOADED", reg)

	if err := audit.CheckWritableDir(cfg.AuditDir); err != nil {
		return fatal(log, "audit directory is not writable", "AUDIT_DIR_ERR", err)
	}
	auditFiles := audit.NewFiles(cfg.AuditDir, cfg.AuditRetentionDays)

	healthSrv, err := health.Start(cfg.HealthAddr, st.Ping, log)
	if err != nil {
		return fatal(log, "failed to listen for healthcheck", "SYS_LISTEN_ERR", err)
	}
	defer healthSrv.Close()

	relayer := func(n downstream.Name) api.Relayer { return reg.Client(n) }
	poller := &api.Poller{Downstream: relayer, Log: log}
	h := &api.Handler{
		Log:        log,
		MgmtClient: pins.NameFromRequest,
		Downstream: relayer,
		Store:      st,
		Audit:      &audit.Recorder{Log: log, Store: st, MaxLen: cfg.AuditMaxLen, Files: auditFiles},
		Status: &api.StatusSource{
			Version:             version,
			StartedAt:           time.Now().UTC(),
			AdminClients:        pins.Names,
			DownstreamsLoadedAt: reg.LoadedAt,
			ClientCertificate:   reg.ClientCertificate,
			ServerCertificate:   serverCert.Leaf,
			Poller:              poller,
		},
	}

	// 背後の処理（下流の状態、監査ログのファイルの整理、SIGHUP の読み直し）は、サーバーが止まったら止める。
	bgCtx, stopBG := context.WithCancel(ctx)
	defer stopBG()
	var wg sync.WaitGroup
	wg.Go(func() { poller.Run(bgCtx, statusInterval) })
	wg.Go(func() { maintainAudit(bgCtx, log, auditFiles) })
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	wg.Go(func() {
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-hup:
				if reload(log, pins, reg, serverCert) {
					poller.PollOnce(bgCtx)
				}
			}
		}
	})

	err = server.Run(ctx, server.Options{
		Addr:           cfg.Addr,
		GetCertificate: serverCert.GetCertificate,
		Pins:           pins,
		Handler:        h.Routes(),
		Log:            log,
	})
	stopBG()
	wg.Wait()
	return err
}

// reload は SIGHUP で、ピン・下流の登録簿・サーバー証明書を読み直す（それぞれ、失敗したら前のまま）。
// 下流の登録簿を読み直せたら true を返す（下流の状態をすぐに集め直す）。
func reload(log *slog.Logger, pins *server.Pins, reg *downstream.Registry, serverCert *certs.File) bool {
	if err := pins.Reload(); err != nil {
		log.Error("failed to reload admin clients (keeping the previous ones)", "event_id", "ADMIN_CLIENTS_RELOAD_ERR", "error", err.Error())
	} else {
		logPins(log, "reloaded admin clients", "ADMIN_CLIENTS_RELOADED", pins)
	}
	if err := serverCert.Reload(); err != nil {
		log.Error("failed to reload server certificate (keeping the previous one)", "event_id", "SERVER_CERT_RELOAD_ERR", "error", err.Error())
	} else {
		logServerCert(log, "reloaded server certificate", "SERVER_CERT_RELOADED", serverCert)
	}
	if err := reg.Reload(); err != nil {
		log.Error("failed to reload downstreams registry (keeping the previous one)", "event_id", "DOWNSTREAMS_RELOAD_ERR", "error", err.Error())
		return false
	}
	logRegistry(log, "reloaded downstreams registry", "DOWNSTREAMS_RELOADED", reg)
	return true
}

func maintainAudit(ctx context.Context, log *slog.Logger, files *audit.Files) {
	t := time.NewTicker(auditMaintainInterval)
	defer t.Stop()
	for {
		if err := files.Maintain(time.Now()); err != nil {
			log.Error("failed to maintain audit files", "event_id", "AUDIT_MAINTAIN_ERR", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func logPins(log *slog.Logger, msg, eventID string, pins *server.Pins) {
	names, _ := pins.Names()
	log.Info(msg, "event_id", eventID, "admin_clients", strings.Join(names, ","), "fingerprints", pins.Fingerprints())
}

func logServerCert(log *slog.Logger, msg, eventID string, f *certs.File) {
	leaf := f.Leaf()
	log.Info(msg, "event_id", eventID, "fingerprint", certs.Fingerprint(leaf), "not_after", leaf.NotAfter,
		"dns_names", leaf.DNSNames, "ip_addresses", leaf.IPAddresses)
}

func logRegistry(log *slog.Logger, msg, eventID string, reg *downstream.Registry) {
	args := []any{"event_id", eventID,
		"client_cert_fingerprint", certs.Fingerprint(reg.ClientCertificate()), "client_cert_not_after", reg.ClientCertificate().NotAfter}
	for _, n := range downstream.Names {
		c := reg.Client(n)
		args = append(args, string(n)+"_url", c.BaseURL(), string(n)+"_server_cert_not_after", c.ServerCertificate().NotAfter)
	}
	log.Info(msg, args...)
}

// checkDownstream は登録簿を読み、全下流の /status に接続できるかを確かめて表示する。導入時の確認に使う。
func checkDownstream(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg, err := downstream.LoadRegistry(cfg.DownstreamsFile, cfg.DownstreamTimeout, log)
	if err != nil {
		return err
	}
	fmt.Println("下流の登録簿:", cfg.DownstreamsFile)
	fmt.Println("クライアント証明書のフィンガープリント:", certs.Fingerprint(reg.ClientCertificate()))
	var errs []error
	for _, n := range downstream.Names {
		c := reg.Client(n)
		fmt.Println()
		fmt.Printf("%s: %s（サーバー証明書の有効期限 %s）\n", n, c.BaseURL(), c.ServerCertificate().NotAfter.Local().Format(time.DateOnly))
		resp, err := c.Relay(ctx, downstream.RelayRequest{Method: http.MethodGet, Path: []string{"status"}})
		switch {
		case err != nil:
			fmt.Println("  接続できませんでした:", err)
			if hint := downstream.Diagnose(n, err); hint != "" {
				fmt.Println("  " + hint)
			}
			errs = append(errs, err)
		case resp.Status != http.StatusOK:
			fmt.Printf("  接続できましたが、/status が %d を返しました。\n", resp.Status)
			errs = append(errs, fmt.Errorf("%s: status %d", n, resp.Status))
		default:
			fmt.Println("  接続できました。")
		}
	}
	return errors.Join(errs...)
}

// serverCert は provisioner の API のサーバー証明書を標準出力に出す。ファイルがなければ自己署名を作る。
func serverCert(stdout, stderr *os.File) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	created, err := certs.EnsureFiles(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSHosts)
	if err != nil {
		return err
	}
	certPEM, err := os.ReadFile(cfg.TLSCertFile)
	if err != nil {
		return err
	}
	f, err := certs.LoadFile(cfg.TLSCertFile, cfg.TLSKeyFile, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		return err
	}
	if _, err := stdout.Write(certPEM); err != nil {
		return err
	}
	leaf := f.Leaf()
	if created {
		fmt.Fprintf(stderr, "自己署名のサーバー証明書を作りました（%s）。\n", cfg.TLSCertFile)
	}
	var sans []string
	sans = append(sans, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	fmt.Fprintf(stderr, "SAN: %s、有効期限 %s\n", strings.Join(sans, ", "), leaf.NotAfter.Local().Format(time.DateOnly))
	fmt.Fprintf(stderr, "SHA-256 フィンガープリント: %s\n", certs.Fingerprint(leaf))
	return nil
}
