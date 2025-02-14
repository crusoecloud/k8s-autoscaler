/*
Copyright 2024 The Kubernetes Authors.

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

package crusoecloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
)

type opStatus string

const (
	opSucceeded  opStatus = "SUCCEEDED"
	opInProgress opStatus = "IN_PROGRESS"
	opFailed     opStatus = "FAILED"

	stateRunning  = "STATE_RUNNING"
	stateDeleting = "STATE_DELETING"
)

// AuthenticatingTransport is a struct implementing http.Roundtripper
// that authenticates a request to Crusoe Cloud before sending it out.
type AuthenticatingTransport struct {
	keyID     string
	secretKey string
	http.RoundTripper
}

// NewAuthenticatingTransport creates an authenticating RoundTripper for Crusoe Cloud
func NewAuthenticatingTransport(r http.RoundTripper, keyID, secretKey string) AuthenticatingTransport {
	if r == nil {
		r = http.DefaultTransport
	}

	return AuthenticatingTransport{
		RoundTripper: r,
		keyID:        keyID,
		secretKey:    secretKey,
	}
}

// RoundTrip implements an authenticating RoundTripper for Crusoe Cloud
func (t AuthenticatingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := addSignature(r, t.keyID, t.secretKey); err != nil {
		return nil, err
	}

	//nolint:wrapcheck // error should be forwarded here.
	return t.RoundTripper.RoundTrip(r)
}

const (
	timestampHeader = "X-Crusoe-Timestamp"
	authHeader      = "Authorization"
	authVersion     = "1.0"
)

// Verifies if the token signature is valid for a given request.
func addSignature(req *http.Request, encodedKeyID, encodedKey string) error {
	req.Header.Set(timestampHeader, time.Now().UTC().Format(time.RFC3339))

	message, err := generateMessageV1_0(req)
	if err != nil {
		return err
	}
	signature, err := signMessageV1_0(message, encodedKey)
	if err != nil {
		return err
	}

	req.Header.Set(authHeader,
		"Bearer "+fmt.Sprintf("%s:%s:%s", authVersion, encodedKeyID, base64.RawURLEncoding.EncodeToString(signature)))

	return nil
}

// Generates a sha256/hmac checksum of a given message.
func signMessageV1_0(message []byte, encodedKey string) ([]byte, error) {
	// Key is b64 encoded.
	expectedKey, err := base64.RawURLEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}

	mac := hmac.New(sha256.New, expectedKey)
	mac.Write(message)

	return mac.Sum(nil), nil
}

// Per RFC, the message consists of:
// --start--
// http_path\n
// canonicalized_request_params\n
// http_verb\n
// timestamp_header_value\n
// --end--.
func generateMessageV1_0(req *http.Request) ([]byte, error) {
	messageString := strings.Builder{}

	// http_path\n
	messageString.WriteString(req.URL.Path + "\n")
	// canonicalized_request_params\n
	canonicalQuery, err := canonicalizeQuery(req.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	messageString.WriteString(canonicalQuery + "\n")
	// http_verb\n
	messageString.WriteString(req.Method + "\n")
	// timestamp_header_value\n
	messageString.WriteString(req.Header.Get(timestampHeader) + "\n")

	return []byte(messageString.String()), nil
}

var errSemicolonSeparator = errors.New("invalid semicolon separator in query")

// Canonicalizes the query into a deterministic string.
// see https://cs.opensource.google/go/go/+/refs/tags/go1.18.8:src/net/url/url.go;l=921
func canonicalizeQuery(query string) (canonicalQuery string, err error) {
	values := make(map[string][]string)
	for query != "" {
		key := query
		if i := strings.IndexAny(key, "&"); i >= 0 {
			key, query = key[:i], key[i+1:]
		} else {
			query = ""
		}
		if strings.Contains(key, ";") {
			err = errSemicolonSeparator

			continue
		}
		if key == "" {
			continue
		}

		key, value, _ := strings.Cut(key, "=")
		key, err1 := url.QueryUnescape(key)
		if err1 != nil {
			if err == nil {
				err = err1
			}

			continue
		}
		value, err1 = url.QueryUnescape(value)
		if err1 != nil {
			if err == nil {
				err = err1
			}

			continue
		}
		values[key] = append(values[key], value)
	}

	return encodeQuery(values), err
}

// encodeQuery encodes a key-value map representing the query into a deterministic string.
// see https://cs.opensource.google/go/go/+/refs/tags/go1.17.6:src/net/url/url.go;l=974
func encodeQuery(values map[string][]string) string {
	if values == nil {
		return ""
	}
	var buf strings.Builder
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vs := values[k]
		keyEscaped := url.QueryEscape(k)
		for _, v := range vs {
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(keyEscaped)
			buf.WriteByte('=')
			buf.WriteString(url.QueryEscape(v))
		}
	}

	return buf.String()
}

// NewAPIClient initializes a new Crusoe API client with the given configuration.
func NewAPIClient(host, key, secret, userAgent string) *crusoeapi.APIClient {
	cfg := crusoeapi.NewConfiguration()
	cfg.UserAgent = userAgent
	cfg.BasePath = host
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	cfg.HTTPClient.Transport = NewAuthenticatingTransport(cfg.HTTPClient.Transport, key, secret)

	return crusoeapi.NewAPIClient(cfg)
}
