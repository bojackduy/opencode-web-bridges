package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojackduy/deepseek-proxy/internal/deepseek"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "listen address")
	authPath := flag.String("auth", "", "auth file (default ~/.deepseek-proxy/auth.json)")
	statePath := flag.String("state", "", "session journal (default ~/.deepseek-proxy/sessions.json)")
	baseURL := flag.String("base-url", deepseek.DefaultBaseURL, "DeepSeek web origin")
	login := flag.Bool("login", false, "save a DeepSeek web userToken and exit")
	token := flag.String("token", "", "token for -login (default: read stdin)")
	flag.Parse()

	store, err := deepseek.NewStore(*authPath)
	if err != nil {
		log.Fatalf("[deepseek] auth: %v", err)
	}
	if *login {
		tok := strings.TrimSpace(*token)
		if tok == "" {
			fmt.Fprint(os.Stderr, "Paste the DeepSeek userToken (chat.deepseek.com -> DevTools -> Application -> Local Storage -> userToken): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			tok = strings.TrimSpace(line)
		}
		if tok == "" {
			log.Fatal("[deepseek] no token provided")
		}
		if err := store.Set(deepseek.AuthData{Token: tok}, true); err != nil {
			log.Fatalf("[deepseek] save token: %v", err)
		}
		fmt.Printf("saved %s\n", store.Path())
		return
	}

	path := *statePath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("[deepseek] home: %v", err)
		}
		path = filepath.Join(home, ".deepseek-proxy", "sessions.json")
	}
	j, err := openJournal(path)
	if err != nil {
		log.Fatalf("[deepseek] journal: %v", err)
	}

	if store.Token() == "" {
		log.Printf("[deepseek] no token configured; run `deepseek-openai -login` before the first turn")
	}
	srv := &server{client: deepseek.NewClient(store, *baseURL), journal: j}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("[deepseek] listening addr=%s sessions=%d", *addr, j.count())
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[deepseek] server: %v", err)
	}
}
