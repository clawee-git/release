package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitHubAPI is a small stand-in for the REST surface, enough to exercise
// the client's own logic: it 422s a duplicate asset name exactly as GitHub
// does, which is the behaviour the idempotent upload exists for.
type fakeGitHubAPI struct {
	mu       sync.Mutex
	assets   map[int64]string // asset id -> name
	nextID   int64
	requests []string
}

func (f *fakeGitHubAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/assets"):
			type asset struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			}
			var out []asset
			for id, name := range f.assets {
				out = append(out, asset{ID: id, Name: name})
			}
			json.NewEncoder(w).Encode(out)

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/assets"):
			name := r.URL.Query().Get("name")
			for _, existing := range f.assets {
				if existing == name {
					// This is the real API's answer, and the reason a naive
					// retry could never finish.
					http.Error(w, `{"message":"already_exists"}`, http.StatusUnprocessableEntity)
					return
				}
			}
			f.nextID++
			f.assets[f.nextID] = name
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, f.nextID)

		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/releases/assets/"):
			var id int64
			fmt.Sscanf(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:], "%d", &id)
			delete(f.assets, id)
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/releases"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":7}`)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/tags/"):
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}
}

func newGitHubFixture(t *testing.T) (*GitHubClient, *fakeGitHubAPI) {
	t.Helper()
	api := &fakeGitHubAPI{assets: map[int64]string{}}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	c := NewGitHubClient("clawee-git", "release", "token", &Guard{AllowPrivate: true})
	c.APIBase, c.UploadBase = srv.URL, srv.URL
	return c, api
}

// TestUploadAssetIsIdempotent is the retry the whole promote pipeline
// promises. A failure between "release created" and "all assets uploaded"
// leaves assets behind; a second attempt re-uploads every one of them, and
// GitHub 422s a duplicate NAME. Without replacement, promote could never
// finish that release — and the pipeline's "a retry after a transient failure
// completes" claim would be false for the one step most likely to fail
// halfway.
func TestUploadAssetIsIdempotent(t *testing.T) {
	c, api := newGitHubFixture(t)
	rel := Release{ID: 7, Tag: "clawee/v0.2.28.2026.09.04.deadbeef"}

	if err := c.UploadAsset(context.Background(), rel, "clawee.zip", "application/zip", []byte("first")); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// The retry: same name, and — because the first attempt may have been cut
	// off mid-body — possibly different bytes.
	if err := c.UploadAsset(context.Background(), rel, "clawee.zip", "application/zip", []byte("second")); err != nil {
		t.Fatalf("re-upload of the same asset name: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.assets) != 1 {
		t.Fatalf("release carries %d assets, want 1", len(api.assets))
	}
	// Replaced, not skipped: a partial first upload leaves an asset of the
	// right name and the wrong bytes, and skipping would publish it.
	deleted := false
	for _, req := range api.requests {
		if strings.HasPrefix(req, "DELETE /repos/clawee-git/release/releases/assets/") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("the re-upload did not delete the existing asset first: %v", api.requests)
	}
}

func TestUploadAssetReportsAListingFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewGitHubClient("o", "r", "t", &Guard{AllowPrivate: true})
	c.APIBase, c.UploadBase = srv.URL, srv.URL
	// Assumed-empty would hit the 422 this exists to avoid, with a worse
	// message.
	err := c.UploadAsset(context.Background(), Release{ID: 1, Tag: "t"}, "a.zip", "application/zip", nil)
	if err == nil || !strings.Contains(err.Error(), "list assets") {
		t.Fatalf("err = %v, want one naming the listing", err)
	}
}

// A zero-value client must not get weaker rules than a constructed one. The
// nil-HTTP fallback used to be a bare http.Client with a one-minute blanket
// timeout — weaker on both counts than the guarded client every constructed
// one gets, and carrying exactly the whole-exchange bound that asset uploads
// must not have.
func TestNilHTTPFallbackIsTheGuardedClient(t *testing.T) {
	fallback := (&Guard{}).Client()
	if fallback.Timeout != 0 {
		t.Fatal("the fallback client carries a blanket timeout")
	}
	// It is also a GUARD, not just an unbounded client: a zero-value
	// GitHubClient still refuses a private address.
	c := &GitHubClient{APIBase: "http://127.0.0.1:1", UploadBase: "http://127.0.0.1:1"}
	_, _, err := c.do(context.Background(), http.MethodGet, "http://127.0.0.1:1/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "download guard") {
		t.Fatalf("the fallback did not apply the guard: %v", err)
	}
}
