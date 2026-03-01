package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	fmt.Println("payflow server starting on :8080")
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
