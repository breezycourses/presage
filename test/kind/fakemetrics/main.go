// Command fakemetrics serves a Prometheus-shaped endpoint whose value comes
// from a file, so a cluster test can move "demand" and watch presage respond.
//
// Deliberately not a real Prometheus: this suite asks whether presage can
// scale a workload on a live cluster, not whether Prometheus works.
//
// It is a Go binary rather than a python:3.12-slim pod because kind side-loads
// images with `--all-platforms`, and a multi-arch image pulled from a registry
// only has blobs for the host platform locally -- so loading it fails with
// "content digest ...: not found". An image built locally from this source is
// single-platform and loads cleanly.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func demand(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 100
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		return 100
	}
	return v
}

func main() {
	path := os.Getenv("DEMAND_FILE")
	if path == "" {
		path = "/config/demand"
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":9090"
	}

	write := func(w http.ResponseWriter, payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}

	http.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		start, _ := strconv.ParseFloat(r.FormValue("start"), 64)
		end, _ := strconv.ParseFloat(r.FormValue("end"), 64)
		step, _ := strconv.ParseFloat(r.FormValue("step"), 64)
		if step <= 0 {
			http.Error(w, "bad step", http.StatusBadRequest)
			return
		}
		v := fmt.Sprintf("%.3f", demand(path))

		values := [][2]any{}
		for t := start; t <= end; t += step {
			values = append(values, [2]any{t, v})
		}
		write(w, map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix",
			"result": []any{map[string]any{
				"metric": map[string]string{"job": "fake"}, "values": values,
			}},
		}})
	})

	http.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		write(w, map[string]any{"status": "success", "data": map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]string{},
				"value":  [2]any{float64(time.Now().Unix()), fmt.Sprintf("%.3f", demand(path))},
			}},
		}})
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
