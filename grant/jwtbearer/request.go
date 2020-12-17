package jwtbearer

import (
	"time"

	"gopkg.in/square/go-jose.v2"
)

type grantRequest struct {
	// Issuer identifies the principal that issued the JWT assertion (same as iss claim in jwt).
	Issuer string `json:"issuer"`

	// Subject identifies the principal that is the subject of the JWT.
	Subject string `json:"subject"`

	// Scope contains list of scope values (as described in Section 3.3 of OAuth 2.0 [RFC6749])
	Scope []string `json:"scope"`

	// PublicKeyJWK contains public key inå JWK format issued by Issuer, that will be used to check JWT assertion signature.
	PublicKeyJWK jose.JSONWebKey `json:"jwk"`

	// ExpiresAt indicates, when grant will expire, so we will reject assertion from Issuer targeting Subject.
	ExpiresAt time.Time `json:"expires_at"`
}
