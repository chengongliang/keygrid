package oidc

// JWKS：拉取/缓存 + 强制刷新兜底（RFC 7517；kid 未命中 → 刷新一次再试）。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksSet JSON Web Key Set（只保留签名用的 RSA/ECDSA 公钥）。
type jwksSet struct {
	Keys map[string]any // kid → *rsa.PublicKey | *ecdsa.PublicKey
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (c *Client) fetchJWKS(ctx context.Context) (*jwksSet, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("jwks decode: %w", err)
	}
	set := &jwksSet{Keys: map[string]any{}}
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			pub, err := rsaJWK(k)
			if err == nil {
				set.Keys[k.Kid] = pub
			}
		case "EC":
			pub, err := ecJWK(k)
			if err == nil {
				set.Keys[k.Kid] = pub
			}
		}
	}
	if len(set.Keys) == 0 {
		return nil, fmt.Errorf("jwks: no usable signing keys")
	}
	return set, nil
}

func rsaJWK(k jwk) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	var e int
	switch len(eb) {
	case 3:
		e = int(binary.BigEndian.Uint32(append([]byte{0}, eb...)))
	case 2:
		e = int(binary.BigEndian.Uint16(eb))
	case 1:
		e = int(eb[0])
	default:
		return nil, fmt.Errorf("rsa jwk: bad exponent len %d", len(eb))
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
}

func ecJWK(k jwk) (*ecdsa.PublicKey, error) {
	var crv elliptic.Curve
	switch k.Crv {
	case "P-256":
		crv = elliptic.P256()
	case "P-384":
		crv = elliptic.P384()
	case "P-521":
		crv = elliptic.P521()
	default:
		return nil, fmt.Errorf("ec jwk: unsupported crv %q", k.Crv)
	}
	xb, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, err
	}
	yb, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, err
	}
	return &ecdsa.PublicKey{Curve: crv, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
}

// keyFor 取验签公钥。forceRefresh=true 时跳过缓存强制重拉（key rotation 兜底）。
func (c *Client) keyFor(ctx context.Context, kid string, forceRefresh bool) (any, error) {
	c.jwksMu.Lock()
	defer c.jwksMu.Unlock()
	if c.jwks == nil || forceRefresh || time.Since(c.jwksFetchedAt) > 15*time.Minute {
		set, err := c.fetchJWKS(ctx)
		if err != nil {
			return nil, err
		}
		c.jwks = set
		c.jwksFetchedAt = time.Now()
	}
	if kid == "" {
		if len(c.jwks.Keys) == 1 {
			for _, v := range c.jwks.Keys {
				return v, nil
			}
		}
		return nil, fmt.Errorf("jwks: token has no kid and key set is not singleton")
	}
	key, ok := c.jwks.Keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks: unknown kid %q", kid)
	}
	return key, nil
}

// VerifyIDToken 校验 id_token：签名（JWKS）、iss、aud、exp。返回标准 claims。
func (c *Client) VerifyIDToken(ctx context.Context, raw string) (jwt.MapClaims, error) {
	// 先解析出 kid / alg（不验签、不消费 claims）
	tok, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("id_token parse: %w", err)
	}
	kid, _ := tok.Header["kid"].(string)
	alg, _ := tok.Header["alg"].(string)
	switch alg {
	case "RS256", "RS384", "RS512", "ES256", "ES384", "ES512":
	default:
		return nil, fmt.Errorf("id_token: unsupported alg %q", alg)
	}

	key, err := c.keyFor(ctx, kid, false)
	if err != nil {
		// kid 未命中/缓存空 → 强制刷新 JWKS 再试一次（key rotation 兜底）
		key, err = c.keyFor(ctx, kid, true)
		if err != nil {
			return nil, err
		}
	}

	claims := jwt.MapClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{alg}),
		jwt.WithIssuer(c.Issuer),
		jwt.WithAudience(c.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if _, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		return key, nil
	}); err != nil {
		return nil, fmt.Errorf("id_token verify: %w", err)
	}
	return claims, nil
}
