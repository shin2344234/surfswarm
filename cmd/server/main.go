// surfswarm-server accepts agent connections, runs tests, and serves the UI.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shin2344234/surfswarm/internal/server"
)

var version = "dev"

func main() {
	// Flags win over environment variables, which suit containers.
	listen := flag.String("listen", envOr("SURFSWARM_LISTEN", ":8080"), "address to listen on (env SURFSWARM_LISTEN)")
	token := flag.String("token", envOr("SURFSWARM_TOKEN", ""), "token agents must present; empty accepts any agent (env SURFSWARM_TOKEN)")
	dataDir := flag.String("data", envOr("SURFSWARM_DATA", "data"), "directory for edited and custom URL lists; empty keeps edits in memory (env SURFSWARM_DATA)")
	uiPassword := flag.String("ui-password", envOr("SURFSWARM_UI_PASSWORD", ""), "password for the web UI and API (basic auth, any user name); empty leaves them open (env SURFSWARM_UI_PASSWORD)")
	flag.Parse()

	srv, err := server.New(server.Config{Token: *token, DataDir: *dataDir, UIPassword: *uiPassword})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()

	if *token == "" {
		log.Printf("warning: no -token set, any agent can connect")
	}
	if *uiPassword == "" {
		log.Printf("warning: no -ui-password set, anyone who can reach this port can start tests")
	}
	sizes := srv.ListSizes()
	custom := 0
	for name := range sizes {
		if name != "browse" && name != "download" && name != "max" {
			custom++
		}
	}
	log.Printf("surfswarm-server %s: lists browse=%d download=%d max=%d custom=%d (data in %q), listening on %s, UI at http://%s/",
		version, sizes["browse"], sizes["download"], sizes["max"], custom, *dataDir, *listen, displayAddr(*listen))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Printf("server stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}
