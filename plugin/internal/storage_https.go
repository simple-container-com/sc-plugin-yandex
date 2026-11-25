// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// Why this file exists: the default `yc-static-website` path runs with no
// `yandex_cdn_resource`. The bucket terminates its own TLS via the control
// plane's setHttpsConfig endpoint, binding the shared wildcard cert from
// the parent's yc-wildcard-cert resource. That removes the ~150 RUB/month
// non-refundable CDN package per stack AND — the reason the staticapp.ru
// story landed here — avoids the YC-CDN cross-tenant cache namespace that
// cannot be partitioned by host (the full ResourceOptions list has no
// vary/host-cache-key, and `disableCache: true` is accepted and stored as
// nil). See forge/docs/design/2026-11-02/yc-static-website-shared-cdn/
// bucket-tls-addendum.md for the full learning chain.
//
// A fresh cert binding takes up to 30 minutes to serve at the Object
// Storage edge. `ensureBucketHTTPS` therefore GETs the current binding
// first and only POSTs on a real change — re-POSTing on every `sc deploy`
// would restart that propagation clock and cause visible outages. This is
// the same gate forge-conductor #318 encoded for the `*.staticapp.ru`
// migration.

const defaultStorageBaseURL = "https://storage.api.cloud.yandex.net"

// storageBaseURL is overridable in tests so the bucket-TLS wiring can be
// unit-tested against an httptest server. Production paths leave it empty
// and get defaultStorageBaseURL.
var storageBaseURL = ""

func effectiveStorageBaseURL() string {
	if storageBaseURL != "" {
		return storageBaseURL
	}
	return defaultStorageBaseURL
}

// BucketWebsiteEndpoint is the hostname a landing's CNAME points at once
// the bucket terminates its own TLS. The bucket is named for the FQDN, so
// the endpoint is derived, never stored.
func BucketWebsiteEndpoint(bucket string) string {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return ""
	}
	return bucket + ".website.yandexcloud.net"
}

// bucketHTTPSConfig is the sliver of the getHttpsConfig response we read.
type bucketHTTPSConfig struct {
	Name          string `json:"name"`
	CertificateID string `json:"certificateId"`
}

// storageOperation is the YC long-running Operation envelope.
type storageOperation struct {
	ID    string                 `json:"id"`
	Done  bool                   `json:"done"`
	Error *storageOperationError `json:"error"`
}

type storageOperationError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ensureBucketHTTPS idempotently binds certID to bucket so the bucket
// serves its custom domain over HTTPS without a CDN in front.
//
// Returns (changed, error): changed=false means the bucket already carried
// this certificate and no write happened. That distinction matters —
// callers re-POSTing on every publish would restart the ~30-minute edge
// propagation clock each time.
func ensureBucketHTTPS(ctx context.Context, iamToken, bucket, certID string) (bool, error) {
	bucket = strings.TrimSpace(bucket)
	certID = strings.TrimSpace(certID)
	if bucket == "" {
		return false, errors.New("bucket is required to bind HTTPS")
	}
	if certID == "" {
		return false, errors.New("certificate id is required to bind HTTPS")
	}
	if iamToken == "" {
		return false, errors.New("iam token is required to bind HTTPS")
	}

	// Read the current binding first. A bucket that has never had HTTPS
	// configured answers 404 — the common first-publish path, not an error.
	path := "/storage/v1/buckets/" + url.PathEscape(bucket) + ":getHttpsConfig"
	var cur bucketHTTPSConfig
	if err := doStorageJSON(ctx, iamToken, http.MethodGet, path, nil, &cur); err != nil {
		if !isStorageNotFound(err) {
			return false, errors.Wrap(err, "read bucket HTTPS config")
		}
	} else if strings.EqualFold(strings.TrimSpace(cur.CertificateID), certID) {
		return false, nil // already bound to this cert
	}

	body, err := json.Marshal(map[string]any{
		"certificateManager": map[string]any{"certificateId": certID},
	})
	if err != nil {
		return false, errors.Wrap(err, "marshal setHttpsConfig")
	}
	path = "/storage/v1/buckets/" + url.PathEscape(bucket) + ":setHttpsConfig"
	var op storageOperation
	if err := doStorageJSON(ctx, iamToken, http.MethodPost, path, body, &op); err != nil {
		return false, errors.Wrap(err, "bind bucket HTTPS")
	}
	if err := pollStorageOperation(ctx, iamToken, op); err != nil {
		return false, errors.Wrap(err, "bind bucket HTTPS")
	}
	return true, nil
}

// pollStorageOperation waits for a YC storage operation to reach Done. YC
// returns a failed long-running operation as HTTP 200 + `{"done":true,
// "error":{…}}`, so without reading op.Error the transport succeeds and
// the work is silently skipped.
func pollStorageOperation(ctx context.Context, iamToken string, op storageOperation) error {
	if op.Done {
		return storageOperationFailure(op)
	}
	if op.ID == "" {
		return errors.New("storage operation returned without id")
	}
	deadline := time.Now().Add(2 * time.Minute)
	path := "/storage/v1/operations/" + url.PathEscape(op.ID)
	for {
		var next storageOperation
		if err := doStorageJSON(ctx, iamToken, http.MethodGet, path, nil, &next); err != nil {
			return err
		}
		if next.Done {
			return storageOperationFailure(next)
		}
		if time.Now().After(deadline) {
			return errors.Errorf("storage operation %s did not complete within 2m", op.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func storageOperationFailure(op storageOperation) error {
	if op.Error == nil {
		return nil
	}
	return errors.Errorf("storage operation %s failed (code %d): %s",
		op.ID, op.Error.Code, strings.TrimSpace(op.Error.Message))
}

// doStorageJSON is the plugin's thin HTTP shim for the YC Object Storage
// control plane. Mirrors yc_cm_poll.go's style — a bearer IAM token on the
// Authorization header, JSON in and out, status != 200 returned as a
// readable error.
func doStorageJSON(ctx context.Context, iamToken, method, path string, body []byte, out any) error {
	u := effectiveStorageBaseURL() + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return errors.Wrap(err, "build storage request")
	}
	req.Header.Set("Authorization", "Bearer "+iamToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "%s %s", method, path)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("storage %s %s HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.Wrapf(err, "decode storage %s response", path)
	}
	return nil
}

// isStorageNotFound reports whether err is a YC 404 from doStorageJSON. The
// error string shape is "storage GET …:getHttpsConfig HTTP 404: <body>", so
// a prefix match on "HTTP 404" is all we need — the body may vary across
// YC's error renderers but the status always comes through verbatim.
func isStorageNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 404") || strings.Contains(s, "404 Not Found")
}

// mintIAMTokenForStorage is a small helper that reuses mintIAMToken from
// yc_cm_poll.go so callers don't import the raw mint. Kept as a named
// shim for readability in the static-site driver.
func mintIAMTokenForStorage(ctx context.Context, saKey string) (string, error) {
	tok, err := mintIAMToken(ctx, saKey)
	if err != nil {
		return "", errors.Wrap(err, "mint IAM token for bucket HTTPS")
	}
	if tok == "" {
		return "", errors.New("IAM token exchange returned empty token")
	}
	return tok, nil
}

// buildBucketHTTPSErrorContext decorates an EnsureBucketHTTPS error with
// the bucket name so a driver-side ApplyT failure explains itself.
func buildBucketHTTPSErrorContext(bucket string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("bucket-TLS binding for %q: %w", bucket, err)
}
