package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ملاحظة أمنية: هذا المفتاح يجب أن يُنقل لاحقاً إلى ملف .env وعدم تركه مكشوفاً في الكود
var jwtSecret = []byte("future_kids_super_secret_key_2026_!@#")

// GenerateToken creates a 30-day valid JWT token for the guardian
func GenerateToken(phone string) (string, error) {
	// تحديد البيانات التي سنزرعها داخل التوكن (Claims)
	claims := jwt.MapClaims{
		"phone": phone,
		"exp":   time.Now().Add(time.Hour * 24 * 30).Unix(), // صلاحية لمدة 30 يوماً
		"iat":   time.Now().Unix(),                          // وقت الإصدار
	}

	// إنشاء التوكن باستخدام خوارزمية التشفير HS256
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// توقيع التوكن بالمفتاح السري وإرجاعه كنص
	return token.SignedString(jwtSecret)
}

// ValidateToken parses and validates the JWT token
func ValidateToken(tokenString string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// التأكد من أن خوارزمية التشفير مطابقة لما استخدمناه
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	// استخراج البيانات إذا كان التوكن صالحاً
	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token")
}
