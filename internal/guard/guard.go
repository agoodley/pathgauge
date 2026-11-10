// Package guard keeps a bug in one connection, request or background loop
// from ending the whole daemon.
//
// A panic in any goroutine exits the process, which for the server takes
// the dashboard, the database writer and every agent session with it. The
// goroutines that handle input from the network recover instead, log the
// panic with its stack and give up only the work they were doing.
package guard

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Recovered logs a recovered panic and reports whether there was one. Call
// it from a deferred function with the result of recover():
//
//	defer func() {
//		if guard.Recovered(log, "data connection", recover()) {
//			conn.Close()
//		}
//	}()
func Recovered(log *slog.Logger, what string, r any) bool {
	if r == nil {
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	log.Error("recovered from a panic (this is a bug, please report it)", "in", what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	return true
}

// Go runs fn in a new goroutine that survives a panic in fn.
func Go(log *slog.Logger, what string, fn func()) {
	go func() {
		defer func() { Recovered(log, what, recover()) }()
		fn()
	}()
}

// restartDelay spaces out restarts of a loop that keeps panicking.
var restartDelay = time.Second

// Loop runs fn, which should return only when ctx is cancelled, and starts
// it again if it panics. It returns when fn returns normally or ctx ends.
func Loop(ctx context.Context, log *slog.Logger, what string, fn func(context.Context)) {
	for {
		if !panicked(log, what, func() { fn(ctx) }) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
		}
	}
}

func panicked(log *slog.Logger, what string, fn func()) (p bool) {
	defer func() { p = Recovered(log, what, recover()) }()
	fn()
	return false
}
