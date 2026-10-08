package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct{
	DB *pgxpool.Pool
}

func (s *Server) HandleCreateRSVP(w http.ResponseWriter, r * http.Request){
	if r.Method != http.MethodPost{
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return 
	}

	var req CreateRSVPRequest
	if err:= json.NewDecoder(r.Body).Decode(&req); err!=nil{
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if req.Name=="" || req.Email=="" || req.Attending==nil{
		http.Error(w, "Required fields are missing: name, email, attending", http.StatusBadRequest)
		return
	}

	ctx, cancel:= context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query:=`
	INSERT INTO rsvps(name, email, attending, guests_count)
	VALUES($1, $2, $3, $4)
	ON CONFLICT (email) DO UPDATE
	SET attending=EXCLUDED.attending,
		guests_count=EXCLUDED.guests_count,
		name= EXCLUDED.name
	RETURNING id, created_at;
	`

	var rsvp RSVP
	rsvp.Name = req.Name
	rsvp.Email = req.Email
	rsvp.Attending = req.Attending
	rsvp.GuestsCount = req.GuestsCount

	err:= s.DB.QueryRow(ctx, query, rsvp.Name, rsvp.Email, rsvp.Attending, rsvp.GuestsCount).Scan(&rsvp.ID, &rsvp.CreatedAt)
	if err!=nil{
		http.Error(w, "Failed to record RSVP: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rsvp)
}