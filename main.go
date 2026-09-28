// kuso-demo-todo-api — minimal Go HTTP server for the kuso end-to-end
// demo. Reads DATABASE_URL from env (kuso wires this in automatically
// when a postgres addon is attached), exposes a JSON CRUD surface for
// todos, and serves CORS so the sister kuso-demo-todo-web frontend
// can call it from a different domain.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type todo struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"createdAt"`
}

var pool *pgxpool.Pool

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL not set — attach a postgres addon in kuso")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pgx connect: %v", err)
	}
	defer p.Close()
	pool = p

	// Subcommands: `api migrate` is the kuso release hook; `api count` and
	// `api fail` are cron / run targets for the e2e test.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if os.Getenv("MIGRATE_FAIL") == "1" {
				log.Fatal("migrate: MIGRATE_FAIL=1 — failing on purpose")
			}
			if err := migrate(ctx); err != nil {
				log.Fatalf("migrate: %v", err)
			}
			log.Print("migrate: ok")
			return
		case "count":
			var n int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM todo`).Scan(&n); err != nil {
				log.Fatalf("count: %v", err)
			}
			fmt.Printf("todo count: %d\n", n)
			return
		case "fail":
			log.Fatal("fail: exiting 1 on purpose")
		}
	}
	// Schema comes from the release hook; the server only verifies it.
	if os.Getenv("MIGRATE_ON_BOOT") == "1" {
		if err := migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/info", infoHandler)
	mux.HandleFunc("/api/todos", todosHandler)
	mux.HandleFunc("/api/todos/", todoByIDHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("kuso-demo-todo-api listening on :%s", port)
	if err := http.ListenAndServe(":"+port, withCORS(mux)); err != nil {
		log.Fatal(err)
	}
}

func migrate(ctx context.Context) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS todo (
			id         BIGSERIAL PRIMARY KEY,
			title      TEXT NOT NULL,
			done       BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

// withCORS — permissive CORS so the demo frontend at a separate
// kuso subdomain can call the API. Real apps should restrict the
// allowed origin; this is a public demo.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func todosHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listTodos(w, r)
	case http.MethodPost:
		createTodo(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func todoByIDHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/todos/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		updateTodo(w, r, id)
	case http.MethodDelete:
		deleteTodo(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func listTodos(w http.ResponseWriter, r *http.Request) {
	rows, err := pool.Query(r.Context(),
		`SELECT id, title, done, created_at FROM todo ORDER BY id DESC LIMIT 200`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := make([]todo, 0, 32)
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Done, &t.CreatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func createTodo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	if body.Title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	var t todo
	err := pool.QueryRow(r.Context(),
		`INSERT INTO todo (title) VALUES ($1) RETURNING id, title, done, created_at`,
		body.Title).Scan(&t.ID, &t.Title, &t.Done, &t.CreatedAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func updateTodo(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Done *bool `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Done == nil {
		http.Error(w, "done required", http.StatusBadRequest)
		return
	}
	var t todo
	err := pool.QueryRow(r.Context(),
		`UPDATE todo SET done = $1 WHERE id = $2 RETURNING id, title, done, created_at`,
		*body.Done, id).Scan(&t.ID, &t.Title, &t.Done, &t.CreatedAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func deleteTodo(w http.ResponseWriter, r *http.Request, id int64) {
	_, err := pool.Exec(r.Context(), `DELETE FROM todo WHERE id = $1`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fingerprint identifies a secret value without revealing it.
func fingerprint(v string) string {
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}

// infoHandler reports how kuso wired this pod: which env it runs in, which
// database it talks to, whether redis answers, and fingerprints of secrets.
func infoHandler(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"appEnv": os.Getenv("APP_ENV"), "version": "pr-preview",
		"greeting":    os.Getenv("GREETING"),
		"demoSecret":  fingerprint(os.Getenv("DEMO_SECRET")),
		"sharedToken": fingerprint(os.Getenv("SHARED_TOKEN")),
		"providerKey": fingerprint(os.Getenv("PROVIDER_KEY")),
		"hostname":    os.Getenv("HOSTNAME"),
	}
	var db string
	var n int64
	var schemaErr string
	if err := pool.QueryRow(r.Context(), `SELECT current_database()`).Scan(&db); err != nil {
		db = "error: " + err.Error()
	}
	if err := pool.QueryRow(r.Context(), `SELECT count(*) FROM todo`).Scan(&n); err != nil {
		schemaErr = err.Error()
	}
	info["database"] = db
	info["dbHost"] = hostOf(os.Getenv("DATABASE_URL"))
	info["todoCount"] = n
	if schemaErr != "" {
		info["schemaError"] = schemaErr
	}
	info["redis"] = redisPing(os.Getenv("REDIS_URL"))
	info["redisHost"] = hostOf(os.Getenv("REDIS_URL"))
	var keys []string
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		for _, p := range []string{"DATABASE_", "POSTGRES_", "REDIS_", "S3_", "DEMO_", "SHARED_", "PROVIDER_", "APP_", "GREETING"} {
			if strings.HasPrefix(k, p) {
				keys = append(keys, k)
				break
			}
		}
	}
	sort.Strings(keys)
	info["envKeys"] = keys
	writeJSON(w, http.StatusOK, info)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// redisPing speaks just enough RESP to AUTH + PING, so the demo needs no
// redis client dependency.
func redisPing(raw string) string {
	if raw == "" {
		return "not configured"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "bad url"
	}
	c, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	rd := bufio.NewReader(c)
	if pw, ok := u.User.Password(); ok && pw != "" {
		fmt.Fprintf(c, "*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(pw), pw)
		line, _ := rd.ReadString('\n')
		if !strings.HasPrefix(line, "+OK") {
			return "auth: " + strings.TrimSpace(line)
		}
	}
	fmt.Fprint(c, "*1\r\n$4\r\nPING\r\n")
	line, _ := rd.ReadString('\n')
	return strings.TrimSpace(line)
}
