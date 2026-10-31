// P1 meter dashboard & logger for the HomeWizard P1 Meter (API v2).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8001", "listen address, e.g. :8001 or 127.0.0.1:8001")
	dataDir := flag.String("data", "./data", "folder for config.json and logs")
	auth := flag.String("auth", os.Getenv("P1_AUTH"), "optional HTTP basic auth as user:password (or env P1_AUTH)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := NewConfigStore(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	meter := NewMeter(cfg)
	logs := NewLogs(ctx, meter, filepath.Join(*dataDir, "logs"))
	if err := logs.Start(); err != nil {
		log.Fatalf("logs: %v", err)
	}
	hub := NewHub(meter)
	logs.OnLive = func(m map[string]any, at time.Time, err error) {
		var payload map[string]any
		if err != nil {
			payload = map[string]any{"error": err.Error()}
		} else {
			payload = map[string]any{"at": at.UnixMilli(), "m": m}
		}
		b, _ := json.Marshal(payload)
		hub.broadcast(b)
	}

	srv := &Server{cfg: cfg, meter: meter, logs: logs, hub: hub, panels: NewPanelStore(filepath.Join(*dataDir, "panels.json")), auth: *auth}
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()

	c := cfg.Get()
	log.Printf("P1 meter %s — data in %s", version, *dataDir)
	if c.Host == "" {
		log.Printf("no meter address yet: open the web UI and fill it in")
	}
	port := hs.Addr
	if len(port) > 0 && port[0] == ':' {
		for _, ip := range lanAddrs() {
			log.Printf("open http://%s%s", ip, port)
		}
		log.Printf("open http://localhost%s", port)
	} else {
		log.Printf("listening on %s", port)
	}
	if *auth == "" && port != "" && port[0] == ':' {
		log.Printf("note: no -auth set; anyone on your network can use the UI (it can reboot the meter)")
	}
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("stopped")
}
