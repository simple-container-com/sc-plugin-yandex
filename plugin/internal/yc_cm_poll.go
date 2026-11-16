// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// Why this file exists: Pulumi caches a managed CM certificate's `Status`
// field at Create time, and a `same` preview re-uses that cached value. The
// first provision almost always catches the cert mid-VALIDATING (Let's
// Encrypt + CM round-trip is typically 60–90 s, longer under DNS propagation
// delay), writes VALIDATING into state, and every subsequent provision then
// re-fails the ApplyT with "still VALIDATING" even though the real cert has
// long since flipped to ISSUED. `pulumi refresh` would fix it; SC does not
// run one. Bypassing Pulumi's cache with a live CM GetCertificate call keeps
// the plugin self-healing without changing SC core.
//
// Live-caught 2026-11-16 on astrofit: cert fpq9sefdtlltoutf9km5 ISSUED at
// 20:20, the next provision at 20:47 still saw VALIDATING in Pulumi state
// and the wait loop errored out.

// ycAuthorizedKey is the JSON shape of a YC IAM authorized key
// (`yc iam key create --service-account-id … --output …`).
type ycAuthorizedKey struct {
	ID               string `json:"id"`
	ServiceAccountID string `json:"service_account_id"`
	PrivateKey       string `json:"private_key"`
	KeyAlgorithm     string `json:"key_algorithm"`
}

// mintIAMToken exchanges a YC authorized-key JSON for a short-lived IAM
// token via the standard JWT-signed /iam/v1/tokens exchange. Returns the
// bearer token suitable for Authorization headers on *.api.cloud.yandex.net.
func mintIAMToken(ctx context.Context, keyJSON string) (string, error) {
	var k ycAuthorizedKey
	if err := json.Unmarshal([]byte(keyJSON), &k); err != nil {
		return "", errors.Wrap(err, "parse SA authorized key")
	}
	if k.ServiceAccountID == "" || k.ID == "" || k.PrivateKey == "" {
		return "", errors.New("SA authorized key missing service_account_id/id/private_key")
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return "", errors.New("SA private_key is not PEM-encoded")
	}
	var rsaKey *rsa.PrivateKey
	if p, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		rsaKey, ok = p.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("SA private_key is not RSA")
		}
	} else if p, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
		rsaKey = p
	} else {
		return "", errors.Wrap(err, "parse SA private_key")
	}

	hdr, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "PS256", "kid": k.ID})
	now := time.Now().Unix()
	body, _ := json.Marshal(map[string]any{
		"iss": k.ServiceAccountID,
		"aud": "https://iam.api.cloud.yandex.net/iam/v1/tokens",
		"iat": now,
		"exp": now + 360,
	})
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hdr) + "." + enc.EncodeToString(body)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		return "", errors.Wrap(err, "sign JWT with SA key")
	}
	jwt := signingInput + "." + enc.EncodeToString(sig)

	reqBody, _ := json.Marshal(map[string]string{"jwt": jwt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://iam.api.cloud.yandex.net/iam/v1/tokens", bytes.NewReader(reqBody))
	if err != nil {
		return "", errors.Wrap(err, "build IAM token request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "POST /iam/v1/tokens")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("IAM token exchange HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr struct {
		IAMToken string `json:"iamToken"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return "", errors.Wrap(err, "decode IAM token response")
	}
	if tr.IAMToken == "" {
		return "", errors.New("IAM token exchange returned empty iamToken")
	}
	return tr.IAMToken, nil
}

// getCertStatusLive calls CM GetCertificate with a bearer IAM token and
// returns the current `status` field. Bypasses Pulumi state entirely.
func getCertStatusLive(ctx context.Context, iamToken, certID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://certificate-manager.api.cloud.yandex.net/certificate-manager/v1/certificates/"+certID, nil)
	if err != nil {
		return "", errors.Wrap(err, "build CM GetCertificate request")
	}
	req.Header.Set("Authorization", "Bearer "+iamToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "GET CM certificate")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("CM GetCertificate HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var cr struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &cr); err != nil {
		return "", errors.Wrap(err, "decode CM GetCertificate response")
	}
	return cr.Status, nil
}

// waitCertIssuedLive polls CM GetCertificate until the certificate is ISSUED
// or RENEWING (both terminal for our purposes), or the timeout fires. Uses
// live YC API — not Pulumi state — so a `same` preview whose state was
// written while the cert was mid-VALIDATING self-heals on the next refresh.
func waitCertIssuedLive(ctx context.Context, keyJSON, certID string, timeout time.Duration) error {
	iam, err := mintIAMToken(ctx, keyJSON)
	if err != nil {
		return errors.Wrap(err, "mint IAM token for live cert poll")
	}
	deadline := time.Now().Add(timeout)
	for {
		status, err := getCertStatusLive(ctx, iam, certID)
		if err != nil {
			return err
		}
		up := strings.ToUpper(status)
		if up == "ISSUED" || up == "RENEWING" {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.Errorf("cert %s is %s (not ISSUED) after %s — check that _acme-challenge has propagated",
				certID, status, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
