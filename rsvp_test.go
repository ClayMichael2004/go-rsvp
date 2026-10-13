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

func setupTestServer(t *testing.T) (*httptest.Server, *Server, func()) {
	t.Helper()

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests")
	}

	pool := initDB()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, "TRUNCATE TABLE rsvps, events, admin_sessions RESTART IDENTITY CASCADE;")
	if err != nil {
		pool.Close()
		t.Fatalf("Failed to truncate test tables: %v", err)
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
	mux.HandleFunc("POST /events", srv.HandleCreateEvent)
	mux.HandleFunc("GET /events", srv.HandleListEvents)
	mux.HandleFunc("GET /events/detail", srv.HandleGetEventDetail)
	mux.HandleFunc("POST /admin/login", srv.HandleAdminLogin)
	mux.HandleFunc("POST /admin/logout", srv.HandleAdminLogout)
	mux.HandleFunc("GET /admin/me", srv.HandleAdminMe)

	handler := recoverMiddleware(SecurityHeadersMiddleware(TracingMiddleware(LoggingMiddleware(mux))))
	ts := httptest.NewServer(handler)

	teardown := func() {
		ts.Close()
		pool.Close()
	}

	return ts, srv, teardown
}

func boolPtr(b bool) *bool {
	return &b
}

func loginTestAdmin(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	goodLogin := `{"username":"admin","password":"admin123"}`
	resp, err := client.Post(baseURL+"/admin/login", "application/json", strings.NewReader(goodLogin))
	if err != nil {
		t.Fatalf("Failed to login test admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 for test admin login, got %d", resp.StatusCode)
	}
	var res AdminLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("Failed to decode login response: %v", err)
	}
	return res.Token
}

