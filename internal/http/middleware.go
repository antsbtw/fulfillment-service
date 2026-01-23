package http

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTClaims represents JWT token claims
type JWTClaims struct {
	jwt.RegisteredClaims
	UID      string `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	UserType string `json:"userType"`
}

// JWTAuthMiddleware validates JWT tokens for user endpoints
func JWTAuthMiddleware(secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			c.Abort()
			return
		}

		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		if tokenString == authHeader {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
			c.Abort()
			return
		}

		claims := &JWTClaims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			return []byte(secretKey), nil
		})

		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		// Set user info in context
		c.Set("userID", claims.UID)
		c.Set("username", claims.Username)
		c.Set("role", claims.Role)
		c.Set("userType", claims.UserType)

		c.Next()
	}
}

// InternalAuthMiddleware validates internal service calls
// 使用常量时间比较防止时序攻击
func InternalAuthMiddleware(internalSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret := c.GetHeader("X-Internal-Secret")
		if subtle.ConstantTimeCompare([]byte(secret), []byte(internalSecret)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized internal access"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// AdminAuthMiddleware validates admin API key
// 使用常量时间比较防止时序攻击
func AdminAuthMiddleware(adminAPIKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-Admin-API-Key")
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(adminAPIKey)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized admin access"})
			c.Abort()
			return
		}
		c.Next()
	}
}
