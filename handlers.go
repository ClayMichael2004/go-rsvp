package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
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

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	var req CreateRSVPRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "Request payload too large (max 1MB)", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "Request body must contain only a single JSON object", http.StatusBadRequest)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)

	if req.Name == "" || req.Email == "" || req.Attending == nil {
		http.Error(w, "Required fields are missing: name, email, attending", http.StatusBadRequest)
		return
	}

	if len(req.Name) > 100 {
		http.Error(w, "Field 'name' cannot exceed 100 characters", http.StatusBadRequest)
		return
	}
	if len(req.Email) > 150 {
		http.Error(w, "Field 'email' cannot exceed 150 characters", http.StatusBadRequest)
		return
	}

	parsedEmail, err := mail.ParseAddress(req.Email)
	if err != nil {
		http.Error(w, "Invalid email format", http.StatusBadRequest)
		return
	}
	parts := strings.Split(parsedEmail.Address, "@")
	if len(parts) != 2 || !strings.Contains(parts[1], ".") || len(parts[0]) == 0 {
		http.Error(w, "Invalid email domain format", http.StatusBadRequest)
		return
	}
	req.Email = strings.ToLower(parsedEmail.Address)

	if req.GuestsCount < 0 {
		http.Error(w, "Field 'guests_count' cannot be negative", http.StatusBadRequest)
		return
	}
	if !*req.Attending {
		req.GuestsCount = 0
	} else if req.GuestsCount > 5 {
		http.Error(w, "Maximum allowed guests per RSVP is 5", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
	INSERT INTO rsvps (name, email, attending, guests_count, created_at, updated_at)
	VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT (email) DO UPDATE
	SET attending = EXCLUDED.attending,
	    guests_count = EXCLUDED.guests_count,
	    name = EXCLUDED.name,
	    updated_at = CURRENT_TIMESTAMP
	RETURNING id, created_at, updated_at;
	`

	var rsvp RSVP
	rsvp.Name = req.Name
	rsvp.Email = req.Email
	rsvp.Attending = *req.Attending
	rsvp.GuestsCount = req.GuestsCount

	err = s.DB.QueryRow(ctx, query, rsvp.Name, rsvp.Email, rsvp.Attending, rsvp.GuestsCount).
		Scan(&rsvp.ID, &rsvp.CreatedAt, &rsvp.UpdatedAt)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(rsvp)
}

func (s *Server) HandleListRSVPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 50
	offset := 0

	queryValues := r.URL.Query()
	if lStr := queryValues.Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			if l > 100 {
				limit = 100
			} else {
				limit = l
			}
		}
	}

	if oStr := queryValues.Get("offset"); oStr != "" {
		if o, err := strconv.Atoi(oStr); err == nil && o >= 0 {
			offset = o
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
		SELECT id, name, email, attending, guests_count, created_at, updated_at 
		FROM rsvps 
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2;
	`

	rows, err := s.DB.Query(ctx, query, limit, offset)
	if err != nil {
		http.Error(w, "Failed to query RSVPs", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	rsvps := make([]RSVP, 0)
	for rows.Next() {
		var item RSVP
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Email,
			&item.Attending,
			&item.GuestsCount,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			http.Error(w, "Failed to parse records", http.StatusInternalServerError)
			return
		}
		rsvps = append(rsvps, item)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rsvps)
}

func (s *Server) HandleRSVPStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
		SELECT 
			COUNT(*) AS total_responses,
			COUNT(*) FILTER (WHERE attending = true) AS attending_count,
			COUNT(*) FILTER (WHERE attending = false) AS declined_count,
			COALESCE(COUNT(*) FILTER (WHERE attending = true) + SUM(guests_count) FILTER (WHERE attending = true), 0) AS total_headcount
		FROM rsvps;
	`

	var stats RSVPStats
	err := s.DB.QueryRow(ctx, query).Scan(
		&stats.TotalResponses,
		&stats.AttendingCount,
		&stats.DeclinedCount,
		&stats.TotalHeadcount,
	)
	if err != nil {
		http.Error(w, "Failed to compute stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}