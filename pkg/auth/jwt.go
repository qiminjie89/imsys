// Package auth 提供认证相关功能
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
)

// Claims JWT claims
type Claims struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id,omitempty"`
	jwt.RegisteredClaims
}

// JWTValidator JWT 验证器
type JWTValidator struct {
	secretKey []byte
}

// NewJWTValidator 创建 JWT 验证器
func NewJWTValidator(secretKey string) *JWTValidator {
	return &JWTValidator{
		secretKey: []byte(secretKey),
	}
}

// Validate 验证 JWT token
func (v *JWTValidator) Validate(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// 验证签名算法
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return v.secretKey, nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// GenerateToken 生成 JWT token（用于测试）
func (v *JWTValidator) GenerateToken(userID string, expiry time.Duration) (string, error) {
	claims := &Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(v.secretKey)
}

// ValidateOrMock 验证或 Mock（开发环境）
// 如果 token 为空或以 "dev_" 开头，直接返回 mock claims
func (v *JWTValidator) ValidateOrMock(tokenString string, mockUserID string) (*Claims, error) {
	// 开发环境：允许 mock token
	if tokenString == "" || len(tokenString) >= 4 && tokenString[:4] == "dev_" {
		return &Claims{
			UserID: mockUserID,
		}, nil
	}

	return v.Validate(tokenString)
}
