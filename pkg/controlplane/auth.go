package controlplane

import (
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
