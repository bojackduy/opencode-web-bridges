// meta-openai translates Meta AI Web replies into OpenAI chat completions.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bojackduy/meta-proxy/internal/meta"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8791", "listen address")
	port := flag.Int("chrome-port", meta.DefaultChromePort, "Chrome debugging port")
	profile := flag.String("profile", defaultProfile(), "Chrome profile")
	chrome := flag.String("chrome", "", "Chrome binary")
	background := flag.Bool("background", false, "minimize Chrome")
	model := flag.String("model", "meta-web", "model ID")
	maxChars := flag.Int("max-chars", 200000, "maximum bytes per Meta turn")
	maxPaste := flag.Int("max-paste-bytes", 0, "maximum bytes one submission may carry (0 disables, per-turn budget alone applies)")
	state := flag.String("state", filepath.Join(defaultProfile(), "openai-sessions.json"), "persistent session state")
	flag.Parse()

	cg := meta.New(*port, *profile, *chrome)
	cg.Background = *background
	defer cg.Close()

	h := &handler{client: cg, model: *model, maxChars: *maxChars, maxPaste: *maxPaste, statePath: *state, profileDir: *profile, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	if err := h.load(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *addr, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("[openai] listening=%s model=%s state=%s chrome_port=%d", *addr, *model, *state, *port)
	log.Printf("[openai] REPAIR: sign in in ~/.meta-proxy Chrome; CDP: localhost:9224/json/version")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func defaultProfile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".meta-proxy")
}
