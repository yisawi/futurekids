package auth

import (
	"fmt"
	"math"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

// InitAuth initializes JWT signing and validation with the configured secret.
func InitAuth(secret string) {
	if secret == "" {
		panic("CRITICAL ERROR: JWT_SECRET is missing in environment variables")
	}
	jwtSecret = []byte(secret)
}

// Token lifetimes. Parents stay signed in on their phone for a month; admin sessions last a week.
const (
	ParentTokenTTL = 30 * 24 * time.Hour
	AdminTokenTTL  = 7 * 24 * time.Hour
)

// SessionVersionClaim carries the account's session_version when the token was issued. A token
// is accepted only while it still equals the stored value, which rises when the PIN or admin
// password changes, so every token issued before the change stops working.
const SessionVersionClaim = "sv"

// GenerateParentToken creates a JWT (valid for ParentTokenTTL, 30 days) embedding the parent's unique DB id.
func GenerateParentToken(parentID int, phone string, sessionVersion int) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"parent_id":         parentID,
		"phone":             phone,
		"role":              "parent",
		SessionVersionClaim: sessionVersion,
		"exp":               time.Now().Add(ParentTokenTTL).Unix(),
		"iat":               time.Now().Unix(),
	})
	return token.SignedString(jwtSecret)
}

// GenerateAdminToken creates a JWT (valid for AdminTokenTTL, 7 days) for an administrator.
func GenerateAdminToken(username string, sessionVersion int) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username":          username,
		"role":              "admin",
		SessionVersionClaim: sessionVersion,
		"exp":               time.Now().Add(AdminTokenTTL).Unix(),
		"iat":               time.Now().Unix(),
	})
	return token.SignedString(jwtSecret)
}

// ValidateToken parses and validates the JWT token. It requires HS256, an exp claim that has
// not passed, and an iat claim that is not in the future.
func ValidateToken(tokenString string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token")
}

// ClaimError reports a claim that is missing or has the wrong type or value.
type ClaimError struct {
	Claim  string
	Reason string
}

func (e *ClaimError) Error() string { return fmt.Sprintf("claim %q %s", e.Claim, e.Reason) }

// Role returns the role claim, which must be a non-empty string.
func Role(claims jwt.MapClaims) (string, error) {
	raw, ok := claims["role"]
	if !ok {
		return "", &ClaimError{"role", "is missing"}
	}
	role, ok := raw.(string)
	if !ok {
		return "", &ClaimError{"role", fmt.Sprintf("has type %T, want string", raw)}
	}
	if role == "" {
		return "", &ClaimError{"role", "is empty"}
	}
	return role, nil
}

// ParentID returns the parent_id claim as a positive integer. JSON numbers decode as float64,
// so any other type, a fraction, zero, a negative or an out-of-range value is rejected.
func ParentID(claims jwt.MapClaims) (int, error) {
	raw, ok := claims["parent_id"]
	if !ok {
		return 0, &ClaimError{"parent_id", "is missing"}
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, &ClaimError{"parent_id", fmt.Sprintf("has type %T, want number", raw)}
	}
	if f != math.Trunc(f) || f < 1 || f > math.MaxInt32 {
		return 0, &ClaimError{"parent_id", fmt.Sprintf("is %v, want a positive integer", f)}
	}
	return int(f), nil
}

// SessionVersion returns the sv claim as a non-negative integer. Tokens issued before session
// versions existed have no sv claim and are rejected.
func SessionVersion(claims jwt.MapClaims) (int, error) {
	raw, ok := claims[SessionVersionClaim]
	if !ok {
		return 0, &ClaimError{SessionVersionClaim, "is missing"}
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, &ClaimError{SessionVersionClaim, fmt.Sprintf("has type %T, want number", raw)}
	}
	if f != math.Trunc(f) || f < 0 || f > math.MaxInt32 {
		return 0, &ClaimError{SessionVersionClaim, fmt.Sprintf("is %v, want a non-negative integer", f)}
	}
	return int(f), nil
}

// Username returns the username claim, which must be a non-empty string.
func Username(claims jwt.MapClaims) (string, error) {
	raw, ok := claims["username"]
	if !ok {
		return "", &ClaimError{"username", "is missing"}
	}
	u, ok := raw.(string)
	if !ok {
		return "", &ClaimError{"username", fmt.Sprintf("has type %T, want string", raw)}
	}
	if u == "" {
		return "", &ClaimError{"username", "is empty"}
	}
	return u, nil
}
