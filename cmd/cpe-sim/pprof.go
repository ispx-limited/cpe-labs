package main

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"
)

// servePprof serves Go's runtime profiles on addr for as long as the
// process runs. Its own mux, so nothing else the process serves picks
// up the handlers, and nothing here listens unless --pprof-addr is set.
func servePprof(addr string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		logger.Info("pprof listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("pprof stopped", "err", err)
		}
	}()
}