func TestE2E_RSVPSuite(t *testing.T) {
	ts, srv, teardown := setupTestServer(t)
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
		// Unauthenticated GET /rsvp must be rejected with 401 Unauthorized
		unauthResp, err := client.Get(ts.URL + "/rsvp")
		if err != nil {
			t.Fatalf("GET /rsvp failed: %v", err)
		}
		unauthResp.Body.Close()
		if unauthResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated GET /rsvp, got %d", unauthResp.StatusCode)
		}

		adminToken := loginTestAdmin(t, client, ts.URL)
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/rsvp", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /rsvp with auth failed: %v", err)
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
		adminToken := loginTestAdmin(t, client, ts.URL)
		req1, _ := http.NewRequest(http.MethodGet, ts.URL+"/rsvp?limit=2&offset=0", nil)
		req1.Header.Set("Authorization", "Bearer "+adminToken)

		resp, err := client.Do(req1)
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

		req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/rsvp?limit=2&offset=2", nil)
		req2.Header.Set("Authorization", "Bearer "+adminToken)

		respOffset, err := client.Do(req2)
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

		adminToken := loginTestAdmin(t, client, ts.URL)
		reqList, _ := http.NewRequest(http.MethodGet, ts.URL+"/rsvp", nil)
		reqList.Header.Set("Authorization", "Bearer "+adminToken)

		resp, err := client.Do(reqList)
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
	t.Run("Admin Authentication Flow", func(t *testing.T) {
		// 1. Unauthenticated /admin/me should return 401
		meResp, err := client.Get(ts.URL + "/admin/me")
		if err != nil {
			t.Fatalf("Failed to request /admin/me: %v", err)
		}
		meResp.Body.Close()
		if meResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated /admin/me, got %d", meResp.StatusCode)
		}

		// 2. Bad credentials should return 401
		badLogin := `{"username":"admin","password":"wrongpassword"}`
		badResp, err := client.Post(ts.URL+"/admin/login", "application/json", strings.NewReader(badLogin))
		if err != nil {
			t.Fatalf("Failed bad login: %v", err)
		}
		badResp.Body.Close()
		if badResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for invalid password, got %d", badResp.StatusCode)
		}

		// 3. Valid login returns token and cookie
		goodLogin := `{"username":"admin","password":"admin123"}`
		loginResp, err := client.Post(ts.URL+"/admin/login", "application/json", strings.NewReader(goodLogin))
		if err != nil {
			t.Fatalf("Failed login: %v", err)
		}
		defer loginResp.Body.Close()
		if loginResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 for valid login, got %d", loginResp.StatusCode)
		}
		var loginData AdminLoginResponse
		_ = json.NewDecoder(loginResp.Body).Decode(&loginData)
		if !loginData.Success || loginData.Token == "" || loginData.Username != "admin" {
			t.Fatalf("Unexpected login response: %+v", loginData)
		}

		// Check cookie in response
		var sessionCookie *http.Cookie
		for _, c := range loginResp.Cookies() {
			if c.Name == "admin_session" {
				sessionCookie = c
				break
			}
		}
		if sessionCookie == nil || sessionCookie.Value != loginData.Token {
			t.Fatalf("Expected admin_session cookie with token value")
		}

		// 4. Authenticated /admin/me with Bearer token
		meReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/me", nil)
		meReq.Header.Set("Authorization", "Bearer "+loginData.Token)
		meAuthResp, err := client.Do(meReq)
		if err != nil {
			t.Fatalf("Failed /admin/me with token: %v", err)
		}
		if meAuthResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 for authenticated /admin/me, got %d", meAuthResp.StatusCode)
		}
		var checkResp AdminCheckResponse
		_ = json.NewDecoder(meAuthResp.Body).Decode(&checkResp)
		meAuthResp.Body.Close()
		if !checkResp.Authenticated || checkResp.Username != "admin" {
			t.Fatalf("Unexpected check response: %+v", checkResp)
		}

		// 5. Logout
		logoutReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/logout", nil)
		logoutReq.Header.Set("Authorization", "Bearer "+loginData.Token)
		logoutResp, err := client.Do(logoutReq)
		if err != nil {
			t.Fatalf("Failed /admin/logout: %v", err)
		}
		logoutResp.Body.Close()
		if logoutResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 for logout, got %d", logoutResp.StatusCode)
		}

		// 6. After logout, session is invalid
		meAfterReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/me", nil)
		meAfterReq.Header.Set("Authorization", "Bearer "+loginData.Token)
		meAfterResp, err := client.Do(meAfterReq)
		if err != nil {
			t.Fatalf("Failed /admin/me after logout: %v", err)
		}
		meAfterResp.Body.Close()
		if meAfterResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for /admin/me after logout, got %d", meAfterResp.StatusCode)
		}
	})

	t.Run("Events and Extended RSVP Suite", func(t *testing.T) {
		eventReq := CreateEventRequest{
			Title:       "Annual Gala 2026",
			Slug:        "annual-gala-2026",
			Description: "Annual celebration and awards",
			EventDate:   "Nov 20, 2026 at 6:00 PM",
			Location:    "Kenyatta International Convention Centre",
			RespondBy:   "Nov 5, 2026",
		}
		b, _ := json.Marshal(eventReq)

		// Unauthenticated attempt should fail
		unauthResp, err := client.Post(ts.URL+"/events", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("Failed unauthenticated event post: %v", err)
		}
		unauthResp.Body.Close()
		if unauthResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for unauthenticated event creation, got %d", unauthResp.StatusCode)
		}

		// Login to get token
		loginPayload := `{"username":"admin","password":"admin123"}`
		lResp, err := client.Post(ts.URL+"/admin/login", "application/json", strings.NewReader(loginPayload))
		if err != nil {
			t.Fatalf("Failed admin login: %v", err)
		}
		var loginData AdminLoginResponse
		_ = json.NewDecoder(lResp.Body).Decode(&loginData)
		lResp.Body.Close()

		// 1. Create an Event with Bearer token
		createReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/events", bytes.NewReader(b))
		createReq.Header.Set("Content-Type", "application/json")
		createReq.Header.Set("Authorization", "Bearer "+loginData.Token)

		evResp, err := client.Do(createReq)
		if err != nil {
			t.Fatalf("Failed to create event: %v", err)
		}
		if evResp.StatusCode != http.StatusCreated {
			t.Fatalf("Expected 201 Created for event, got %d", evResp.StatusCode)
		}
		var createdEv Event
		_ = json.NewDecoder(evResp.Body).Decode(&createdEv)
		evResp.Body.Close()

		if createdEv.ID == 0 || createdEv.Slug != "annual-gala-2026" {
			t.Fatalf("Unexpected created event: %+v", createdEv)
		}

		// Duplicate slug creation must return 409 Conflict (QA fix)
		dupReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/events", bytes.NewReader(b))
		dupReq.Header.Set("Content-Type", "application/json")
		dupReq.Header.Set("Authorization", "Bearer "+loginData.Token)
		dupResp, err := client.Do(dupReq)
		if err != nil {
			t.Fatalf("Failed duplicate event creation: %v", err)
		}
		dupResp.Body.Close()
		if dupResp.StatusCode != http.StatusConflict {
			t.Fatalf("Expected 409 Conflict for duplicate event slug, got %d", dupResp.StatusCode)
		}

		// Empty title creation must return 400 Bad Request
		emptyTitleEv := CreateEventRequest{Title: "   "}
		bEmpty, _ := json.Marshal(emptyTitleEv)
		emptyReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/events", bytes.NewReader(bEmpty))
		emptyReq.Header.Set("Content-Type", "application/json")
		emptyReq.Header.Set("Authorization", "Bearer "+loginData.Token)
		emptyResp, err := client.Do(emptyReq)
		if err != nil {
			t.Fatalf("Failed empty title event creation: %v", err)
		}
		emptyResp.Body.Close()
		if emptyResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request for empty title, got %d", emptyResp.StatusCode)
		}

		// Invalid event id in detail request must return 400 Bad Request
		badDetailResp, err := client.Get(ts.URL + "/events/detail?id=-99")
		if err != nil {
			t.Fatalf("Failed to query invalid event id: %v", err)
		}
		badDetailResp.Body.Close()
		if badDetailResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request for negative event id, got %d", badDetailResp.StatusCode)
		}

		// 2. Fetch Event Detail
		detailResp, err := client.Get(ts.URL + "/events/detail?slug=annual-gala-2026")
		if err != nil {
			t.Fatalf("Failed to get event detail: %v", err)
		}
		if detailResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 OK for event detail, got %d", detailResp.StatusCode)
		}
		var detail EventDetailResponse
		_ = json.NewDecoder(detailResp.Body).Decode(&detail)
		detailResp.Body.Close()

		if detail.Event.Title != "Annual Gala 2026" {
			t.Fatalf("Expected event title 'Annual Gala 2026', got '%s'", detail.Event.Title)
		}

		// 3. RSVP with Additional Guests details
		rsvpWithGuests := CreateRSVPRequest{
			EventID:      &createdEv.ID,
			Name:         "Sophia Adams",
			Email:        "sophia@example.com",
			Attending:    boolPtr(true),
			GuestsCount:  2,
			GuestDetails: []string{"Liam Adams - Vegetarian", "Noah Adams - Child"},
		}
		bGuests, _ := json.Marshal(rsvpWithGuests)
		rResp, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewReader(bGuests))
		if err != nil {
			t.Fatalf("Failed to create RSVP with guests: %v", err)
		}
		if rResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 OK, got %d", rResp.StatusCode)
		}
		var savedRSVP RSVP
		_ = json.NewDecoder(rResp.Body).Decode(&savedRSVP)
		rResp.Body.Close()

		if savedRSVP.GuestsCount != 2 {
			t.Fatalf("Expected 2 guests, got %d", savedRSVP.GuestsCount)
		}
		if len(savedRSVP.GuestDetails) != 2 || savedRSVP.GuestDetails[0] != "Liam Adams - Vegetarian" {
			t.Fatalf("Expected guest details to be preserved, got %+v", savedRSVP.GuestDetails)
		}

		// 4. RSVP with Decline Reason
		rsvpDeclined := CreateRSVPRequest{
			EventID:       &createdEv.ID,
			Name:          "Daniel Brown",
			Email:         "daniel@example.com",
			Attending:     boolPtr(false),
			GuestsCount:   0,
			DeclineReason: "Traveling out of town that weekend",
		}
		bDeclined, _ := json.Marshal(rsvpDeclined)
		dResp, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewReader(bDeclined))
		if err != nil {
			t.Fatalf("Failed to create declined RSVP: %v", err)
		}
		if dResp.StatusCode != http.StatusOK {
			t.Fatalf("Expected 200 OK, got %d", dResp.StatusCode)
		}
		var savedDeclined RSVP
		_ = json.NewDecoder(dResp.Body).Decode(&savedDeclined)
		dResp.Body.Close()

		if savedDeclined.Attending != false || savedDeclined.DeclineReason != "Traveling out of town that weekend" {
			t.Fatalf("Expected decline reason preserved, got: %+v", savedDeclined)
		}

		// 5. Check Event Stats: 2 responses, 1 accepted, 1 declined, 3 headcount (Sophia + 2 guests)
		statsResp, err := client.Get(fmt.Sprintf("%s/rsvp/stats?event_id=%d", ts.URL, createdEv.ID))
		if err != nil {
			t.Fatalf("Failed to get event stats: %v", err)
		}
		var eventStats RSVPStats
		_ = json.NewDecoder(statsResp.Body).Decode(&eventStats)
		statsResp.Body.Close()

		if eventStats.TotalResponses != 2 || eventStats.AttendingCount != 1 || eventStats.DeclinedCount != 1 || eventStats.TotalHeadcount != 3 {
			t.Fatalf("Unexpected event stats: %+v", eventStats)
		}
	})

	t.Run("Security Headers Enforcement", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("Failed to request healthz: %v", err)
		}
		resp.Body.Close()

		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("Expected X-Content-Type-Options nosniff, got %s", resp.Header.Get("X-Content-Type-Options"))
		}
		if resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("Expected X-Frame-Options DENY, got %s", resp.Header.Get("X-Frame-Options"))
		}
		if resp.Header.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
			t.Errorf("Expected Referrer-Policy strict-origin-when-cross-origin, got %s", resp.Header.Get("Referrer-Policy"))
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("Expected Content-Security-Policy default-src 'self', got %s", resp.Header.Get("Content-Security-Policy"))
		}
	})

	t.Run("Path Traversal and File Inclusion Prevention", func(t *testing.T) {
		traversalReqs := []string{
			"/static/../main.go",
			"/static/../go.mod",
			"/static/../../etc/passwd",
			"/static/..%2fmain.go",
		}
		for _, target := range traversalReqs {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+target, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Failed request for traversal %s: %v", target, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("Expected 404 for traversal path %s, got %d", target, resp.StatusCode)
			}
		}
	})

	t.Run("Referenced Event Integrity Validation", func(t *testing.T) {
		invalidEventID := 999999
		payload := CreateRSVPRequest{
			EventID:   &invalidEventID,
			Name:      "Ghost Guest",
			Email:     "ghost@example.com",
			Attending: boolPtr(true),
		}
		b, _ := json.Marshal(payload)
		resp, err := client.Post(ts.URL+"/rsvp", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("Failed to post invalid event rsvp: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected 400 Bad Request for non-existent event_id, got %d. Body: %s", resp.StatusCode, string(body))
		}
	})

	t.Run("Rate Limiting Defense", func(t *testing.T) {
		// Use a dedicated server with a tight rate limiter of 3 requests
		tightLimiter := NewRateLimiter(3, 10*time.Second)
		testSrv := &Server{
			DB:           srv.DB,
			LoginLimiter: tightLimiter,
		}

		muxTight := http.NewServeMux()
		muxTight.HandleFunc("POST /admin/login", testSrv.HandleAdminLogin)
		tsTight := httptest.NewServer(muxTight)
		defer tsTight.Close()

		clientTight := tsTight.Client()
		loginPayload := `{"username":"admin","password":"wrongpassword"}`

		var hitRateLimit bool
		for i := 0; i < 5; i++ {
			resp, err := clientTight.Post(tsTight.URL+"/admin/login", "application/json", strings.NewReader(loginPayload))
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests {
				hitRateLimit = true
				break
			}
		}

		if !hitRateLimit {
			t.Fatal("Expected rate limiter to trigger HTTP 429 Too Many Requests")
		}
	})
}

