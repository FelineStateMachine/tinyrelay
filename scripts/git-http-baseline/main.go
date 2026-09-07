// This benchmark-only server exposes plain native Git Smart HTTP on loopback.
// It intentionally has no Nostr authorization; never deploy it as a relay.
package main

import (
	"compress/gzip"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	root := flag.String("root", "", "directory of bare benchmark repositories")
	listen := flag.String("listen", "127.0.0.1:17449", "loopback HTTP listener")
	flag.Parse()
	if *root == "" {
		log.Fatal("--root is required")
	}
	path, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		log.Fatal(err)
	}
	handler := &cgi.Handler{
		Path: git, Args: []string{"http-backend"}, Dir: path,
		Env: []string{"GIT_PROJECT_ROOT=" + path, "GIT_HTTP_EXPORT_ALL=1"},
	}
	log.Printf("benchmark Git HTTP listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http has already decoded HTTP chunk framing. Git can consume an
		// unknown-length request from stdin; the generic CGI guard cannot.
		r.TransferEncoding = nil
		if r.Header.Get("Content-Encoding") == "gzip" {
			reader, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer reader.Close()
			file, err := os.CreateTemp("", "git-http-request-")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer os.Remove(file.Name())
			defer file.Close()
			size, err := io.Copy(file, reader)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			r.Body = file
			r.ContentLength = size
			r.Header.Del("Content-Encoding")
		}
		handler.ServeHTTP(w, r)
	})))
}
