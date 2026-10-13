package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	DB           *pgxpool.Pool
	LoginLimiter *RateLimiter
	RSVPLimiter  *RateLimiter
}

func NewServer(db *pgxpool.Pool) *Server {
	return &Server{
		DB:           db,
		LoginLimiter: NewRateLimiter(5, 1*time.Minute),
		RSVPLimiter:  NewRateLimiter(20, 1*time.Minute),
	}
}

func slugify(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	for _, r := range title {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
				b.WriteRune('-')
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 50 {
		s = s[:50]
	}
	if s == "" {
		s = "event"
	}
	randSuffix := generateID()[:6]
	return s + "-" + randSuffix
}

func sanitizeString(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

func (s *Server) GetAuthenticatedAdmin(r *http.Request) (*AdminUser, error) {
	var token string
	if cookie, err := r.Cookie("admin_session"); err == nil && cookie.Value != "" {
		token = cookie.Value
	} else {
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	if token == "" {
		return nil, errors.New("no session token provided")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var admin AdminUser
	query := `
		SELECT u.id, u.username, u.created_at
		FROM admin_sessions s
		JOIN admin_users u ON s.admin_id = u.id
		WHERE s.token = $1 AND s.expires_at > CURRENT_TIMESTAMP;
	`
	err := s.DB.QueryRow(ctx, query, token).Scan(&admin.ID, &admin.Username, &admin.CreatedAt)
	if err != nil {
		return nil, errors.New("invalid or expired session")
	}
	return &admin, nil
}

func (s *Server) HandleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	if s.LoginLimiter != nil && !s.LoginLimiter.Allow(getClientIP(r)) {
		http.Error(w, "Too many login attempts. Please wait a minute and try again.", http.StatusTooManyRequests)
		return
	}

	var req AdminLoginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		http.Error(w, "Username and password are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var adminID int
	var passwordHash string
	query := `SELECT id, password_hash FROM admin_users WHERE username = $1;`
	err := s.DB.QueryRow(ctx, query, req.Username).Scan(&adminID, &passwordHash)
	if err != nil {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	token := generateID() + generateID()
	insertSession := `
		INSERT INTO admin_sessions (token, admin_id, expires_at, created_at)
		VALUES ($1, $2, CURRENT_TIMESTAMP + INTERVAL '24 hours', CURRENT_TIMESTAMP);
	`
	if _, err := s.DB.Exec(ctx, insertSession, token, adminID); err != nil {
		http.Error(w, "Failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * time.Hour),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AdminLoginResponse{
		Success:  true,
		Username: req.Username,
		Token:    token,
	})
}

func (s *Server) HandleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var token string
	if cookie, err := r.Cookie("admin_session"); err == nil {
		token = cookie.Value
	}
	if token == "" {
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	if token != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		_, _ = s.DB.Exec(ctx, "DELETE FROM admin_sessions WHERE token = $1;", token)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (s *Server) HandleAdminMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	admin, err := s.GetAuthenticatedAdmin(r)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(AdminCheckResponse{Authenticated: false})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AdminCheckResponse{
		Authenticated: true,
		Username:      admin.Username,
	})
}

func (s *Server) HandleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile("static/index.html")
	if err != nil {
		data, err = staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "Page not found", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) HandleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" && r.URL.Path != "/admin/" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile("static/admin.html")
	if err != nil {
		data, err = staticFS.ReadFile("static/admin.html")
		if err != nil {
			http.Error(w, "Admin page not found", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) HandleStatic(w http.ResponseWriter, r *http.Request) {
	cleanPath := path.Clean(r.URL.Path)
	diskPath := strings.TrimPrefix(cleanPath, "/")
	if data, err := os.ReadFile(diskPath); err == nil {
		http.ServeContent(w, r, path.Base(diskPath), time.Now(), bytes.NewReader(data))
		return
	}
	data, err := staticFS.ReadFile(strings.TrimPrefix(cleanPath, "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(cleanPath), time.Now(), bytes.NewReader(data))
}

func (s *Server) HandleCreateRSVP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	if s.RSVPLimiter != nil && !s.RSVPLimiter.Allow(getClientIP(r)) {
		http.Error(w, "Rate limit exceeded. Please wait a moment before submitting again.", http.StatusTooManyRequests)
		return
	}

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

	req.Dietary = strings.TrimSpace(req.Dietary)
	if len(req.Dietary) > 100 {
		req.Dietary = req.Dietary[:100]
	}

	cleanGuests := make([]string, 0)
	req.DeclineReason = strings.TrimSpace(req.DeclineReason)
	if len(req.DeclineReason) > 500 {
		req.DeclineReason = req.DeclineReason[:500]
	}

	if !*req.Attending {
		req.GuestsCount = 0
		req.GuestDetails = []string{}
	} else {
		req.DeclineReason = ""
		if req.GuestsCount > 5 {
			http.Error(w, "Maximum allowed guests per RSVP is 5", http.StatusBadRequest)
			return
		}
		for _, g := range req.GuestDetails {
			trimmed := strings.TrimSpace(g)
			if trimmed != "" {
				if len(trimmed) > 200 {
					trimmed = trimmed[:200]
				}
				cleanGuests = append(cleanGuests, trimmed)
			}
		}
	}
	req.GuestDetails = cleanGuests

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Verify referenced event exists if specified
	if req.EventID != nil {
		var exists bool
		checkErr := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE id = $1);", *req.EventID).Scan(&exists)
		if checkErr != nil || !exists {
			http.Error(w, "Referenced event does not exist", http.StatusBadRequest)
			return
		}
	}

	query := `
	INSERT INTO rsvps (event_id, name, email, dietary, attending, guests_count, guest_details, decline_reason, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT (email) DO UPDATE
	SET event_id = COALESCE(EXCLUDED.event_id, rsvps.event_id),
	    attending = EXCLUDED.attending,
	    guests_count = EXCLUDED.guests_count,
	    guest_details = EXCLUDED.guest_details,
	    dietary = EXCLUDED.dietary,
	    decline_reason = EXCLUDED.decline_reason,
	    name = EXCLUDED.name,
	    updated_at = CURRENT_TIMESTAMP
	RETURNING id, created_at, updated_at;
	`

	var rsvp RSVP
	rsvp.EventID = req.EventID
	rsvp.Name = req.Name
	rsvp.Email = req.Email
	rsvp.Dietary = req.Dietary
	rsvp.Attending = *req.Attending
	rsvp.GuestsCount = req.GuestsCount
	rsvp.GuestDetails = req.GuestDetails
	rsvp.DeclineReason = req.DeclineReason

	err = s.DB.QueryRow(ctx, query,
		rsvp.EventID,
		rsvp.Name,
		rsvp.Email,
		rsvp.Dietary,
		rsvp.Attending,
		rsvp.GuestsCount,
		rsvp.GuestDetails,
		rsvp.DeclineReason,
	).Scan(&rsvp.ID, &rsvp.CreatedAt, &rsvp.UpdatedAt)
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

	// Protect attendee PII - only authenticated administrators can access full RSVP list
	if _, err := s.GetAuthenticatedAdmin(r); err != nil {
		http.Error(w, "Unauthorized: Admin login required", http.StatusUnauthorized)
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
			if o > 1000000 {
				offset = 1000000
			} else {
				offset = o
			}
		}
	}

	var eventID *int
	if eStr := queryValues.Get("event_id"); eStr != "" {
		if e, err := strconv.Atoi(eStr); err == nil {
			eventID = &e
		}
	}

	var attending *bool
	if aStr := queryValues.Get("attending"); aStr != "" {
		if a, err := strconv.ParseBool(aStr); err == nil {
			attending = &a
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
		SELECT id, event_id, name, email, COALESCE(dietary, ''), attending, guests_count, COALESCE(guest_details, '{}'), COALESCE(decline_reason, ''), created_at, updated_at 
		FROM rsvps 
		WHERE ($1::int IS NULL OR event_id = $1)
		  AND ($2::boolean IS NULL OR attending = $2)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4;
	`

	rows, err := s.DB.Query(ctx, query, eventID, attending, limit, offset)
	if err != nil {
		http.Error(w, "Failed to query RSVPs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	rsvps := make([]RSVP, 0)
	for rows.Next() {
		var item RSVP
		if err := rows.Scan(
			&item.ID,
			&item.EventID,
			&item.Name,
			&item.Email,
			&item.Dietary,
			&item.Attending,
			&item.GuestsCount,
			&item.GuestDetails,
			&item.DeclineReason,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			http.Error(w, "Failed to parse records: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if item.GuestDetails == nil {
			item.GuestDetails = []string{}
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

	var eventID *int
	if eStr := r.URL.Query().Get("event_id"); eStr != "" {
		if e, err := strconv.Atoi(eStr); err == nil {
			eventID = &e
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
		SELECT 
			COUNT(*) AS total_responses,
			COUNT(*) FILTER (WHERE attending = true) AS attending_count,
			COUNT(*) FILTER (WHERE attending = false) AS declined_count,
			COALESCE(COUNT(*) FILTER (WHERE attending = true) + SUM(guests_count) FILTER (WHERE attending = true), 0) AS total_headcount
		FROM rsvps
		WHERE ($1::int IS NULL OR event_id = $1);
	`

	var stats RSVPStats
	err := s.DB.QueryRow(ctx, query, eventID).Scan(
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

func (s *Server) HandleCreateEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify admin authentication
	if _, err := s.GetAuthenticatedAdmin(r); err != nil {
		http.Error(w, "Unauthorized: Admin login required", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	var req CreateEventRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	req.Title = sanitizeString(strings.TrimSpace(req.Title))
	if req.Title == "" {
		http.Error(w, "Event title is required", http.StatusBadRequest)
		return
	}
	if len(req.Title) > 200 {
		http.Error(w, "Event title cannot exceed 200 characters", http.StatusBadRequest)
		return
	}

	req.Slug = sanitizeString(strings.TrimSpace(req.Slug))
	if req.Slug == "" {
		req.Slug = slugify(req.Title)
	} else {
		req.Slug = strings.ToLower(req.Slug)
		var b strings.Builder
		for _, r := range req.Slug {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			}
		}
		req.Slug = strings.Trim(b.String(), "-")
		if req.Slug == "" {
			req.Slug = slugify(req.Title)
		}
	}
	if len(req.Slug) > 100 {
		http.Error(w, "Event slug cannot exceed 100 characters", http.StatusBadRequest)
		return
	}

	req.Description = sanitizeString(strings.TrimSpace(req.Description))
	if len(req.Description) > 2000 {
		http.Error(w, "Event description cannot exceed 2000 characters", http.StatusBadRequest)
		return
	}

	req.EventDate = sanitizeString(strings.TrimSpace(req.EventDate))
	if len(req.EventDate) > 100 {
		http.Error(w, "Event date cannot exceed 100 characters", http.StatusBadRequest)
		return
	}

	req.Location = sanitizeString(strings.TrimSpace(req.Location))
	if len(req.Location) > 200 {
		http.Error(w, "Event location cannot exceed 200 characters", http.StatusBadRequest)
		return
	}

	req.RespondBy = sanitizeString(strings.TrimSpace(req.RespondBy))
	if len(req.RespondBy) > 100 {
		http.Error(w, "Respond-by deadline cannot exceed 100 characters", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Check slug uniqueness
	var slugExists bool
	slugCheckErr := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE slug = $1);", req.Slug).Scan(&slugExists)
	if slugCheckErr == nil && slugExists {
		http.Error(w, "An event with this URL slug already exists. Please choose a different slug.", http.StatusConflict)
		return
	}

	query := `
		INSERT INTO events (title, slug, description, event_date, location, respond_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP)
		RETURNING id, created_at;
	`

	var event Event
	event.Title = req.Title
	event.Slug = req.Slug
	event.Description = req.Description
	event.EventDate = req.EventDate
	event.Location = req.Location
	event.RespondBy = req.RespondBy

	err := s.DB.QueryRow(ctx, query, event.Title, event.Slug, event.Description, event.EventDate, event.Location, event.RespondBy).
		Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(event)
}

func (s *Server) HandleListEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	query := `
		SELECT id, title, slug, description, event_date, location, respond_by, created_at
		FROM events
		ORDER BY created_at DESC;
	`

	rows, err := s.DB.Query(ctx, query)
	if err != nil {
		http.Error(w, "Failed to query events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var e Event
		if err := rows.Scan(
			&e.ID,
			&e.Title,
			&e.Slug,
			&e.Description,
			&e.EventDate,
			&e.Location,
			&e.RespondBy,
			&e.CreatedAt,
		); err != nil {
			http.Error(w, "Failed to parse records: "+err.Error(), http.StatusInternalServerError)
			return
		}
		events = append(events, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func (s *Server) HandleGetEventDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	idStr := strings.TrimSpace(r.URL.Query().Get("id"))

	if slug == "" && idStr == "" {
		http.Error(w, "Either slug or id is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var event Event
	var err error
	if slug != "" {
		query := `
			SELECT id, title, slug, description, event_date, location, respond_by, created_at
			FROM events
			WHERE slug = $1;
		`
		err = s.DB.QueryRow(ctx, query, slug).Scan(
			&event.ID,
			&event.Title,
			&event.Slug,
			&event.Description,
			&event.EventDate,
			&event.Location,
			&event.RespondBy,
			&event.CreatedAt,
		)
	} else {
		id, convErr := strconv.Atoi(idStr)
		if convErr != nil {
			http.Error(w, "Invalid event id", http.StatusBadRequest)
			return
		}
		query := `
			SELECT id, title, slug, description, event_date, location, respond_by, created_at
			FROM events
			WHERE id = $1;
		`
		err = s.DB.QueryRow(ctx, query, id).Scan(
			&event.ID,
			&event.Title,
			&event.Slug,
			&event.Description,
			&event.EventDate,
			&event.Location,
			&event.RespondBy,
			&event.CreatedAt,
		)
	}

	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	statsQuery := `
		SELECT 
			COUNT(*) AS total_responses,
			COUNT(*) FILTER (WHERE attending = true) AS attending_count,
			COUNT(*) FILTER (WHERE attending = false) AS declined_count,
			COALESCE(COUNT(*) FILTER (WHERE attending = true) + SUM(guests_count) FILTER (WHERE attending = true), 0) AS total_headcount
		FROM rsvps
		WHERE event_id = $1;
	`
	var stats RSVPStats
	_ = s.DB.QueryRow(ctx, statsQuery, event.ID).Scan(
		&stats.TotalResponses,
		&stats.AttendingCount,
		&stats.DeclinedCount,
		&stats.TotalHeadcount,
	)

	resp := EventDetailResponse{
		Event: event,
		Stats: stats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}