func TestHandleIndex(t *testing.T) {
	srv := &Server{DB: nil}

	t.Run("GET / returns 200 with HTML UI", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()

		srv.HandleIndex(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", rec.Code)
		}

		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			t.Fatalf("Expected Content-Type text/html, got %s", contentType)
		}

		body := rec.Body.String()
		if !strings.Contains(body, "RSVP") {
			t.Fatal("Expected HTML body to contain 'RSVP'")
		}
		if !strings.Contains(body, "First Name") || !strings.Contains(body, "Last Name") {
			t.Fatal("Expected HTML body to contain First Name and Last Name fields")
		}
		if !strings.Contains(body, "Will you be attending?") {
			t.Fatal("Expected HTML body to contain 'Will you be attending?'")
		}
		if !strings.Contains(body, "Accepts with Pleasure!") || !strings.Contains(body, "Declines with Regret.") {
			t.Fatal("Expected HTML body to contain attendance options")
		}
		if !strings.Contains(body, "Additional Guests") {
			t.Fatal("Expected HTML body to contain Additional Guests selection")
		}
		if !strings.Contains(body, "SEND") {
			t.Fatal("Expected HTML body to contain SEND button")
		}
	})

	t.Run("GET /nonexistent returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/not-found", nil)
		rec := httptest.NewRecorder()

		srv.HandleIndex(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("Expected status 404, got %d", rec.Code)
		}
	})
}

