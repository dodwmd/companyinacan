//go:build canpprof

// Footprint diagnostics (docs/measurements/footprint.md). This file is compiled only with
// -tags canpprof, which no release or compose image uses: a normal build has no profiling code.

package obs

import (
	"net"
	"net/http"
	"net/http/pprof"
	"runtime/debug"
	"time"
)

// With the tag, CAN_PPROF_ADDR (loopback only) serves /debug/pprof/ and POST /freeosmemory.
func init() {
	diagStart = func(getenv func(string) string, log Logger) {
		addr := getenv("CAN_PPROF_ADDR")
		if addr == "" {
			return
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !loopbackHost(host) {
			if log != nil {
				log.Warn("CAN_PPROF_ADDR ignored: it must be a loopback host:port")
			}
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index) // also heap, goroutine, allocs
		mux.HandleFunc("POST /freeosmemory", func(http.ResponseWriter, *http.Request) { debug.FreeOSMemory() })
		go func() {
			_ = (&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
		}()
	}
}
