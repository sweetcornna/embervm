package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// TokenInfo is what a bearer token grants.
type TokenInfo struct {
	Owner        string `json:"owner"`
	MaxSandboxes int    `json:"max_sandboxes"`
}

// TokenStore maps bearer tokens to their grants.
type TokenStore struct {
	tokens map[string]TokenInfo
}

// NewTokenStore builds a token store from a token→info map.
func NewTokenStore(tokens map[string]TokenInfo) *TokenStore {
	return &TokenStore{tokens: tokens}
}

// Lookup returns the info for a token, ok=false if unknown.
func (ts *TokenStore) Lookup(token string) (TokenInfo, bool) {
	info, ok := ts.tokens[token]
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
// get 401.
func (ts *TokenStore) Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
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
