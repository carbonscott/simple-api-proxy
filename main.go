package main

import (
	"context"
	"crypto/rand"
	"database/sql"
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

	_ "modernc.org/sqlite"
)

// --- Data types ---

type KeyStore interface {
	Add(username string) (string, error)
	Revoke(username string) error
	List() ([]KeyInfo, error)
	Lookup(token string) (string, bool)
}

type KeyInfo struct {
	Username  string `json:"username"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at"`
}

type contextKey string

const ctxUser contextKey = "user"

// --- SQLite store ---

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS keys (
		username   TEXT PRIMARY KEY,
		key        TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Add(username string) (string, error) {
	key := generateKey()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		"INSERT INTO keys (username, key, created_at) VALUES (?, ?, ?)",
		username, key, createdAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return "", fmt.Errorf("user %q already exists", username)
		}
		return "", err
	}
	return key, nil
}

func (s *SQLiteStore) Revoke(username string) error {
	res, err := s.db.Exec("DELETE FROM keys WHERE username = ?", username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", username)
	}
	return nil
}

func (s *SQLiteStore) List() ([]KeyInfo, error) {
	rows, err := s.db.Query("SELECT username, key, created_at FROM keys ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []KeyInfo
	for rows.Next() {
		var username, key, createdAt string
		if err := rows.Scan(&username, &key, &createdAt); err != nil {
			return nil, err
		}
		prefix := key
		if len(prefix) > 11 {
			prefix = prefix[:11] + "..."
		}
		keys = append(keys, KeyInfo{
			Username:  username,
			KeyPrefix: prefix,
			CreatedAt: createdAt,
		})
	}
	return keys, rows.Err()
}

func (s *SQLiteStore) Lookup(token string) (string, bool) {
	var username string
	err := s.db.QueryRow("SELECT username FROM keys WHERE key = ?", token).Scan(&username)
	if err != nil {
		return "", false
	}
	return username, true
}

// insertRaw inserts a key entry directly (used by migrate command).
func (s *SQLiteStore) insertRaw(username, key, createdAt string) error {
	_, err := s.db.Exec(
		"INSERT INTO keys (username, key, created_at) VALUES (?, ?, ?)",
		username, key, createdAt,
	)
	return err
}

// --- Key generation ---

func generateKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return "pk-" + hex.EncodeToString(b)
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

func loadAdminToken(filePath string) string {
	if filePath != "" {
		filePath = expandHome(filePath)
		data, err := os.ReadFile(filePath)
		if err != nil {
			log.Fatalf("cannot read admin token file %s: %v", filePath, err)
		}
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token
		}
	}
	return os.Getenv("ADMIN_TOKEN")
}

// --- JSON migration helper ---

type jsonKeyEntry struct {
	Key     string `json:"key"`
	Created string `json:"created"`
}

type jsonKeyStore struct {
	Users map[string]jsonKeyEntry `json:"users"`
}