func TestHandleStatic(t *testing.T) {
	srv := &Server{DB: nil}

	t.Run("GET /static/thankyou.jpg returns 200 with image/jpeg", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/thankyou.jpg", nil)
		rec := httptest.NewRecorder()

		srv.HandleStatic(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", rec.Code)
		}

		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "image/jpeg") {
			t.Fatalf("Expected Content-Type image/jpeg, got %s", contentType)
		}
	})

	t.Run("GET /static/unknown.jpg returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/unknown.jpg", nil)
		rec := httptest.NewRecorder()

		srv.HandleStatic(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("Expected status 404, got %d", rec.Code)
		}
	})
}
func TestHandleAdmin(t *testing.T) {
	srv := &Server{DB: nil}

	t.Run("GET /admin returns 200 with HTML", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		rec := httptest.NewRecorder()

		srv.HandleAdmin(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", rec.Code)
		}

		contentType := rec.Header().Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			t.Fatalf("Expected Content-Type text/html, got %s", contentType)
		}

		body := rec.Body.String()
		if !strings.Contains(body, "Admin Panel") {
			t.Fatal("Expected Admin page to contain 'Admin Panel'")
		}
		if !strings.Contains(body, "Share Event & Invitation Link") {
			t.Fatal("Expected Admin page to contain share link card")
		}
	})
}
