package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	DB *pgxpool.Pool
}

func (s *Server) HandleCreateRSVP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CreateRSVPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Email == "" || req.Attending == nil {
		http.Error(w, "Required fields are missing: name, email, attending", http.StatusBadRequest)
		return
	}

	if req.GuestsCount < 0 {
		http.Error(w, "guests_count cannot be negative", http.StatusBadRequest)
		return
	}

	if !*req.Attending && req.GuestsCount > 0 {
		http.Error(w, "Cannot specify guests if attending is false", http.StatusBadRequest)
		return
	}

	if req.GuestsCount > 5 {
		http.Error(w, "Exceeded maximum allowed guests (max 5)", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
	INSERT INTO rsvps(name, email, attending, guests_count, updated_at)
	VALUES($1, $2, $3, $4, CURRENT_TIMESTAMP)
	ON CONFLICT (email) DO UPDATE
	SET attending=EXCLUDED.attending,
		guests_count=EXCLUDED.guests_count,
		name= EXCLUDED.name
	RETURNING id, created_at;
	`

	var rsvp RSVP
	rsvp.Name = req.Name
	rsvp.Email = req.Email
	rsvp.Attending = *req.Attending
	rsvp.GuestsCount = req.GuestsCount

	err := s.DB.QueryRow(ctx, query, rsvp.Name, rsvp.Email, rsvp.Attending, rsvp.GuestsCount).Scan(&rsvp.ID, &rsvp.CreatedAt)
	if err != nil {
		http.Error(w, "Failed to record RSVP: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rsvp)
}

func (s *Server) HandleListRSVPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	rows, err := s.DB.Query(ctx, `
		SELECT id, name, email, attending, guests_count, created_at 
		FROM rsvps 
		ORDER BY created_at DESC;
	`)
	if err != nil {
		http.Error(w, "Failed to query RSVPs", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var rsvps []RSVP = []RSVP{}
	for rows.Next() {
		var item RSVP
		if err := rows.Scan(&item.ID, &item.Name, &item.Email, &item.Attending, &item.GuestsCount, &item.CreatedAt); err != nil {
			http.Error(w, "Failed to parse records", http.StatusInternalServerError)
			return
		}
		rsvps = append(rsvps, item)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rsvps)
}
