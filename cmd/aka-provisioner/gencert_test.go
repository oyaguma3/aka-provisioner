package main

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

func TestGenClientCertStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := genClientCert([]string{"-name", "provisioner"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	// 標準出力は証明書と秘密鍵を続けた 1 つの PEM で、そのまま鍵のペアとして読める。
	cert, err := tls.X509KeyPair(stdout.Bytes(), stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if cert.Leaf.Subject.CommonName != "provisioner" {
		t.Errorf("CN = %q", cert.Leaf.Subject.CommonName)
	}
	// 下流のピンのファイルに書く行を出す。
	fp := certs.Fingerprint(cert.Leaf)
	for _, want := range []string{"\nprovisioner=" + fp + "\n"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestGenClientCertFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	var stdout, stderr bytes.Buffer
	args := []string{"-name", "provisioner", "-days", "30", "-out-cert", certPath, "-out-key", keyPath}
	if err := genClientCert(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q", stdout.String())
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore).Hours(); d != 30*24 {
		t.Errorf("validity = %vh", d)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key perm: %v, %v", fi.Mode().Perm(), err)
	}

	// 既存のファイルは上書きしない。
	if err := genClientCert(args, &stdout, &stderr); err == nil {
		t.Error("overwrite: want error")
	}
}

func TestGenClientCertBadArgs(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-name", "bad name"},
		{"-name", strings.Repeat("a", 65)},
		{"-name", "provisioner", "-days", "0"},
		{"-name", "provisioner", "extra"},
		{"-unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if err := genClientCert(args, &stdout, &stderr); err == nil {
			t.Errorf("%q: want error", args)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q: stdout = %q", args, stdout.String())
		}
	}
	// -h は使い方を出して正常終了する。
	var stdout, stderr bytes.Buffer
	if err := genClientCert([]string{"-h"}, &stdout, &stderr); err != nil || !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("-h: err = %v, stderr = %q", err, stderr.String())
	}
}
