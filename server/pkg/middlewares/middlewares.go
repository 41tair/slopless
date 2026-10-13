package middlewares

import (
	"net/http"

	"github.com/google/uuid"
)

func RequestMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Request-Id", uuid.NewString())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, req)
	})
}

func RateLimit(
	next http.Handler,
	admit func(*http.Request) error,
	reject func(http.ResponseWriter, *http.Request, error),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := admit(req); err != nil {
			reject(w, req, err)
			return
		}
		next.ServeHTTP(w, req)
	})
}