func loadKeysJSON(path string) (*jsonKeyStore, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ks jsonKeyStore
	if err := json.Unmarshal(data, &ks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if ks.Users == nil {
		ks.Users = make(map[string]jsonKeyEntry)
	}
	return &ks, nil
}

// --- CLI subcommands ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 4000, "listen port")
	dbPath := fs.String("db", "keys.db", "path to SQLite database")
	apiKeyPath := fs.String("apikey", "~/.config/simple-api-proxy/key.dat", "path to API key file")
	upstream := fs.String("upstream", "https://aiapi-prod.stanford.edu", "upstream base URL")
	adminTokenFile := fs.String("admin-token-file", "", "path to admin token file (or set ADMIN_TOKEN env)")
	fs.Parse(args)

	apiKey := loadAPIKey(*apiKeyPath)

	store, err := NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer store.Close()

	keys, _ := store.List()
	if len(keys) == 0 {
		log.Printf("WARNING: no proxy keys in %s — all requests will be rejected", *dbPath)
	} else {
		log.Printf("loaded %d proxy key(s) from %s", len(keys), *dbPath)
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

	adminToken := loadAdminToken(*adminTokenFile)
	if adminToken != "" {
		adminAuth := adminAuthMiddleware(adminToken)
		mux.Handle("GET /admin/keys", adminAuth(handleAdminListKeys(store)))
		mux.Handle("POST /admin/keys", adminAuth(handleAdminAddKey(store)))
		mux.Handle("DELETE /admin/keys/{username}", adminAuth(handleAdminRevokeKey(store)))
		log.Println("admin API enabled on /admin/keys")
	} else {
		log.Println("WARNING: no admin token configured — admin API disabled")
	}

	mux.Handle("/", authMiddleware(store, proxy))

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
	dbPath := fs.String("db", "keys.db", "path to SQLite database")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: simple-api-proxy add <username> [-db path]")
		os.Exit(1)
	}
	username := fs.Arg(0)

	store, err := NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer store.Close()

	key, err := store.Add(username)
	if err != nil {
		log.Fatalf("cannot add key: %v", err)
	}

	fmt.Printf("Created key for %s: %s\n", username, key)
}

func cmdRevoke(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	dbPath := fs.String("db", "keys.db", "path to SQLite database")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: simple-api-proxy revoke <username> [-db path]")
		os.Exit(1)
	}
	username := fs.Arg(0)

	store, err := NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer store.Close()

	if err := store.Revoke(username); err != nil {
		log.Fatalf("cannot revoke key: %v", err)
	}

	fmt.Printf("Revoked key for %s\n", username)
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", "keys.db", "path to SQLite database")
	fs.Parse(args)

	store, err := NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer store.Close()

	keys, err := store.List()
	if err != nil {
		log.Fatalf("cannot list keys: %v", err)
	}

	if len(keys) == 0 {
		fmt.Println("No keys.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tKEY PREFIX\tCREATED")
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\t%s\n", k.Username, k.KeyPrefix, k.CreatedAt)
	}
	w.Flush()
}

func cmdMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	from := fs.String("from", "keys.json", "path to JSON keys file")
	dbPath := fs.String("db", "keys.db", "path to SQLite database")
	fs.Parse(args)

	ks, err := loadKeysJSON(*from)
	if err != nil {
		log.Fatalf("cannot read %s: %v", *from, err)
	}

	store, err := NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer store.Close()

	count := 0
	for username, entry := range ks.Users {
		if err := store.insertRaw(username, entry.Key, entry.Created); err != nil {
			log.Printf("skipping %s: %v", username, err)
			continue
		}
		count++
	}

	fmt.Printf("Migrated %d key(s) from %s to %s\n", count, *from, *dbPath)
}

// --- Admin API handlers ---

func handleAdminAddKey(store KeyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"username is required"}`)
			return
		}

		key, err := store.Add(req.Username)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"username": req.Username,
			"key":      key,
		})
	}
}

func handleAdminRevokeKey(store KeyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		if username == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"username is required"}`)
			return
		}

		if err := store.Revoke(username); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"message":"revoked key for %s"}`, username)
	}
}

func handleAdminListKeys(store KeyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := store.List()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"failed to list keys"}`)
			return
		}
		if keys == nil {
			keys = []KeyInfo{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}
}

// --- HTTP middleware ---

func adminAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != token {
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authMiddleware(store KeyStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		user, ok := store.Lookup(token)
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
	fmt.Fprintln(os.Stderr, `usage: simple-api-proxy <command> [flags]

commands:
  serve    start the proxy server
  add      add a user and generate a proxy key
  revoke   revoke a user's proxy key
  list     list all proxy keys
  migrate  migrate keys from JSON file to SQLite`)
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
	case "migrate":
		cmdMigrate(os.Args[2:])
	default:
		usage()
	}
}
