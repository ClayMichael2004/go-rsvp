package main

import (
	"log"
	"net/http"
)

func main() {
	pool := InitDB()
	defer pool.Close()

	srv := &Server{DB: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /rsvp", srv.HandleCreateRSVP)
	mux.HandleFunc("GET /rsvp", srv.HandleListRSVPs)

	log.Println("Server running on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("Server stopped: %v", err)
	}
}