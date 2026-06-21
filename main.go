package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time" // Added for the retry sleep timer

	_ "github.com/lib/pq"
)

type ShortenRequest struct {
	URL   string `json:"url"`
	Alias string `json:"alias,omitempty"`
}

type ShortenResponse struct {
	Alias       string `json:"alias"`
	ShortURL    string `json:"short_url"`
	OriginalURL string `json:"original_url"`
}

var db *sql.DB

const base62Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generateAlias(length int) (string, error) {
	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(base62Chars))))
		if err != nil {
			return "", err
		}
		result[i] = base62Chars[num.Int64()]
	}
	return string(result), nil
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// 1. Database Connection with a Retry Loop to wait for private network initialization
	var err error
	for i := 1; i <= 5; i++ {
		log.Printf("Connecting to database (attempt %d/5)...", i)
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
			err = db.Ping()
			if err == nil {
				log.Println("Successfully connected to the database!")
				break
			}
		}
		log.Printf("Database connection attempt failed: %v. Retrying in 3 seconds...", err)
		time.Sleep(3 * time.Second)
	}

	if err != nil {
		log.Fatalf("Failed to connect to database after 5 attempts: %v", err)
	}
	defer db.Close()

	// 2. Simple Schema Migration
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS urls (
		id SERIAL PRIMARY KEY,
		alias VARCHAR(15) UNIQUE NOT NULL,
		original_url TEXT NOT NULL
	);`
	if _, err := db.Exec(createTableQuery); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	// 3. Routing (using GET /{$} to match the root path exactly)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleHome)
	mux.HandleFunc("POST /api/shorten", handleShorten)
	mux.HandleFunc("GET /{alias}", handleRedirect)
	mux.HandleFunc("GET /swagger.yaml", handleSwaggerYAML)
	mux.HandleFunc("GET /docs", handleSwaggerUI)

	// 4. Start Server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	address := "0.0.0.0:" + port
	log.Printf("Server starting on %s", address)
	if err := http.ListenAndServe(address, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/docs", http.StatusFound)
}

func handleShorten(w http.ResponseWriter, r *http.Request) {
	var req ShortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	alias := strings.TrimSpace(req.Alias)
	if alias == "" {
		var err error
		alias, err = generateAlias(6)
		if err != nil {
			http.Error(w, "Failed to generate alias", http.StatusInternalServerError)
			return
		}
	} else {
		if len(alias) < 3 || len(alias) > 15 {
			http.Error(w, "Custom alias must be between 3 and 15 characters", http.StatusBadRequest)
			return
		}
	}

	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM urls WHERE alias=$1)", alias).Scan(&exists)
	if err != nil {
		http.Error(w, "Database verification failed", http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, "Alias already in use", http.StatusConflict)
		return
	}

	_, err = db.Exec("INSERT INTO urls (alias, original_url) VALUES ($1, $2)", alias, req.URL)
	if err != nil {
		http.Error(w, "Failed to save URL", http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	shortURL := fmt.Sprintf("%s://%s/%s", scheme, r.Host, alias)

	resp := ShortenResponse{
		Alias:       alias,
		ShortURL:    shortURL,
		OriginalURL: req.URL,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func handleRedirect(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if alias == "" {
		http.NotFound(w, r)
		return
	}

	var originalURL string
	err := db.QueryRow("SELECT original_url FROM urls WHERE alias=$1", alias).Scan(&originalURL)
	if err == sql.ErrNoRows {
		http.Error(w, "Short URL not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, originalURL, http.StatusFound)
}

func handleSwaggerYAML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/yaml")
	fmt.Fprint(w, swaggerSpec)
}

func handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, swaggerUIHTML)
}

const swaggerSpec = `openapi: 3.0.3
info:
  title: URL Shortener API
  version: 1.0.0
  description: A RESTful API to shorten URLs and redirect requests, written in Go.
paths:
  /api/shorten:
    post:
      summary: Create a shortened URL
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required:
                - url
              properties:
                url:
                  type: string
                  format: uri
                  example: https://go.dev
                alias:
                  type: string
                  example: gohome
      responses:
        '201':
          description: Short URL created successfully
          content:
            application/json:
              schema:
                type: object
                properties:
                  alias:
                    type: string
                  short_url:
                    type: string
                  original_url:
                    type: string
        '400':
          description: Invalid payload or parameters
        '409':
          description: Alias conflict
  /{alias}:
    get:
      summary: Redirect to the original URL
      parameters:
        - name: alias
          in: path
          required: true
          schema:
            type: string
      responses:
        '302':
          description: Permanent/Temporary redirect to destination URL
        '404':
          description: Alias not found
`

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: '/swagger.yaml',
        dom_id: '#swagger-ui',
      });
    };
  </script>
</body>
</html>
`