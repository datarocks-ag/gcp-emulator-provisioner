// Command gcp-token-stub serves a static OAuth2 token endpoint for local
// Google Cloud emulators.
//
// It exists because some Google client libraries insist on *obtaining* a token
// before they will issue a request, even against an emulator that ignores
// authentication entirely. google-cloud-storage for Java has no
// STORAGE_EMULATOR_HOST equivalent, so an application reaches fake-gcs-server
// through spring.cloud.gcp.storage.host while still holding real
// ServiceAccountCredentials: those sign a JWT and exchange it at the token_uri
// from their key file. Point that token_uri here and the exchange never leaves
// the machine.
//
// The assertion is deliberately not verified. Nothing downstream checks the
// signatures either — fake-gcs-server does not validate them — so verifying
// here would only be theatre. This is a local development stub and must never
// be exposed to anything that matters.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

const (
	defaultAddr        = ":8099"
	defaultAccessToken = "local-emulator-token"
	defaultExpiresIn   = 3600
	// shutdownGrace bounds the wait for in-flight requests on SIGTERM, so
	// `docker compose down` is not held up by a wedged connection.
	shutdownGrace = 5 * time.Second
	// readHeaderTimeout bounds how long a client may take to send headers.
	readHeaderTimeout = 10 * time.Second
	// probeTimeout bounds the self-check used by container healthchecks.
	probeTimeout = 3 * time.Second
)

// tokenResponse is the OAuth2 access token response, in Google's shape.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func main() {
	addrFlag := flag.String("addr", "", "Listen address. Overrides TOKEN_STUB_ADDR (default \":8099\")")
	tokenFlag := flag.String("token", "", "Access token to hand out. Overrides TOKEN_STUB_ACCESS_TOKEN")
	ping := flag.Bool("ping", false,
		"Probe a running stub and exit 0 if it answers. For container healthchecks, since the scratch image has no shell")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	setupLogging()

	addr := firstNonEmpty(*addrFlag, os.Getenv("TOKEN_STUB_ADDR"), defaultAddr)
	token := firstNonEmpty(*tokenFlag, os.Getenv("TOKEN_STUB_ACCESS_TOKEN"), defaultAccessToken)

	expiresIn := defaultExpiresIn
	if raw := os.Getenv("TOKEN_STUB_EXPIRES_IN"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			slog.Error("Invalid TOKEN_STUB_EXPIRES_IN, want a positive number of seconds", "value", raw)
			os.Exit(1)
		}
		expiresIn = parsed
	}

	if *ping {
		if err := probe(addr); err != nil {
			slog.Error("Token stub is not answering", "addr", addr, "error", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, addr, token, expiresIn); err != nil {
		slog.Error("Token stub failed", "error", err)
		os.Exit(1)
	}
}

// probe asks a running stub for a token, so a container healthcheck can call
// the binary itself. The scratch image ships no shell, wget or curl, so there
// is nothing else in it that could make an HTTP request.
func probe(addr string) error {
	// A bare ":8099" is a valid listen address but not a valid URL host.
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}

	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get("http://" + host + "/token")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func run(ctx context.Context, addr, token string, expiresIn int) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(token, expiresIn),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errs := make(chan error, 1)
	go func() {
		slog.Info("Serving local OAuth2 token endpoint",
			"version", version, "addr", addr, "path", "/token",
			"note", "static token, assertions are not verified — local development only")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		slog.Info("Shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// newHandler builds the mux. /token answers GET as well as POST so a container
// healthcheck can probe it without forging a grant request.
func newHandler(token string, expiresIn int) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
			return
		}

		slog.Debug("Issuing token", "method", r.Method, "grant_type", r.FormValue("grant_type"))
		writeJSON(w, http.StatusOK, tokenResponse{
			AccessToken: token,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("Unhandled request", "method", r.Method, "path", r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("Failed to write response", "error", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func setupLogging() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}
