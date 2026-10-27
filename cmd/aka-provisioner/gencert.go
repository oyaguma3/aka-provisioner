package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

// clientNamePattern は、下流の管理クライアントの識別名（PROVISIONING_API_ADMIN_CLIENTS / VECGEN_ADMIN_CLIENTS）の形式。
var clientNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const genClientCertUsage = `usage: aka-provisioner gen-client-cert -name <識別名> [-days 825] [-out-cert <file>] [-out-key <file>]

下流（本PoCの Provisioning API と aka-vecgen の管理API）に提示するクライアント証明書（自己署名、ECDSA P-256）と
秘密鍵を作る。1 つの証明書を両方の下流に登録して使う。
出力先を省略すると、証明書と秘密鍵を続けて標準出力に出す（1 つの PEM ファイルとして使える）。
フィンガープリントと、下流の .env に書く値（PROVISIONING_API_ADMIN_CLIENTS、VECGEN_ADMIN_CLIENTS）を標準エラーに出す。

options:
`

// genClientCert はクライアント証明書と秘密鍵を作る。
// コンテナでは docker compose run で実行し、標準出力をホストのファイルに受ける使い方を想定する。
func genClientCert(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen-client-cert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, genClientCertUsage)
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "証明書の CommonName。下流の監査ログの mgmt_client になる識別名と同じにする（英数字と . _ - の 64 文字まで）")
	days := fs.Int("days", 825, "有効日数")
	outCert := fs.String("out-cert", "", "証明書の出力先（省略時は標準出力）")
	outKey := fs.String("out-key", "", "秘密鍵の出力先（省略時は標準出力。ファイルはパーミッション 600 で作る）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case !clientNamePattern.MatchString(*name):
		return fmt.Errorf("-name must match %s", clientNamePattern)
	case *days < 1:
		return errors.New("-days must be a positive integer")
	}

	certPEM, keyPEM, err := certs.SelfSignedClient(*name, time.Duration(*days)*24*time.Hour)
	if err != nil {
		return err
	}
	if err := writeOutput(stdout, *outCert, certPEM, 0o644); err != nil {
		return err
	}
	if err := writeOutput(stdout, *outKey, keyPEM, 0o600); err != nil {
		return err
	}

	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	fp := certs.Fingerprint(leaf)
	fmt.Fprintf(stderr, "クライアント証明書を作りました（CN=%s、有効期限 %s）。\n", *name, leaf.NotAfter.Local().Format(time.DateOnly))
	fmt.Fprintf(stderr, "SHA-256 フィンガープリント: %s\n", fp)
	fmt.Fprintln(stderr, "下流の .env に次の値を登録してください（他の登録があればカンマ区切りで加える）:")
	fmt.Fprintln(stderr, "  本PoC（aka-radius）:")
	fmt.Fprintf(stderr, "PROVISIONING_API_ADMIN_CLIENTS=%s=%s\n", *name, fp)
	fmt.Fprintln(stderr, "  aka-vecgen（鍵を aka-vecgen に置く加入者を扱う場合）:")
	fmt.Fprintf(stderr, "VECGEN_ADMIN_CLIENTS=%s=%s\n", *name, fp)
	return nil
}

// writeOutput は path が空なら stdout に、そうでなければ新しいファイルに書く。
// 既存のファイル（使用中の秘密鍵など）を誤って上書きしないよう、ファイルがあればエラーにする。
func writeOutput(stdout io.Writer, path string, data []byte, perm os.FileMode) error {
	if path == "" {
		_, err := stdout.Write(data)
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
