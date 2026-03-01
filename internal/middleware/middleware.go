package middleware

import "net/http"

func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// placeholder: add logging here
		next.ServeHTTP(w, r)
	})
}
