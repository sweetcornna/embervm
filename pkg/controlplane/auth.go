package controlplane

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// TokenInfo is what a bearer token grants.
type TokenInfo struct {
	Owner        string `json:"owner"`
	MaxSandboxes int    `json:"max_sandboxes"`
}

// TokenStore maps bearer tokens to their grants. Tokens are stored and
// compared as SHA-256 digests: a plain-string map lookup would compare
// secret material byte-by-byte (a timing side channel), and hashing also
// keeps raw tokens out of process memory dumps.
type TokenStore struct {
	tokens map[[sha256.Size]byte]TokenInfo
}

// NewTokenStore builds a token store from a token→info map.
func NewTokenStore(tokens map[string]TokenInfo) *TokenStore {
	hashed := make(map[[sha256.Size]byte]TokenInfo, len(tokens))
	for token, info := range tokens {
		hashed[sha256.Sum256([]byte(token))] = info
	}
	return &TokenStore{tokens: hashed}
}

// Lookup returns the info for a token, ok=false if unknown.
func (ts *TokenStore) Lookup(token string) (TokenInfo, bool) {
	info, ok := ts.tokens[sha256.Sum256([]byte(token))]
	return info, ok
}

// DevTokenName / DevTokenInfo are the single token used when no tokens file
// is configured (local dev convenience).
const DevTokenName = "dev-token"

// DevTokenStore returns a store with only the dev token (owner "dev", a
// generous quota). Callers should log that they are using it.
func DevTokenStore() *TokenStore {
	return NewTokenStore(map[string]TokenInfo{DevTokenName: {Owner: "dev", MaxSandboxes: 100}})
}

// LoadTokensFromFile parses a JSON object mapping bearer tokens to
// {owner,max_sandboxes}.
func LoadTokensFromFile(path string) (*TokenStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]TokenInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse tokens file: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("tokens file %s defines no tokens", path)
	}
	return NewTokenStore(m), nil
}

// ResolveTokens picks the token source for a server, failing closed: a tokens
// file wins; otherwise the well-known insecure dev token is used ONLY when
// allowInsecureDevToken is explicitly set. With neither, it returns an error
// rather than silently exposing the API. usedInsecure signals the caller to
// print a warning.
func ResolveTokens(tokensFile string, allowInsecureDevToken bool) (store *TokenStore, usedInsecure bool, err error) {
	if tokensFile != "" {
		store, err = LoadTokensFromFile(tokensFile)
		return store, false, err
	}
	if allowInsecureDevToken {
		return DevTokenStore(), true, nil
	}
	return nil, false, fmt.Errorf("no authentication configured: pass --tokens-file, " +
		"or --insecure-dev-token to accept the well-known dev token (trials only)")
}

const ctxTokenKey = "embervm.token"

// Auth is Gin middleware that requires a valid `Authorization: Bearer <token>`
// and stashes the TokenInfo in the request context. Unknown or missing tokens
// get 401. WebSocket upgrades may instead smuggle the token in a
// Sec-WebSocket-Protocol entry (see wsProtocolToken) — the browser WebSocket
// API cannot set Authorization.
func (ts *TokenStore) Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			token, ok = wsProtocolToken(c.Request)
		}
		if !ok {
			c.AbortWithStatusJSON(401, gin.H{"error": "missing bearer token"})
			return
		}
		info, ok := ts.Lookup(token)
		if !ok {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid token"})
			return
		}
		c.Set(ctxTokenKey, info)
		c.Next()
	}
}

// wsTokenPrefix marks the credential entry a browser offers among its
// WebSocket subprotocols: "bearer.<base64url-nopad(token)>". base64url
// because raw tokens may contain characters illegal in the header's token
// grammar (e.g. "=").
const wsTokenPrefix = "bearer."

// wsProtocolToken extracts a bearer token from Sec-WebSocket-Protocol on a
// WebSocket upgrade. The matched entry is REMOVED from the request header
// before any handler runs: /term and the generic guest proxy both forward
// the handshake into guest-controlled code, and the platform credential must
// never travel with it. The remaining entries (e.g. the real application
// subprotocol) are preserved for the upstream negotiation.
func wsProtocolToken(r *http.Request) (string, bool) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return "", false
	}
	var token string
	found := false
	var kept []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(header, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !found && strings.HasPrefix(p, wsTokenPrefix) {
				if raw, err := base64.RawURLEncoding.DecodeString(p[len(wsTokenPrefix):]); err == nil {
					token = string(raw)
					found = true
					continue // strip the credential entry
				}
			}
			kept = append(kept, p)
		}
	}
	if !found {
		return "", false
	}
	if len(kept) > 0 {
		r.Header.Set("Sec-WebSocket-Protocol", strings.Join(kept, ", "))
	} else {
		r.Header.Del("Sec-WebSocket-Protocol")
	}
	return token, true
}

// tokenInfo extracts the authenticated TokenInfo set by Auth.
func tokenInfo(c *gin.Context) TokenInfo {
	if v, ok := c.Get(ctxTokenKey); ok {
		if info, ok := v.(TokenInfo); ok {
			return info
		}
	}
	return TokenInfo{}
}

// bearerToken parses "Bearer <token>" (case-insensitive scheme).
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
