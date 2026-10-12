package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests")
	}

	pool := initDB()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, "TRUNCATE TABLE rsvps RESTART IDENTITY CASCADE;")
	if err != nil {
		pool.Close()
		t.Fatalf("Failed to truncate rsvps table: %v", err)
	}

	srv := &Server{DB: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctxPing, cancelPing := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancelPing()

		if err := srv.DB.Ping(ctxPing); err != nil {
			http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("POST /rsvp", srv.HandleCreateRSVP)
	mux.HandleFunc("GET /rsvp", srv.HandleListRSVPs)
	mux.HandleFunc("GET /rsvp/stats", srv.HandleRSVPStats)

	handler := recoverMiddleware(TracingMiddleware(LoggingMiddleware(mux)))
	ts := httptest.NewServer(handler)

	teardown := func() {
		ts.Close()
		pool.Close()
	}

	return ts, teardown
}

func boolPtr(b bool) *bool {
	return &b
}

func TestE2E_RSVPSuite(t *testing.T) {
	ts, teardown := setupTestServer(t)
	defer teardown()

	client := ts.Client()

	t.Run("Healthz Endpoint", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("Failed to get healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "OK" {
			t.Fatalf("Expected 'OK', got %s", string(body))
		}

		reqID := resp.Header.Get("X-Request-Id")
		if reqID == "" {
			t.Fatal("Expected X-Request-Id header to be present")
		}
	})

	t.Run("Empty Database Reads", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/rsvp")
		if err != nil {
			t.Fatalf("GET /rsvp failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200, got %d", resp.StatusCode)
		}

		var list []RSVP
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("Failed to decode empty list: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("Expected 0 records, got %d", len(list))
		}

		statsResp, err := client.Get(ts.URL + "/rsvp/stats")
		if err != nil {
			t.Fatalf("GET /rsvp/stats failed: %v", err)
		}
		defer statsResp.Body.Close()

		var stats RSVPStats
		if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
			t.Fatalf("Failed to decode stats: %v", err)
		}
		if stats.TotalResponses != 0 || stats.AttendingCount != 0 || stats.DeclinedCount != 0 || stats.TotalHeadcount != 0 {
			t.Fatalf("Expected all zero stats, got %+v", stats)
		}
	})

	t.Run("Validation and Negative Tests", func(t *testing.T) {
		tests := []struct {
			name       string
			method     string
			path       string
			payload    string
			wantStatus int
		}{
			{
				name:       "Disallowed Method DELETE",
				method:     http.MethodDelete,
				path:       "/rsvp",
				payload:    "",
				wantStatus: http.StatusMethodNotAllowed,
			},
			{
				name:       "Missing required email",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "attending": true}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Missing required attending",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@example.com"}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Whitespace-only name",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "   ", "email": "alice@example.com", "attending": true}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Malformed email without dot",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@nodomain", "attending": true}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Malformed email without user",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "@domain.com", "attending": true}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Negative guests_count",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@example.com", "attending": true, "guests_count": -1}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Guests count exceeds 5",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@example.com", "attending": true, "guests_count": 6}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Unknown field rejection",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@example.com", "attending": true, "role": "admin"}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Trailing JSON token",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    `{"name": "Alice", "email": "alice@example.com", "attending": true}{"extra": 1}`,
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Name length exceeds 100",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    fmt.Sprintf(`{"name": "%s", "email": "alice@example.com", "attending": true}`, strings.Repeat("a", 101)),
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Email length exceeds 150",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    fmt.Sprintf(`{"name": "Alice", "email": "%s@example.com", "attending": true}`, strings.Repeat("b", 140)),
				wantStatus: http.StatusBadRequest,
			},
			{
				name:       "Oversized request payload (> 1MB)",
				method:     http.MethodPost,
				path:       "/rsvp",
				payload:    fmt.Sprintf(`{"name": "Alice", "email": "alice@example.com", "attending": true, "padding": "%s"}`, strings.Repeat("x", 1048576+50)),
				wantStatus: http.StatusRequestEntityTooLarge,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req, err := http.NewRequest(tc.method, ts.URL+tc.path, bytes.NewBufferString(tc.payload))
				if err != nil {
					t.Fatalf("Failed to create request: %v", err)
				}
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("Failed to execute request: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != tc.wantStatus {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("Expected status %d, got %d. Body: %s", tc.wantStatus, resp.StatusCode, string(body))
				}
			})
		}
	})

	t.Run("Positive Inserts, Domain Rules, and Headcount", func(t *testing.T) {
		// 1. Insert Solo Attendee (Alex)
		alexReq := `{"name": " Alex ", "email": "Alex@Example.COM", "attending": true, "guests_count": 0}`
		resp1, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewBufferString(alexReq))
		if err != nil {
			t.Fatalf("Failed to post Alex: %v", err)
		}
		defer resp1.Body.Close()

		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 for Alex, got %d", resp1.StatusCode)
		}

		var alex RSVP
		_ = json.NewDecoder(resp1.Body).Decode(&alex)
		if alex.ID != 1 || alex.Name != "Alex" || alex.Email != "alex@example.com" {
			t.Fatalf("Unexpected values for Alex: %+v", alex)
		}

		// 2. Insert Attendee with Guests (Sarah)
		sarahReq := `{"name": "Sarah Jenkins", "email": "sarah@example.com", "attending": true, "guests_count": 2}`
		resp2, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewBufferString(sarahReq))
		if err != nil {
			t.Fatalf("Failed to post Sarah: %v", err)
		}
		defer resp2.Body.Close()

		var sarah RSVP
		_ = json.NewDecoder(resp2.Body).Decode(&sarah)
		if sarah.ID != 2 || sarah.GuestsCount != 2 {
			t.Fatalf("Unexpected values for Sarah: %+v", sarah)
		}

		// 3. Insert Declining Responder with Auto-Zero Check (Marcus)
		marcusReq := `{"name": "Marcus Vance", "email": "marcus@example.com", "attending": false, "guests_count": 4}`
		resp3, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewBufferString(marcusReq))
		if err != nil {
			t.Fatalf("Failed to post Marcus: %v", err)
		}
		defer resp3.Body.Close()

		var marcus RSVP
		_ = json.NewDecoder(resp3.Body).Decode(&marcus)
		if marcus.ID != 3 || marcus.Attending != false || marcus.GuestsCount != 0 {
			t.Fatalf("Marcus guest count was not auto-zeroed: %+v", marcus)
		}

		// 4. Update Alex to Decline (Idempotency and Timestamp Drift)
		time.Sleep(100 * time.Millisecond)
		alexUpdate := `{"name": "Alex", "email": "alex@example.com", "attending": false, "guests_count": 0}`
		resp4, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewBufferString(alexUpdate))
		if err != nil {
			t.Fatalf("Failed to update Alex: %v", err)
		}
		defer resp4.Body.Close()

		var alexUpdated RSVP
		_ = json.NewDecoder(resp4.Body).Decode(&alexUpdated)
		if alexUpdated.ID != 1 || alexUpdated.Attending != false {
			t.Fatalf("Failed idempotent update for Alex: %+v", alexUpdated)
		}
		if !alexUpdated.UpdatedAt.After(alexUpdated.CreatedAt) {
			t.Fatalf("Expected updated_at (%v) to be after created_at (%v)", alexUpdated.UpdatedAt, alexUpdated.CreatedAt)
		}

		// 5. Verify Full Aggregated Stats
		statsResp, err := client.Get(ts.URL + "/rsvp/stats")
		if err != nil {
			t.Fatalf("Failed to get stats: %v", err)
		}
		defer statsResp.Body.Close()

		var stats RSVPStats
		_ = json.NewDecoder(statsResp.Body).Decode(&stats)

		// Sarah (1 responder + 2 guests = 3 total headcount)
		// Alex (declined), Marcus (declined)
		if stats.TotalResponses != 3 {
			t.Errorf("Expected TotalResponses 3, got %d", stats.TotalResponses)
		}
		if stats.AttendingCount != 1 {
			t.Errorf("Expected AttendingCount 1, got %d", stats.AttendingCount)
		}
		if stats.DeclinedCount != 2 {
			t.Errorf("Expected DeclinedCount 2, got %d", stats.DeclinedCount)
		}
		if stats.TotalHeadcount != 3 {
			t.Errorf("Expected TotalHeadcount 3, got %d", stats.TotalHeadcount)
		}
	})

	t.Run("Pagination and Ordering", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/rsvp?limit=2&offset=0")
		if err != nil {
			t.Fatalf("Failed to get paginated RSVPs: %v", err)
		}
		defer resp.Body.Close()

		var p1 []RSVP
		_ = json.NewDecoder(resp.Body).Decode(&p1)

		if len(p1) != 2 {
			t.Fatalf("Expected 2 records on page 1, got %d", len(p1))
		}
		if p1[0].ID != 3 || p1[1].ID != 2 {
			t.Fatalf("Expected records ordered by created_at DESC (3, 2), got (%d, %d)", p1[0].ID, p1[1].ID)
		}

		respOffset, err := client.Get(ts.URL + "/rsvp?limit=2&offset=2")
		if err != nil {
			t.Fatalf("Failed to get offset RSVPs: %v", err)
		}
		defer respOffset.Body.Close()

		var p2 []RSVP
		_ = json.NewDecoder(respOffset.Body).Decode(&p2)
		if len(p2) != 1 || p2[0].ID != 1 {
			t.Fatalf("Expected remaining record ID 1 on offset page, got %+v", p2)
		}
	})

	t.Run("Concurrent Upsert Safety", func(t *testing.T) {
		const workers = 10
		var wg sync.WaitGroup
		wg.Add(workers)

		for i := 0; i < workers; i++ {
			go func(idx int) {
				defer wg.Done()
				attending := (idx % 2) == 0
				payload := fmt.Sprintf(`{"name": "Race User", "email": "race@example.com", "attending": %t, "guests_count": 1}`, attending)

				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rsvp", bytes.NewBufferString(payload))
				req.Header.Set("Content-Type", "application/json")
				resp, reqErr := client.Do(req)
				if reqErr == nil {
					_ = resp.Body.Close()
				}
			}(i)
		}

		wg.Wait()

		resp, err := client.Get(ts.URL + "/rsvp")
		if err != nil {
			t.Fatalf("Failed to list RSVPs after race: %v", err)
		}
		defer resp.Body.Close()

		var all []RSVP
		_ = json.NewDecoder(resp.Body).Decode(&all)

		raceCount := 0
		for _, r := range all {
			if r.Email == "race@example.com" {
				raceCount++
			}
		}

		if raceCount != 1 {
			t.Fatalf("Expected exactly 1 row for race@example.com due to upsert constraint, found %d", raceCount)
		}
	})
}