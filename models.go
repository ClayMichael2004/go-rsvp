package main

import "time"

type RSVP struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	Attending   bool      `json:"attending"`
	GuestsCount int       `json:"guests_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type CreateRSVPRequest struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	Attending   bool   `json:"attending"`
	GuestsCount int    `json:"guests_count"`
}
