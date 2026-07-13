/*
Copyright 2026 gojnimer-labs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package gateway serves the browser-facing /gw/* routes, reverse-proxying
// into the cluster via the Kubernetes API server's services/proxy
// subresource — the same mechanism kubectl proxy/the Dashboard use, so the
// operator never needs direct pod-network reachability.
//
// Authentication is a round trip followed by a cookie: Convex mints a
// one-time token and hands it to the browser; the operator exchanges it for
// access by calling back to Convex (the only party that can enforce true
// single-use, since it holds the state), then signs its own short-lived
// session cookie — scoped to this exact workload via both the cookie's Path
// and the signed payload itself — so every subsequent request (including the
// sub-resource requests a proxied app's own HTML/JS triggers, which can't
// carry the one-time token in their URL) authenticates from the cookie alone
// with no further Convex calls. Sign mints that cookie; Verify checks it.
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrMalformedToken   = errors.New("gateway: malformed token")
	ErrInvalidSignature = errors.New("gateway: invalid signature")
	ErrScopeMismatch    = errors.New("gateway: token not valid for this workload")
	ErrExpired          = errors.New("gateway: token expired")
)

// clockSkew tolerates minor clock drift between Convex (which stamps exp)
// and the operator (which checks it).
const clockSkew = 30 * time.Second

// Payload is the claim set embedded in a gateway access token.
type Payload struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UserID    string `json:"userId"`
	Exp       int64  `json:"exp"` // unix seconds
}

// Sign mints a cookie value carrying payload — called by the operator itself
// once a one-time token has been verified and consumed via Convex (see
// internal/api.Server.requireGatewayToken). Nothing outside this package
// mints this wire format anymore; Convex's one-time tokens are opaque random
// strings it tracks in its own database instead (see
// ai-cloud-v2/convex/gateway/mutations.ts), since real single-use enforcement
// needs shared state only Convex holds.
func Sign(secret []byte, payload Payload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadB64 + "." + sigB64, nil
}

// Verify checks token against secret and confirms it authorizes access to
// namespace/name specifically — a cookie signed for one workload must not be
// replayable against another's /gw/... URL even though its signature alone
// would still validate (defense in depth alongside the cookie's own Path
// scoping, which a client could in principle ignore).
//
// Wire format: base64url(JSON(Payload)) + "." + base64url(HMAC-SHA256 over
// the base64url payload string, keyed by secret) — exactly what Sign
// produces.
func Verify(secret []byte, namespace, name, token string) (*Payload, error) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return nil, ErrMalformedToken
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, ErrMalformedToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payloadB64))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrInvalidSignature
	}

	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrMalformedToken
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrMalformedToken
	}
	if p.Namespace != namespace || p.Name != name {
		return nil, ErrScopeMismatch
	}
	if time.Now().After(time.Unix(p.Exp, 0).Add(clockSkew)) {
		return nil, ErrExpired
	}
	return &p, nil
}
