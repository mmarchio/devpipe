package service

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// containPanic is deferred at the top of every background goroutine
// (WBS 18.196): a panic is logged with its stack and the goroutine ends;
// the process and every other project keep running. Work the goroutine
// was doing is left in its durable state for reconciliation — never
// retried blindly (WBS 18.197).
func containPanic(log *slog.Logger, what string, args ...any) {
	v := recover()
	if v == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Error("background "+what+" panicked; contained", append(args, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))...)
}
