package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// --- Data types ---

type KeyEntry struct {
	Key     string `json:"key"`
	Created string `json:"created"`
}

type KeyStore struct {
	Users map[string]KeyEntry `json:"users"`
}

type contextKey string

const ctxUser contextKey = "user"

// --- Key store functions ---

func loadKeys(path string) (*KeyStore, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &KeyStore{Users: make(map[string]KeyEntry)}, nil
		}
		return nil, err
	}
	var ks KeyStore
	if err := json.Unmarshal(data, &ks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if ks.Users == nil {
		ks.Users = make(map[string]KeyEntry)
	}
	return &ks, nil
}

func saveKeys(path string, ks *KeyStore) error {
	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func generateKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return "pk-" + hex.EncodeToString(b)
}

func buildLookup(ks *KeyStore) map[string]string {
	m := make(map[string]string, len(ks.Users))
	for user, entry := range ks.Users {
		m[entry.Key] = user
	}
	return m
}

// --- Path helpers ---

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("cannot determine home directory: %v", err)
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func loadAPIKey(path string) string {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("cannot read API key file %s: %v", path, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		log.Fatalf("API key file is empty: %s", path)
	}
	return key
}

// --- CLI subcommands ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 4000, "listen port")
	keysPath := fs.String("keys", "keys.json", "path to keys file")
	apiKeyPath := fs.String("apikey", "~/.config/stanford-ai/key.dat", "path to API key file")
	upstream := fs.String("upstream", "https://aiapi-prod.stanford.edu", "upstream base URL")
	fs.Parse(args)

	apiKey := loadAPIKey(*apiKeyPath)

	ks, err := loadKeys(*keysPath)
	if err != nil {
		log.Fatalf("cannot load keys: %v", err)
	}
	lookup := buildLookup(ks)
	if len(lookup) == 0 {
		log.Printf("WARNING: no proxy keys in %s — all requests will be rejected", *keysPath)
	} else {
		log.Printf("loaded %d proxy key(s) from %s", len(lookup), *keysPath)
	}

	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstreamURL)
			pr.Out.Host = upstreamURL.Host
			pr.Out.Header.Set("Authorization", "Bearer "+apiKey)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[%s] %s %s -> 502 (upstream error: %v)",
				r.Context().Value(ctxUser), r.Method, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":"upstream unavailable"}`)
		},
		ModifyResponse: func(resp *http.Response) error {
			user := resp.Request.Context().Value(ctxUser)
			log.Printf("[%s] %s %s -> %d",
				user, resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	})
	mux.Handle("/", authMiddleware(lookup, proxy))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("proxy listening on http://0.0.0.0:%d", *port)
		log.Printf("forwarding to %s (key injected)", *upstream)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
}

func cmdAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	keysPath := fs.String("keys", "keys.json", "path to keys file")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: stanford-proxy add <username> [-keys path]")
		os.Exit(1)
	}
	username := fs.Arg(0)

	ks, err := loadKeys(*keysPath)
	if err != nil {
		log.Fatalf("cannot load keys: %v", err)
	}

	if _, exists := ks.Users[username]; exists {
		log.Fatalf("user %q already exists — revoke first to regenerate", username)
	}

	key := generateKey()
	ks.Users[username] = KeyEntry{
		Key:     key,
		Created: time.Now().UTC().Format(time.RFC3339),
	}

	if err := saveKeys(*keysPath, ks); err != nil {
		log.Fatalf("cannot save keys: %v", err)
	}

	fmt.Printf("Created key for %s: %s\n", username, key)
}

func cmdRevoke(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	keysPath := fs.String("keys", "keys.json", "path to keys file")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: stanford-proxy revoke <username> [-keys path]")
		os.Exit(1)
	}
	username := fs.Arg(0)

	ks, err := loadKeys(*keysPath)
	if err != nil {
		log.Fatalf("cannot load keys: %v", err)
	}

	if _, exists := ks.Users[username]; !exists {
		log.Fatalf("user %q not found", username)
	}

	delete(ks.Users, username)

	if err := saveKeys(*keysPath, ks); err != nil {
		log.Fatalf("cannot save keys: %v", err)
	}

	fmt.Printf("Revoked key for %s\n", username)
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	keysPath := fs.String("keys", "keys.json", "path to keys file")
	fs.Parse(args)

	ks, err := loadKeys(*keysPath)
	if err != nil {
		log.Fatalf("cannot load keys: %v", err)
	}

	if len(ks.Users) == 0 {
		fmt.Println("No keys.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tKEY PREFIX\tCREATED")
	for user, entry := range ks.Users {
		prefix := entry.Key
		if len(prefix) > 11 {
			prefix = prefix[:11] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", user, prefix, entry.Created)
	}
	w.Flush()
}

// --- HTTP middleware ---

func authMiddleware(lookup map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		user, ok := lookup[token]
		if !ok {
			http.Error(w, `{"error":"invalid proxy key"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- Main ---

func usage() {
	fmt.Fprintln(os.Stderr, `usage: stanford-proxy <command> [flags]

commands:
  serve    start the proxy server
  add      add a user and generate a proxy key
  revoke   revoke a user's proxy key
  list     list all proxy keys`)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		cmdServe(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "add":
		cmdAdd(os.Args[2:])
	case "revoke":
		cmdRevoke(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	default:
		usage()
	}
}
