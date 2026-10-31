// chatgpt-proxy — an MCP server that bridges AI agents to ChatGPT Web.
//
// Agents (Hermes, Claude Desktop, Codebuff-style assistants, ...) connect
// over MCP (stdio or Streamable HTTP) and use ask_chatgpt to send prompts
// to the user's ChatGPT Web session, driven through a dedicated Chrome
// profile via CDP — free, no API key.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"chatgpt-proxy/internal/chatgpt"
	"chatgpt-proxy/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "chatgpt-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	transport := flag.String("transport", "stdio", "transport: stdio or http")
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (http transport)")
	chromePort := flag.Int("chrome-port", chatgpt.DefaultChromePort, "Chrome remote debugging port")
	profile := flag.String("profile", defaultProfile(), "Chrome profile directory (keeps the ChatGPT login)")
	chrome := flag.String("chrome", "", "Chrome binary path (auto-detected when empty)")
	open := flag.Bool("open", false, "just open Chrome with the profile and exit (first-time login)")
	waitLogin := flag.Bool("wait-login", false, "open Chrome and poll until the user is logged in at chatgpt.com, then exit")
	background := flag.Bool("background", false, "run Chrome window in background (off-screen / minimized)")
	flag.Parse()

	if *open && *waitLogin {
		return errors.New("--open and --wait-login cannot be used together")
	}
	if *transport != "stdio" && *transport != "http" {
		return fmt.Errorf("unknown transport %q (want stdio or http)", *transport)
	}

	cg := chatgpt.New(*chromePort, *profile, *chrome)
	cg.Background = *background
	cg.DedicatedTab, cg.SetupLocks = true, true
	defer cg.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *open {
		if err := openBrowser(ctx, cg); err != nil {
			return fmt.Errorf("open chrome: %w", err)
		}
		fmt.Println("Chrome opened with profile:", *profile)
		fmt.Println("Log in at chatgpt.com in that window once — the session is kept.")
		return nil
	}

	if *waitLogin {
		fmt.Println("Opening Chrome — please log in at chatgpt.com in the new window.")
		fmt.Println("The session is saved to", *profile, "and reused on later runs.")
		if err := openBrowser(ctx, cg); err != nil {
			return fmt.Errorf("open chrome: %w", err)
		}

		deadline := time.NewTimer(5 * time.Minute)
		defer deadline.Stop()

		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return errors.New("timed out after 5 minutes waiting for login")
			case <-ticker.C:
				st, err := cg.LoginState(ctx)
				if err != nil {
					log.Printf("login check: %v", err)
				} else if st.LoggedIn {
					fmt.Println("✅ Logged in — session saved.")
					return nil
				}
			}
		}
	}

	log.SetOutput(os.Stderr)
	h := server.NewHandler(cg, *profile)

	switch *transport {
	case "stdio":
		log.Printf("chatgpt-proxy %s — MCP stdio server (chrome port %d, profile %s)", server.Version, *chromePort, *profile)
		if err := h.RunStdio(ctx); err != nil {
			return fmt.Errorf("stdio: %w", err)
		}
	case "http":
		log.Printf("chatgpt-proxy %s — MCP Streamable HTTP on http://%s/mcp (chrome port %d, profile %s)", server.Version, *addr, *chromePort, *profile)
		srv := &http.Server{
			Addr:              *addr,
			Handler:           h.NewHTTPHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				log.Printf("http shutdown: %v", err)
			}
		}()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http: %w", err)
		}
	}

	return nil
}

func defaultProfile() string {
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		return ".chatgpt-proxy"
	}
	return filepath.Join(dir, ".chatgpt-proxy")
}

// openBrowser launches Chrome with the dedicated profile so the user can log
// in once. It reuses the same launch path as the chatgpt client.
func openBrowser(ctx context.Context, cg *chatgpt.Client) error {
	// Status() attaches or launches Chrome and navigates to chatgpt.com.
	_, err := cg.Status(ctx)
	return err
}
