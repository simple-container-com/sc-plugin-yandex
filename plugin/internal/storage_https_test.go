// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// stubStorage spins up a tiny httptest server speaking the YC Object
// Storage control-plane shape (getHttpsConfig / setHttpsConfig / operation
// poll). Routes return whatever the test wires in; the handler records
// every call so tests can assert on the sequence.
type stubStorage struct {
	server *httptest.Server
	calls  []string // "METHOD path"

	// handlers keyed by "METHOD path"
	handlers map[string]http.HandlerFunc
}

func newStubStorage(t *testing.T, handlers map[string]http.HandlerFunc) *stubStorage {
	t.Helper()
	s := &stubStorage{handlers: handlers}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		s.calls = append(s.calls, key)
		if h, ok := s.handlers[key]; ok {
			h(w, r)
			return
		}
		http.Error(w, "not routed in test: "+key, http.StatusTeapot)
	}))
	t.Cleanup(func() {
		s.server.Close()
	})
	prev := storageBaseURL
	storageBaseURL = s.server.URL
	t.Cleanup(func() { storageBaseURL = prev })
	return s
}

func respondJSON(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func TestBucketWebsiteEndpoint(t *testing.T) {
	g := NewWithT(t)
	g.Expect(BucketWebsiteEndpoint("")).To(Equal(""))
	g.Expect(BucketWebsiteEndpoint("  ")).To(Equal(""))
	g.Expect(BucketWebsiteEndpoint("simple-forge.ru")).To(Equal("simple-forge.ru.website.yandexcloud.net"))
	g.Expect(BucketWebsiteEndpoint("www.simple-forge.ru")).To(Equal("www.simple-forge.ru.website.yandexcloud.net"))
}

func TestEnsureBucketHTTPS_RequiredFields(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	_, err := ensureBucketHTTPS(ctx, "iam", "", "cert")
	g.Expect(err).To(MatchError(ContainSubstring("bucket is required")))

	_, err = ensureBucketHTTPS(ctx, "iam", "b", "")
	g.Expect(err).To(MatchError(ContainSubstring("certificate id is required")))

	_, err = ensureBucketHTTPS(ctx, "", "b", "c")
	g.Expect(err).To(MatchError(ContainSubstring("iam token is required")))
}

func TestEnsureBucketHTTPS_AlreadyBound_SkipsPOST(t *testing.T) {
	g := NewWithT(t)
	stub := newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/simple-forge.ru:getHttpsConfig": respondJSON(http.StatusOK, bucketHTTPSConfig{
			Name:          "simple-forge.ru",
			CertificateID: "fpqabc",
		}),
	})

	changed, err := ensureBucketHTTPS(context.Background(), "iam", "simple-forge.ru", "fpqabc")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeFalse(), "already-bound cert must short-circuit")
	g.Expect(stub.calls).To(HaveLen(1), "no POST when cert matches")
}

func TestEnsureBucketHTTPS_MismatchCert_PostsAndPolls(t *testing.T) {
	g := NewWithT(t)
	setCalls := 0
	stub := newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/simple-forge.ru:getHttpsConfig": respondJSON(http.StatusOK, bucketHTTPSConfig{
			Name:          "simple-forge.ru",
			CertificateID: "stale",
		}),
		"POST /storage/v1/buckets/simple-forge.ru:setHttpsConfig": func(w http.ResponseWriter, r *http.Request) {
			setCalls++
			body, _ := io.ReadAll(r.Body)
			g.Expect(string(body)).To(ContainSubstring(`"certificateId":"fpqnew"`))
			// Return already-done operation to avoid poll loop in tests.
			respondJSON(http.StatusOK, storageOperation{ID: "op1", Done: true})(w, r)
		},
	})

	changed, err := ensureBucketHTTPS(context.Background(), "iam", "simple-forge.ru", "fpqnew")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(setCalls).To(Equal(1))
	g.Expect(stub.calls).To(Equal([]string{
		"GET /storage/v1/buckets/simple-forge.ru:getHttpsConfig",
		"POST /storage/v1/buckets/simple-forge.ru:setHttpsConfig",
	}))
}

func TestEnsureBucketHTTPS_FirstBind_Handles404(t *testing.T) {
	g := NewWithT(t)
	stub := newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/simple-forge.ru:getHttpsConfig": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"code":5,"message":"HttpsConfig not found"}`, http.StatusNotFound)
		},
		"POST /storage/v1/buckets/simple-forge.ru:setHttpsConfig": respondJSON(http.StatusOK, storageOperation{ID: "op1", Done: true}),
	})

	changed, err := ensureBucketHTTPS(context.Background(), "iam", "simple-forge.ru", "fpqnew")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue(), "404 on GET is first-publish, not a hard failure")
	g.Expect(stub.calls).To(HaveLen(2))
}

