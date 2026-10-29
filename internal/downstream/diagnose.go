package downstream

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
)

// hints は、下流に接続できなかったときに示す対処の文。%[1]s に下流の名前が入る。
var hints = struct {
	hostnameMismatch, unknownServer, serverCertInvalid, clientRejected, connectionReset, nameNotFound, dialFailed, timeout string
}{
	hostnameMismatch:  "%[1]s のサーバー証明書の SAN に、登録簿の url のホスト名か IP アドレスが入っていません。",
	unknownServer:     "%[1]s のサーバー証明書が、登録簿の serverCert と一致しません（%[1]s が証明書を作り直したら、登録簿の証明書も差し替えて SIGHUP を送ってください）。",
	serverCertInvalid: "%[1]s のサーバー証明書が有効期間外です。",
	clientRejected:    "%[1]s が provisioner のクライアント証明書を受け付けませんでした。%[1]s の管理クライアントのピンのファイルに、provisioner のクライアント証明書のフィンガープリントがあるか確かめてください。",
	connectionReset:   "%[1]s が接続を切りました。%[1]s の管理クライアントのピンのファイルに、provisioner のクライアント証明書のフィンガープリントがあるか確かめてください。",
	nameNotFound:      "%[1]s のホスト名 %[2]s を解決できません（同じホストの構成では、%[1]s が動いているか、共有のネットワークに入っているか確かめてください）。",
	dialFailed:        "%[1]s に接続できません。%[1]s が動いているか、登録簿の url のアドレスとポート、ファイアウォールを確かめてください。",
	timeout:           "%[1]s から時間内に応答がありません（PROVISIONER_DOWNSTREAM_TIMEOUT）。",
}

// Diagnose は、下流に接続できなかったときの原因の見当を、運用者向けの文で返す。
// 見当がつかなければ空文字列を返す。
func Diagnose(name Name, err error) string {
	if err == nil {
		return ""
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return fmt.Sprintf(hints.hostnameMismatch, name)
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return fmt.Sprintf(hints.unknownServer, name)
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return fmt.Sprintf(hints.serverCertInvalid, name)
	}
	// 相手から TLS のアラートを受け取ると、Op が "remote error" の *net.OpError になる。
	// 下流は未登録・有効期間外・証明書なしの接続を bad_certificate 等で拒否する。
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "remote error" &&
		(strings.Contains(opErr.Err.Error(), "bad certificate") || strings.Contains(opErr.Err.Error(), "certificate required")) {
		return fmt.Sprintf(hints.clientRejected, name)
	}
	// TLS 1.3 では、下流がクライアント証明書を検証して拒否する前に、provisioner はハンドシェイクを終えて
	// リクエストを送っている。下流が読まずに接続を閉じると、拒否のアラートより先に接続のリセットが届き、
	// アラートを受け取れないことがある（手元の確認で 16 回に 1 回ほど）。
	if errors.Is(err, syscall.ECONNRESET) {
		return fmt.Sprintf(hints.connectionReset, name)
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
		// 同一ホストの構成では、下流のコンテナが止まっているときも名前を解決できなくなる。
		return fmt.Sprintf(hints.nameNotFound, name, dnsErr.Name)
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "dial" {
		return fmt.Sprintf(hints.dialFailed, name)
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return fmt.Sprintf(hints.timeout, name)
	}
	return ""
}
