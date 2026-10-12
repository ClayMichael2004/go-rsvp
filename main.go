package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	pool := InitDB()
	defer pool.Close()

	srv := &Server{DB: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /rsvp", srv.HandleCreateRSVP)
	mux.HandleFunc("GET /rsvp", srv.HandleListRSVPs)
	mux.HandleFunc("GET /rsvp/stats", srv.HandleRSVPStats)

	httpServer := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Println("Server running on :8080")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server listen error: %v", err)
		}
	}()

	sig := <-stopChan
	log.Printf("Received signal %s. Shutting down gracefully...\n", sig)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited cleanly.")
}