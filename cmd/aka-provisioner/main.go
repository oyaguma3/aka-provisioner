// aka-provisioner は、EAP-AKA RADIUS PoC（aka-radius）の Provisioning API と
// aka-vecgen の管理API を組み合わせて操作する統合API。設計は docs/design-overview.md を参照。
package main

import (
	"cmp"
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

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/api"
	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/config"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/server"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

// 操作の記録とロックの既定値（設計概要 §9）。
const (
	// operationRetention は、完了した操作の記録を残す期間。
	operationRetention = 7 * 24 * time.Hour
	// retryDelay は、補償・やり直しが失敗したとき、次に試みるまでの時間。失敗するたびに倍にする。
	retryDelay = 30 * time.Second
	// maxRetryDelay は、次に試みるまでの時間の上限。
	maxRetryDelay = 10 * time.Minute
	// giveUpAfter は、自動のやり直しをやめて failed にするまでの時間（操作の作成から。手でやり直すと延びる）。
	giveUpAfter = 24 * time.Hour
	// workerInterval は、ワーカーが未完了の操作を見る間隔。
	workerInterval = 30 * time.Second
	// workerStopTimeout は、停止のときにワーカーが処理中の操作を終えるのを待つ上限。超えたら待たずに終了する
	// （操作の記録が残るので、次の起動でワーカーが続きを行う）。サーバーの停止の上限（10 秒）と合わせて、
	// compose の stop_grace_period（30 秒）に収める。
	workerStopTimeout = 15 * time.Second
)

const usage = `usage: aka-provisioner <command>

commands:
  serve              サーバーを起動する（コマンド省略時の既定）
  check-downstream   下流（本PoCの Provisioning API、aka-vecgen の管理API）に接続できるか確かめる
  gen-client-cert    下流に提示するクライアント証明書と秘密鍵を作る
                     （aka-provisioner gen-client-cert -h で使い方を表示）
  server-cert        provisioner の API のサーバー証明書（PEM）を標準出力に出す（なければ自己署名を作る）。
                     管理クライアント（BFF など）に検証用として渡す
  healthcheck        動いている provisioner の API に接続できるかを確かめる（compose の healthcheck 用）

設定は環境変数で与える。
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

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.CheckServe(); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "version", version)

	openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
	st, err := store.Open(openCtx, store.Options{Addr: cfg.ValkeyAddr, Password: cfg.ValkeyPassword})
	cancelOpen()
	if err != nil {
		return err
	}
	defer st.Close()

	created, err := certs.EnsureFiles(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSHosts)
	if err != nil {
		return err
	}
	if created {
		log.Info("generated self-signed server certificate", "cert", cfg.TLSCertFile, "hosts", cfg.TLSHosts)
	}
	cert, err := certs.LoadFile(cfg.TLSCertFile, cfg.TLSKeyFile, log)
	if err != nil {
		return err
	}
	log.Info("loaded server certificate", "cert", cfg.TLSCertFile,
		"fingerprint", certs.Fingerprint(cert.Leaf()), "not_after", cert.Leaf().NotAfter,
		"dns_names", cert.Leaf().DNSNames, "ip_addresses", cert.Leaf().IPAddresses)
	for fp, name := range cfg.AdminClients {
		log.Info("admin client", "name", name, "fingerprint", fp)
	}
	log.Info("plmn map", "entries", plmnMapString(cfg))

	prov, aka, err := newClients(cfg, log)
	if err != nil {
		return err
	}
	logClient(log, prov.Client)
	if aka != nil {
		logClient(log, aka.Client)
	} else {
		log.Info("aka-vecgen is not configured (PROVISIONER_AKA_URL is empty)")
	}
	// 起動時点で下流が動いていなくても起動は続ける。/status と各操作のエラーで示す。
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	checkProv(checkCtx, log, prov)
	if aka != nil {
		checkAka(checkCtx, log, aka, cfg.AkaAVClientID)
	}
	cancel()

	auditRec := &audit.Recorder{Log: log, Store: st, MaxLen: cfg.AuditMaxLen}
	h := &api.Handler{
		Log:               log,
		MgmtClient:        func(r *http.Request) string { return server.AdminClientName(r, cfg.AdminClients) },
		Prov:              prov,
		AkaAVClientID:     cfg.AkaAVClientID,
		PLMNMap:           cfg.PLMNMap,
		Store:             st,
		DownstreamTimeout: cfg.DownstreamTimeout,
		Audit:             auditRec,
		Version:           version,
		StartedAt:         time.Now().UTC(),
	}
	subs := &subscriber.Service{
		Prov: prov, AVClientID: cfg.AkaAVClientID, PLMN: cfg.PLMNMap, Store: st, Log: log, Audit: auditRec,
		LockTTL: api.LockTTL, Retention: operationRetention,
		RetryDelay: retryDelay, MaxRetryDelay: maxRetryDelay, GiveUpAfter: giveUpAfter,
	}
	if aka != nil {
		// nil の *akaapi.Client をそのまま入れると、nil でないインターフェースになるので分ける。
		h.Aka, subs.Aka = aka, aka
	}
	h.Subscribers = subs
	// やり直しのワーカー。要求が落ちた操作と、補償・やり直しが失敗した操作を続ける（設計概要 §9.3）。
	// サーバーが停止の合図以外で終わった（待ち受けの失敗など）ときもワーカーを止めるよう、別の ctx で動かす。
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	var wg sync.WaitGroup
	wg.Go(func() { subs.RunWorker(workerCtx, workerInterval) })
	err = server.Run(ctx, server.Options{
		Addr:           cfg.Addr,
		GetCertificate: cert.GetCertificate,
		AdminClients:   cfg.AdminClients,
		Handler:        h.Routes(),
		Log:            log,
	})
	// 停止のときは、ワーカーが処理中の操作を終えるのを待つ（途中で終わると補償が中断する）。
	stopWorker()
	waitWorker(&wg, workerStopTimeout, log)
	return err
}

// waitWorker は、ワーカーが戻るのを timeout まで待つ。
func waitWorker(wg *sync.WaitGroup, timeout time.Duration, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Info("worker stopped")
	case <-time.After(timeout):
		log.Warn("worker did not stop in time; the operation in progress will be resumed after restart", "timeout", timeout)
	}
}

// newClients は下流のクライアントを作る。aka-vecgen を扱わない設定なら aka は nil。
func newClients(cfg config.Config, log *slog.Logger) (*provapi.Client, *akaapi.Client, error) {
	prov, err := provapi.New(downstream.Options{
		BaseURL:        cfg.ProvURL,
		ClientCertFile: cfg.ClientCertFile,
		ClientKeyFile:  cfg.ClientKeyFile,
		ServerCertFile: cfg.ProvServerCertFile,
		Timeout:        cfg.DownstreamTimeout,
		Log:            log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("provisioning-api client: %w", err)
	}
	if !cfg.AkaEnabled() {
		return prov, nil, nil
	}
	aka, err := akaapi.New(downstream.Options{
		BaseURL:        cfg.AkaURL,
		ClientCertFile: cfg.ClientCertFile,
		ClientKeyFile:  cfg.ClientKeyFile,
		ServerCertFile: cfg.AkaServerCertFile,
		Timeout:        cfg.DownstreamTimeout,
		Log:            log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("aka-vecgen client: %w", err)
	}
	return prov, aka, nil
}

func logClient(log *slog.Logger, c *downstream.Client) {
	log.Info("downstream client", "downstream", string(c.Name()), "url", c.BaseURL(),
		"client_cert_fingerprint", certs.Fingerprint(c.ClientCertificate()),
		"client_cert_not_after", c.ClientCertificate().NotAfter)
}

func checkProv(ctx context.Context, log *slog.Logger, prov *provapi.Client) {
	if st, err := prov.Status(ctx); err != nil {
		log.Warn("provisioning-api is not available", "error", err, "hint", provapi.Diagnose(err))
	} else {
		log.Info("provisioning-api is available", "server_version", st.Version, "node_name", st.NodeName)
	}
}

func checkAka(ctx context.Context, log *slog.Logger, aka *akaapi.Client, avClientID int64) {
	st, err := aka.Status(ctx)
	if err != nil {
		log.Warn("aka-vecgen is not available", "error", err, "hint", akaapi.Diagnose(err))
		return
	}
	log.Info("aka-vecgen is available", "server_version", st.Version)
	switch c, err := aka.GetAVClient(ctx, avClientID); {
	case err != nil:
		log.Warn("av client of vector-gateway is not available", "av_client_id", avClientID, "error", err)
	case !c.Enabled:
		log.Warn("av client of vector-gateway is disabled", "av_client_id", avClientID, "name", c.Name)
	default:
		log.Info("av client of vector-gateway", "av_client_id", avClientID, "name", c.Name)
	}
}

func plmnMapString(cfg config.Config) string {
	var parts []string
	for _, e := range cfg.PLMNMap.Entries() {
		parts = append(parts, e.PLMN+"="+string(e.KeyStore))
	}
	return strings.Join(parts, ",")
}

// checkDownstream は下流への接続を確かめ、結果を表示する。導入時の確認に使う。
func checkDownstream(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	prov, aka, err := newClients(cfg, log)
	if err != nil {
		return err
	}
	fmt.Println("クライアント証明書のフィンガープリント:", certs.Fingerprint(prov.ClientCertificate()))
	fmt.Println("PLMN マップ:", cmp.Or(plmnMapString(cfg), "（なし。すべての加入者の鍵を本PoCに置く）"))

	var errs []error
	fmt.Println()
	fmt.Println("本PoCの Provisioning API:", prov.BaseURL())
	if st, err := prov.Status(ctx); err != nil {
		printFailure(provapi.Diagnose(err), err)
		errs = append(errs, err)
	} else {
		fmt.Printf("  接続できました。provisioning-api %s（ノード %s、加入者 %d、RADIUSクライアント %d、認可ポリシー %d）\n",
			st.Version, st.NodeName, st.SubscriberCount, st.ClientCount, st.PolicyCount)
	}

	fmt.Println()
	if aka == nil {
		fmt.Println("aka-vecgen の管理API: 設定なし（PROVISIONER_AKA_URL が空）")
		return errors.Join(errs...)
	}
	fmt.Println("aka-vecgen の管理API:", aka.BaseURL())
	st, err := aka.Status(ctx)
	if err != nil {
		printFailure(akaapi.Diagnose(err), err)
		return errors.Join(append(errs, err)...)
	}
	fmt.Printf("  接続できました。aka-vecgen %s（加入者 %d、AVクライアント %d）\n", st.Version, st.SubscriberCount, st.ClientCount)
	switch c, err := aka.GetAVClient(ctx, cfg.AkaAVClientID); {
	case downstream.CauseOf(err) == akaapi.CauseClientNotFound:
		fmt.Printf("  vector-gateway の AVクライアント（ID %d）が aka-vecgen にありません。PROVISIONER_AKA_AV_CLIENT_ID を確認してください。\n",
			cfg.AkaAVClientID)
		errs = append(errs, err)
	case err != nil:
		printFailure("", err)
		errs = append(errs, err)
	case !c.Enabled:
		fmt.Printf("  vector-gateway の AVクライアント: ID %d（%s）は無効になっています。\n", c.ID, c.Name)
		errs = append(errs, errors.New("av client is disabled"))
	default:
		fmt.Printf("  vector-gateway の AVクライアント: ID %d（%s、証明書の有効期限 %s）\n",
			c.ID, c.Name, c.NotAfter.Local().Format(time.DateOnly))
	}
	return errors.Join(errs...)
}

func printFailure(hint string, err error) {
	fmt.Println("  接続できませんでした:", err)
	if hint != "" {
		fmt.Println("  " + hint)
	}
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
