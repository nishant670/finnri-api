package http

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	nethttp "net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"finnri/internal/models"
)

const googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

type googleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
	Nonce         string
}

type googleTokenClaims struct {
	Issuer        string `json:"iss"`
	Subject       string `json:"sub"`
	Audience      any    `json:"aud"`
	ExpiresAt     int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Nonce         string `json:"nonce"`
}

type googleJWKSet struct {
	Keys []googleJWK `json:"keys"`
}

type googleJWK struct {
	KeyID string `json:"kid"`
	Type  string `json:"kty"`
	Alg   string `json:"alg"`
	Use   string `json:"use"`
	N     string `json:"n"`
	E     string `json:"e"`
}

type googleJWKSCache struct {
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
}

var googleKeys googleJWKSCache

var verifyGoogleIDToken = verifyGoogleIDTokenWithJWKS

func verifyGoogleIDTokenWithJWKS(ctx context.Context, rawToken string, audiences []string) (googleIdentity, error) {
	if len(audiences) == 0 {
		return googleIdentity{}, errors.New("google_client_ids_not_configured")
	}

	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return googleIdentity{}, errors.New("invalid_google_token")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return googleIdentity{}, errors.New("invalid_google_token")
	}
	var header struct {
		Alg string `json:"alg"`
		Key string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return googleIdentity{}, errors.New("invalid_google_token")
	}
	if header.Alg != "RS256" || header.Key == "" {
		return googleIdentity{}, errors.New("unsupported_google_token")
	}

	key, err := googleKeys.publicKey(ctx, header.Key)
	if err != nil {
		return googleIdentity{}, err
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return googleIdentity{}, errors.New("invalid_google_token")
	}
	signed := []byte(parts[0] + "." + parts[1])
	digest := sha256.Sum256(signed)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return googleIdentity{}, errors.New("invalid_google_signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return googleIdentity{}, errors.New("invalid_google_token")
	}
	var claims googleTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return googleIdentity{}, errors.New("invalid_google_token")
	}
	if claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com" {
		return googleIdentity{}, errors.New("invalid_google_issuer")
	}
	if time.Now().UTC().Unix() >= claims.ExpiresAt {
		return googleIdentity{}, errors.New("expired_google_token")
	}
	if !googleAudienceAllowed(claims.Audience, audiences) {
		return googleIdentity{}, errors.New("invalid_google_audience")
	}
	if claims.Subject == "" || strings.TrimSpace(claims.Email) == "" || !googleEmailVerified(claims.EmailVerified) {
		return googleIdentity{}, errors.New("google_email_not_verified")
	}

	return googleIdentity{
		Subject:       claims.Subject,
		Email:         strings.ToLower(strings.TrimSpace(claims.Email)),
		EmailVerified: true,
		Name:          strings.TrimSpace(claims.Name),
		Picture:       strings.TrimSpace(claims.Picture),
		Nonce:         strings.TrimSpace(claims.Nonce),
	}, nil
}

func (c *googleJWKSCache) publicKey(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	if c.keys == nil || now.After(c.expiresAt) {
		if err := c.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	key := c.keys[keyID]
	if key == nil {
		if err := c.refreshLocked(ctx); err != nil {
			return nil, err
		}
		key = c.keys[keyID]
	}
	if key == nil {
		return nil, errors.New("google_key_not_found")
	}
	return key, nil
}

func (c *googleJWKSCache) refreshLocked(ctx context.Context) error {
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, googleJWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("google_keys_unavailable")
	}

	var set googleJWKSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.KeyID == "" || jwk.Type != "RSA" || jwk.N == "" || jwk.E == "" {
			continue
		}
		key, err := rsaPublicKeyFromJWK(jwk)
		if err == nil {
			keys[jwk.KeyID] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("google_keys_empty")
	}

	c.keys = keys
	c.expiresAt = time.Now().UTC().Add(1 * time.Hour)
	return nil
}

func rsaPublicKeyFromJWK(jwk googleJWK) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid_google_key_exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

func googleAudienceAllowed(audience any, allowed []string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			allowedSet[trimmed] = struct{}{}
		}
	}
	switch aud := audience.(type) {
	case string:
		_, ok := allowedSet[aud]
		return ok
	case []any:
		for _, item := range aud {
			if text, ok := item.(string); ok {
				if _, found := allowedSet[text]; found {
					return true
				}
			}
		}
	}
	return false
}

func googleEmailVerified(value any) bool {
	switch verified := value.(type) {
	case bool:
		return verified
	case string:
		return strings.EqualFold(verified, "true")
	default:
		return false
	}
}

// The username a Google account should be introduced by.
//
// Every Google sign-in used to land as `User_2c9f4a1b` — the app then printed
// that on Home and Profile, which is the database talking rather than a
// greeting. The `profile` scope the app asks for means the ID token carries a
// display name, so there is a real answer available and it was being thrown
// away.
//
// Usernames carry a unique index and are also what Edit Profile writes to, so
// this is a starting name, not a claim on one: a taken name gets a counter, and
// after enough collisions it gets a random suffix rather than looping forever.
func googleUsername(tx *gorm.DB, identity googleIdentity) string {
	base := normalizeGoogleDisplayName(identity.Name)
	if base == "" {
		// No name on the token — a Workspace account with the field withheld,
		// or a profile that never set one. The local part of the address is
		// still closer to a name than a UUID is.
		base = normalizeGoogleDisplayName(emailLocalPart(identity.Email))
	}
	if base == "" {
		return "User_" + generateUUID()[:8]
	}

	candidate := base
	for attempt := 2; attempt <= 20; attempt++ {
		if !usernameTaken(tx, candidate) {
			return candidate
		}
		candidate = fmt.Sprintf("%s %d", base, attempt)
	}
	return base + " " + generateUUID()[:4]
}

// Whether a username was generated by us rather than chosen by the person using
// it. Only these get replaced when a real name turns up; a name someone typed
// into Edit Profile is theirs, and a later sign-in must not overwrite it.
var generatedUsernamePattern = regexp.MustCompile(`^(?i)(guest|user)_[0-9a-f]{4,}$`)

func isGeneratedUsername(username string) bool {
	return generatedUsernamePattern.MatchString(strings.TrimSpace(username))
}

func usernameTaken(tx *gorm.DB, username string) bool {
	var count int64
	if err := tx.Model(&models.User{}).Where("username = ?", username).Count(&count).Error; err != nil {
		// Treat an unreadable index as "taken" so the caller tries the next
		// candidate instead of racing the unique constraint.
		return true
	}
	return count > 0
}

// Collapses the whitespace Google is relaxed about and caps the length at what
// the column and every label in the app can hold.
func normalizeGoogleDisplayName(value string) string {
	collapsed := strings.Join(strings.Fields(value), " ")
	if len(collapsed) > 60 {
		collapsed = strings.TrimSpace(collapsed[:60])
	}
	return collapsed
}

func emailLocalPart(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found {
		return ""
	}
	// `nishant.munjal` and `nishant_munjal` both read as a name once the
	// separators are spaces; the capitalisation is left alone because guessing
	// it wrong on a name is worse than leaving it lowercase.
	return strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
}
