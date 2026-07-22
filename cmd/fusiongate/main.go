package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fusiongate/fusiongate/internal/fusiongate"
)

func envBool(k string) bool { return os.Getenv(k) == "1" || os.Getenv(k) == "true" }

func secretEnv(name string) (string, error) {
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	file := os.Getenv(name + "_FILE")
	if file == "" {
		return "", nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

func main() {
	masterKey, err := secretEnv("FUSIONGATE_MASTER_KEY")
	if err != nil {
		log.Fatal(err)
	}
	adminPassword, err := secretEnv("FUSIONGATE_ADMIN_PASSWORD")
	if err != nil {
		log.Fatal(err)
	}
	cfg := fusiongate.Config{Addr: os.Getenv("FUSIONGATE_ADDR"), DataDir: os.Getenv("FUSIONGATE_DATA_DIR"), MasterKey: masterKey, AdminPassword: adminPassword, AllowInsecureUpstreams: envBool("FUSIONGATE_ALLOW_INSECURE_UPSTREAMS"), AllowPrivateUpstreams: envBool("FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS")}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(".", "data")
	}
	app, err := fusiongate.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()
	srv := &http.Server{Addr: cfg.Addr, Handler: app.Router(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		fmt.Printf("FusionGate listening on http://%s\n", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
