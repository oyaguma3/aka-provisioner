package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStartAndCheck(t *testing.T) {
	var down atomic.Bool
	ping := func(ctx context.Context) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	s, err := Start("127.0.0.1:0", ping, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := Check(t.Context(), s.Addr()); err != nil {
		t.Errorf("Check() = %v", err)
	}
	// 0.0.0.0 で待ち受けていてもループバックに問い合わせる
	_, port, _ := strings.Cut(s.Addr(), ":")
	if err := Check(t.Context(), "0.0.0.0:"+port); err != nil {
		t.Errorf("Check(0.0.0.0) = %v", err)
	}

	down.Store(true)
	if err := Check(t.Context(), s.Addr()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Check() with Valkey down = %v", err)
	}

	_ = s.Close()
	if err := Check(t.Context(), s.Addr()); err == nil {
		t.Error("Check() after Close: want error")
	}
	if err := Check(t.Context(), "nonsense"); err == nil {
		t.Error("Check(bad addr): want error")
	}
}

func TestStartListenError(t *testing.T) {
	s, err := Start("127.0.0.1:0", func(context.Context) error { return nil }, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := Start(s.Addr(), nil, nil); err == nil {
		t.Error("Start on a used port: want error")
	}
}