func TestEnsureBucketHTTPS_OperationFailure_Surfaces(t *testing.T) {
	g := NewWithT(t)
	newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/b:getHttpsConfig": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"code":5,"message":"not found"}`, http.StatusNotFound)
		},
		"POST /storage/v1/buckets/b:setHttpsConfig": respondJSON(http.StatusOK, storageOperation{
			ID:   "op1",
			Done: true,
			Error: &storageOperationError{
				Code:    13,
				Message: "cert not issued yet",
			},
		}),
	})

	_, err := ensureBucketHTTPS(context.Background(), "iam", "b", "fpqpending")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(And(
		ContainSubstring("op1"),
		ContainSubstring("cert not issued yet"),
	))
}

func TestEnsureBucketHTTPS_PollsUntilDone(t *testing.T) {
	g := NewWithT(t)
	getCalls := 0
	newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/b:getHttpsConfig": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		},
		"POST /storage/v1/buckets/b:setHttpsConfig": respondJSON(http.StatusOK, storageOperation{ID: "op1", Done: false}),
		"GET /storage/v1/operations/op1": func(w http.ResponseWriter, r *http.Request) {
			getCalls++
			if getCalls < 2 {
				respondJSON(http.StatusOK, storageOperation{ID: "op1", Done: false})(w, r)
				return
			}
			respondJSON(http.StatusOK, storageOperation{ID: "op1", Done: true})(w, r)
		},
	})

	changed, err := ensureBucketHTTPS(context.Background(), "iam", "b", "fpq")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(getCalls).To(BeNumerically(">=", 2), "must poll until op.Done")
}

func TestIsStorageNotFound(t *testing.T) {
	g := NewWithT(t)
	g.Expect(isStorageNotFound(nil)).To(BeFalse())
	g.Expect(isStorageNotFound(errWith("storage GET /x HTTP 404: not found"))).To(BeTrue())
	g.Expect(isStorageNotFound(errWith("x: 404 Not Found: y"))).To(BeTrue())
	g.Expect(isStorageNotFound(errWith("HTTP 500: broken"))).To(BeFalse())
}

// errWith is a test helper that mints an error with a fixed message.
func errWith(s string) error {
	return &stringErr{s: s}
}

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

// TestEffectiveStorageBaseURL — the production default is used when the
// test override is empty.
func TestEffectiveStorageBaseURL(t *testing.T) {
	g := NewWithT(t)
	prev := storageBaseURL
	t.Cleanup(func() { storageBaseURL = prev })

	storageBaseURL = ""
	g.Expect(effectiveStorageBaseURL()).To(Equal(defaultStorageBaseURL))

	storageBaseURL = "https://test.example"
	g.Expect(effectiveStorageBaseURL()).To(Equal("https://test.example"))
}

// TestEnsureBucketHTTPS_Auth — the bearer header is attached.
func TestEnsureBucketHTTPS_Auth(t *testing.T) {
	g := NewWithT(t)
	var gotAuth string
	newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/b:getHttpsConfig": func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			respondJSON(http.StatusOK, bucketHTTPSConfig{Name: "b", CertificateID: "fpq"})(w, r)
		},
	})
	_, err := ensureBucketHTTPS(context.Background(), "my-iam-token", "b", "fpq")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gotAuth).To(Equal("Bearer my-iam-token"))
}

// TestEnsureBucketHTTPS_BucketNameEscaping — bucket names containing dots
// must be passed through url.PathEscape, not corrupted.
func TestEnsureBucketHTTPS_BucketNameEscaping(t *testing.T) {
	g := NewWithT(t)
	stub := newStubStorage(t, map[string]http.HandlerFunc{
		"GET /storage/v1/buckets/www.simple-forge.ru:getHttpsConfig": respondJSON(http.StatusOK, bucketHTTPSConfig{
			Name:          "www.simple-forge.ru",
			CertificateID: "fpq",
		}),
	})
	_, err := ensureBucketHTTPS(context.Background(), "iam", "www.simple-forge.ru", "fpq")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.calls[0]).To(ContainSubstring("www.simple-forge.ru"))
	g.Expect(strings.Contains(stub.calls[0], "%2E")).To(BeFalse(), "dots are legal in paths, don't over-escape")
}

// TestBuildBucketHTTPSErrorContext — the wrapper names the bucket.
func TestBuildBucketHTTPSErrorContext(t *testing.T) {
	g := NewWithT(t)
	g.Expect(buildBucketHTTPSErrorContext("b", nil)).To(BeNil())
	err := buildBucketHTTPSErrorContext("simple-forge.ru", errWith("boom"))
	g.Expect(err.Error()).To(ContainSubstring("simple-forge.ru"))
	g.Expect(err.Error()).To(ContainSubstring("boom"))
}
