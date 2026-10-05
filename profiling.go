package main

import (
	"net"
	"net/http"
	"net/http/pprof"
	"os"
)

// startProfiler serves Go's pprof endpoints on PI9696_PPROF (e.g.
// "127.0.0.1:6060") when it is set, so CPU, heap and goroutine profiles of
// the running recorder can be taken without a rebuild:
//
//	go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
//
// Off by default. It has no authentication, so bind it to loopback; it is
// deliberately a separate listener from the WebUI, never mounted on it.
func startProfiler() {
	addr := os.Getenv("PI9696_PPROF")
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		logErrorf("profiler: listen %s: %v", addr, err)
		return
	}
	logInfof("profiler on http://%s/debug/pprof/", l.Addr())
	go http.Serve(l, mux)
}
