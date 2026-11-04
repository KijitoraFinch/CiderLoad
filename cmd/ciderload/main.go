package main

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"ciderload/internal/app"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	var interrupted atomic.Int32
	go func() {
		select {
		case sig := <-ch:
			interrupted.Store(int32(sig.(syscall.Signal)))
			cancel()
		case <-ctx.Done():
		}
	}()
	code := app.RunProcess(ctx, os.Args[1:])
	signal.Stop(ch)
	cancel()
	if sig := interrupted.Load(); sig != 0 {
		code = 128 + int(sig)
	}
	os.Exit(code)
}
