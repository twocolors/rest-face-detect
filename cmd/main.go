package main

import (
	_ "embed"

	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"rest-face-detect/internal/engine"
	"rest-face-detect/internal/store"
)

var idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type App struct {
	db       *store.Store
	ml       *engine.Engine
	model    string
	debug    bool
	maxBody  int64
	requests atomic.Uint64
	ui       bool
}

type imageJSON struct {
	Image string `json:"image"`
}

type Match struct {
	ID         string  `json:"id"`
	Similarity float32 `json:"similarity"`
}

type FaceResult struct {
	Face    int     `json:"face"`
	Matches []Match `json:"matches"`
}

//go:embed index.html
var index []byte

func main() {
	dataPath := getenv("DATA_PATH", "/data")

	modelPath := getenv("MODEL_PATH", "/models")
	model := getenv("MODEL", "buffalo_sc")
	modelFile := filepath.Join(modelPath, model+".gguf")

	libFile := getenv("FACEDETECT_LIB", "/usr/local/lib/libfacedetect.so")

	addr := getenv("ADDR", ":8000")
	debug := getenv("DEBUG", "false") == "true"
	ui := getenv("UI", "false") == "true"
	imageSizeMB := 10
	if v := os.Getenv("IMAGE_SIZE"); v != "" {
		if _, err := fmt.Sscan(v, &imageSizeMB); err != nil || imageSizeMB <= 0 {
			log.Printf("invalid IMAGE_SIZE=%q, using default=%d", v, 10)
			imageSizeMB = 10
		}
	}
	maxBody := int64(imageSizeMB) * 1024 * 1024

	if err := os.MkdirAll(dataPath, 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(modelPath, 0755); err != nil {
		log.Fatal(err)
	}

	db, err := store.Open(filepath.Join(dataPath, "db.sqlite"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ml, err := engine.Load(libFile, modelFile)
	if err != nil {
		log.Fatal(err)
	}
	defer ml.Close()

	app := &App{db: db, ml: ml, model: model, debug: debug, ui: ui, maxBody: maxBody}

	srv := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       8 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	log.Printf("listening addr=%s model=%s debug=%t image_size_mb=%d max_body_bytes=%d", addr, filepath.Base(modelFile), debug, imageSizeMB, maxBody)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.withTimeout(2*time.Second, a.index))
	mux.HandleFunc("/health", a.withTimeout(500*time.Millisecond, a.health))
	mux.HandleFunc("/register", a.withTimeout(2*time.Second, a.persons))
	mux.HandleFunc("/register/", a.withTimeout(8*time.Second, a.person))
	mux.HandleFunc("/identify", a.withTimeout(6*time.Second, a.search))
	mux.HandleFunc("/analyze", a.withTimeout(6*time.Second, a.analyze))

	return a.withMiddleware(mux)
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	people, embeddings, err := a.db.Counts()
	if err != nil {
		a.fail(r, w, http.StatusInternalServerError, "database error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":      a.model,
		"persons":    people,
		"embeddings": embeddings,
	})
}

func (a *App) persons(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	v, err := a.db.List()
	if err != nil {
		a.fail(r, w, http.StatusInternalServerError, "database error", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *App) person(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/register/")
	if !validID(id) {
		errorJSON(w, http.StatusBadRequest, "invalid person id")
		return
	}

	switch r.Method {
	case http.MethodPost:
		image, err := readImage(w, r, a.maxBody)
		if err != nil {
			a.fail(r, w, http.StatusBadRequest, "invalid image", err)
			return
		}
		items, err := a.ml.EmbeddingsWithPreview(image)
		if err != nil {
			a.fail(r, w, http.StatusUnprocessableEntity, "face processing failed", err)
			return
		}
		if len(items) != 1 {
			errorJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("expected 1 face, found %d", len(items)))
			return
		}
		if err := a.db.AddEmbedding(id, items[0].Embedding, items[0].Preview); err != nil {
			a.fail(r, w, http.StatusInternalServerError, "database error", err)
			return
		}
		p, _, err := a.db.Get(id)
		if err != nil {
			a.fail(r, w, http.StatusInternalServerError, "database error", err)
			return
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodDelete:
		ok, err := a.db.Delete(id)
		if err != nil {
			a.fail(r, w, http.StatusInternalServerError, "database error", err)
			return
		}
		if !ok {
			errorJSON(w, http.StatusNotFound, "person not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})

	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	image, err := readImage(w, r, a.maxBody)
	if err != nil {
		a.fail(r, w, http.StatusBadRequest, "invalid image", err)
		return
	}

	faces, err := a.ml.Embeddings(image)
	if err != nil {
		a.fail(r, w, http.StatusUnprocessableEntity, "face processing failed", err)
		return
	}

	known, err := a.db.Embeddings()
	if err != nil {
		a.fail(r, w, http.StatusInternalServerError, "database error", err)
		return
	}

	result := make([]FaceResult, 0, len(faces))
	for i, query := range faces {
		best := make(map[string]float32)
		for _, person := range known {
			s := engine.Similarity(query, person.Embedding)
			if old, ok := best[person.PersonID]; !ok || s > old {
				best[person.PersonID] = s
			}
		}

		matches := make([]Match, 0, len(best))
		for id, s := range best {
			matches = append(matches, Match{ID: id, Similarity: s})
		}
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].Similarity > matches[j].Similarity
		})

		result = append(result, FaceResult{
			Face:    i + 1,
			Matches: matches,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

func (a *App) analyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	image, err := readImage(w, r, a.maxBody)
	if err != nil {
		a.fail(r, w, http.StatusBadRequest, "invalid image", err)
		return
	}
	path, err := a.ml.Analyze(image)
	if err != nil {
		a.fail(r, w, http.StatusUnprocessableEntity, "analysis failed", err)
		return
	}
	writeJSON(w, http.StatusOK, path)
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || !a.ui {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

func readImage(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	ct := r.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "application/json") {
		var body imageJSON
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return nil, errors.New("image payload too large")
			}
			return nil, errors.New("invalid JSON")
		}
		if dec.More() {
			return nil, errors.New("invalid JSON")
		}
		if body.Image == "" {
			return nil, errors.New("image is required")
		}

		v := body.Image
		if strings.HasPrefix(v, "data:") {
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[i+1:]
			}
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, errors.New("invalid base64")
		}
		if len(b) == 0 {
			return nil, errors.New("empty image")
		}
		return b, nil
	}

	b, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errors.New("image payload too large")
		}
		return nil, errors.New("read image failed")
	}
	if len(b) == 0 {
		return nil, errors.New("empty image")
	}
	return b, nil
}

func validID(id string) bool {
	return idRE.MatchString(id)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *App) fail(r *http.Request, w http.ResponseWriter, status int, public string, err error) {
	method, path := "", ""
	if r != nil {
		method = r.Method
		path = r.URL.Path
	}

	if errors.Is(err, context.DeadlineExceeded) {
		log.Printf("timeout method=%s path=%s status=%d public=%q", method, path, status, public)
	} else {
		log.Printf("error method=%s path=%s status=%d public=%q detail=%v", method, path, status, public, err)
	}
	errorJSON(w, status, public)
}

func (a *App) debugf(format string, args ...any) {
	if a.debug {
		log.Printf("debug "+format, args...)
	}
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func (a *App) withTimeout(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

func (a *App) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				a.fail(r, rw, http.StatusInternalServerError, "internal error", fmt.Errorf("panic: %v", rec))
			}
			dur := time.Since(start)
			a.debugf("request method=%s path=%s status=%d bytes=%d dur_ms=%d remote=%s",
				r.Method,
				r.URL.Path,
				rw.status,
				rw.bytes,
				dur.Milliseconds(),
				r.RemoteAddr,
			)
		}()

		next.ServeHTTP(rw, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}
