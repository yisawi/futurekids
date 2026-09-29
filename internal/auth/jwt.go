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

// GenerateParentToken creates a 30-day JWT embedding the parent's unique DB id.
func GenerateParentToken(parentID int, phone string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"parent_id": parentID,
		"phone":     phone,
		"role":      "parent",
		"exp":       time.Now().Add(time.Hour * 24 * 30).Unix(),
		"iat":       time.Now().Unix(),
	})
	return token.SignedString(jwtSecret)
}

// GenerateAdminToken creates a short-lived JWT for an administrator.
func GenerateAdminToken(username string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username": username,
		"role":     "admin",
		"exp":      time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat":      time.Now().Unix(),
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
