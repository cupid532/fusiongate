package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fusiongate/fusiongate/internal/fusiongate"
)

func envBool(k string) bool { return os.Getenv(k) == "1" || os.Getenv(k) == "true" }

func envInt(k string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(k))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDuration(k string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(k))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

const defaultListenAddr = "127.0.0.1:8787"

func listenAddr(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return defaultListenAddr
}

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
	syscall.Umask(0o077)
	masterKey, err := secretEnv("FUSIONGATE_MASTER_KEY")
	if err != nil {
		log.Fatal(err)
	}
	adminPassword, err := secretEnv("FUSIONGATE_ADMIN_PASSWORD")
	if err != nil {
		log.Fatal(err)
	}
	cfg := fusiongate.Config{
		Addr:                   listenAddr(os.Getenv("FUSIONGATE_ADDR")),
		DataDir:                os.Getenv("FUSIONGATE_DATA_DIR"),
		MasterKey:              masterKey,
		AdminPassword:          adminPassword,
		AllowInsecureUpstreams: envBool("FUSIONGATE_ALLOW_INSECURE_UPSTREAMS"),
		AllowPrivateUpstreams:  envBool("FUSIONGATE_ALLOW_PRIVATE_UPSTREAMS"),
		MaxFailoverAttempts:    envInt("FUSIONGATE_MAX_FAILOVER_ATTEMPTS", 8),
		MaxConcurrentRequests:  envInt("FUSIONGATE_MAX_CONCURRENT_REQUESTS", 64),
		StreamStartTimeout:     envDuration("FUSIONGATE_STREAM_START_TIMEOUT", 12*time.Second),
		StreamIdleTimeout:      envDuration("FUSIONGATE_STREAM_IDLE_TIMEOUT", 5*time.Minute),
		CORSOrigins:            strings.TrimSpace(os.Getenv("FUSIONGATE_CORS_ORIGINS")),
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(".", "data")
	}
	app, err := fusiongate.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	app.StartBackgroundTasks(bgCtx)
	srv := &http.Server{Addr: cfg.Addr, Handler: app.Router(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 3 * time.Minute, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		fmt.Printf("FusionGate listening on http://%s\n", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	app.BeginShutdown()
	bgCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
