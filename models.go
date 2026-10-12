package main

import "time"

type RSVP struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	Attending   bool      `json:"attending"`
	GuestsCount int       `json:"guests_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateRSVPRequest struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	Attending   *bool  `json:"attending"`
	GuestsCount int    `json:"guests_count"`
}

type RSVPStats struct {
	TotalResponses int `json:"total_responses"`
	AttendingCount int `json:"attending_count"`
	DeclinedCount  int `json:"declined_count"`
	TotalHeadcount int `json:"total_headcount"`
}

type Event struct {
	ID          int       `json:"id"`
	Title       string    `json:"title"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	EventDate   string    `json:"event_date"`
	Location    string    `json:"location"`
	RespondBy   string    `json:"respond_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type CreateEventRequest struct {
	Title       string `json:"title"`
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description,omitempty"`
	EventDate   string `json:"event_date,omitempty"`
	Location    string `json:"location,omitempty"`
	RespondBy   string `json:"respond_by,omitempty"`
}

type EventDetailResponse struct {
	Event Event     `json:"event"`
	Stats RSVPStats `json:"stats"`
}

type AdminUser struct {
	ID        int       `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type AdminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type AdminLoginResponse struct {
	Success  bool   `json:"success"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type AdminCheckResponse struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}