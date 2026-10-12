// dev-api provides ONLY loopback development diagnostics. It is NOT the
// production authenticated OpenSite API or a substitute for PostgreSQL.
// It depends on pagegraph/local; configured media roots must resolve away from root.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"example.com/opensite-commerce/internal/pagegraph"
	"example.com/opensite-commerce/internal/storage/local"
)

type reply struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Key    string `json:"key,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

func jsonReply(w http.ResponseWriter, code int, v reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func newHandler(store *local.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, reply{Status: "ok"}) })
	mux.HandleFunc("POST /internal/preflight/page", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			jsonReply(w, 413, reply{Status: "invalid", Detail: "body too large"})
			return
		}
		if err = pagegraph.ValidateJSON(b); err != nil {
			jsonReply(w, 422, reply{Status: "invalid", Detail: err.Error()})
			return
		}
		jsonReply(w, 200, reply{Status: "valid"})
	})
	mux.HandleFunc("POST /internal/preflight/blob", func(w http.ResponseWriter, r *http.Request) {
		if t := r.Header.Get("Content-Type"); t != "application/octet-stream" {
			jsonReply(w, 415, reply{Status: "invalid", Detail: "Content-Type must be application/octet-stream"})
			return
		}
		blob, err := store.Put(r.Context(), r.Body)
		if err != nil {
			code := 400
			if errors.Is(err, os.ErrPermission) {
				code = 500
			}
			jsonReply(w, code, reply{Status: "invalid", Detail: err.Error()})
			return
		}
		jsonReply(w, 201, reply{Status: "stored-staging", Key: blob.Key, SHA256: blob.SHA256, Bytes: blob.Size})
	})
	return mux
}
func devMediaRoot(root string) (string, error) {
	if root == "" {
		root = "./.dev-media"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve the existing prefix without creating the missing default directory.
	path, tail := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			abs = filepath.Join(resolved, tail)
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
			return "", err
		}
		tail = filepath.Join(filepath.Base(path), tail)
		path = filepath.Dir(path)
	}
	if filepath.Dir(abs) == abs {
		return "", errors.New("refusing to use filesystem root")
	}
	return abs, nil
}

func main() {
	root, err := devMediaRoot(os.Getenv("OPENSITE_DEV_MEDIA_ROOT"))
	if err != nil {
		log.Fatal(err)
	}
	store, err := local.New(root, 10<<20)
	if err != nil {
		log.Fatal(err)
	}
	addr := "127.0.0.1:8081"
	srv := &http.Server{Addr: addr, Handler: newHandler(store), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20}
	fmt.Printf("Development diagnostics listening on http://%s (loopback only)\n", addr)
	log.Fatal(srv.ListenAndServe())
}
