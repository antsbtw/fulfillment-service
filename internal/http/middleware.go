package http

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTAuthMiddleware validates JWT tokens for user endpoints
// 兼容 auth-service 签发的 JWT 格式，使用 MapClaims 解析
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

		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			return []byte(secretKey), nil
		})

		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token claims"})
			c.Abort()
			return
		}

		// 提取用户信息，兼容 auth-service 的 JWT 格式
		// 优先使用 uid 字段，其次使用 sub 字段（标准 JWT claim）
		if uid, ok := claims["uid"].(string); ok {
			c.Set("userID", uid)
		} else if sub, ok := claims["sub"].(string); ok {
			c.Set("userID", sub)
		}

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
