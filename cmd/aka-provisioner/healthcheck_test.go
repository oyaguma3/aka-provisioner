package main

import (
	"net"
	"testing"
)

func TestCheckListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, open, _ := net.SplitHostPort(ln.Addr().String())

	// 閉じたポートを用意する（一度開いてすぐ閉じる）。
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closed, _ := net.SplitHostPort(tmp.Addr().String())
	tmp.Close()

	if err := checkListener(t.Context(), ":"+open); err != nil {
		t.Errorf("open port: %v", err)
	}
	if err := checkListener(t.Context(), ":"+closed); err == nil {
		t.Error("closed port: want error")
	}
	if err := checkListener(t.Context(), "no-port"); err == nil {
		t.Error("bad address: want error")
	}
}
