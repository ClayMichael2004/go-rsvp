# Cybersecurity Audit & Remediation Report
**Project:** `go-rsvp`  
**Assessment Date:** October 13, 2026  
**Auditor Role:** Senior Cybersecurity Analyst & Defensive Engineer  
**Status:** Remediated & Verified (100% Pass on Test & Race Suite)

---

## 1. Executive Summary

A comprehensive, end-to-end cybersecurity audit was performed across the `go-rsvp` application stack. The assessment evaluated the system against the **OWASP Top 10**, **OWASP API Security Top 10**, and **CWE (Common Weakness Enumeration)** standards. 

The audit identified **14 distinct security vulnerabilities** ranging from Critical to Low severity. All 14 flaws have been patched directly in the codebase and verified using automated integration testing and Go's race detector.

### Risk Distribution
- **Critical Severity:** 1 (PII Exposure via Broken Authorization)
- **High Severity:** 3 (Path Traversal, Plaintext Session Storage, Credential Brute-Force)
- **Medium Severity:** 5 (Timing Attacks, Missing Security Headers, Slowloris DoS, SQL Error Leakage, Log/Header Injection)
- **Low / Defense-in-Depth:** 5 (Cookie Security, Input Sanitization, Migration Gaps, XSS Escaping, Hardcoded Credentials)

---

## 2. Vulnerability Summary Matrix

| # | Flaw / Weakness | Severity | CWE / OWASP | Affected Component | Status |
|---|-----------------|:--------:|:-----------:|:------------------:|:------:|
| 1 | Sensitive PII Disclosure via Unauthenticated `/rsvp` | **Critical** | CWE-200 / OWASP API3 | `handlers.go` (`HandleListRSVPs`) | **Resolved** |
| 2 | Arbitrary File Read & Directory Traversal | **High** | CWE-22 / CWE-23 | `handlers.go` (`HandleStatic`) | **Resolved** |
| 3 | Plaintext Admin Session Token Storage at Rest | **High** | CWE-312 / CWE-256 | `handlers.go`, `db.go` | **Resolved** |
| 4 | Authentication Brute-Force & Bcrypt DoS | **High** | CWE-307 / CWE-400 | `middleware.go`, `handlers.go` | **Resolved** |
| 5 | Username Enumeration via Timing Analysis | **Medium** | CWE-208 | `handlers.go` (`HandleAdminLogin`) | **Resolved** |
| 6 | Missing Defensive HTTP Security Headers | **Medium** | CWE-693 / CWE-1021 | `middleware.go`, `main.go` | **Resolved** |
| 7 | Slowloris Denial of Service Vulnerability | **Medium** | CWE-400 | `main.go` (`httpServer`) | **Resolved** |
| 8 | Internal Database Schema & Error Message Disclosure | **Medium** | CWE-209 | `handlers.go` | **Resolved** |
| 9 | Unsanitized `X-Request-Id` (Log & Header Injection) | **Medium** | CWE-113 / CWE-117 | `middleware.go` | **Resolved** |
| 10 | Missing Cookie `Secure` Flag for HTTPS Channels | **Low** | CWE-614 | `handlers.go` | **Resolved** |
| 11 | Unchecked Foreign Keys & Missing Event Field Bounds | **Low** | CWE-20 | `handlers.go` | **Resolved** |
| 12 | Incomplete Database Migration & Missing Indexes | **Low** | CWE-1188 / CWE-703 | `db.go` | **Resolved** |
| 13 | Incomplete Single-Quote Escaping in Client Template | **Low** | CWE-79 | `static/index.html` | **Resolved** |
| 14 | Hardcoded Database Fallback Credentials | **Low** | CWE-798 | `db.go` | **Resolved** |

---

## 3. Detailed Findings & Remediation

### 3.1. Sensitive PII Disclosure via Unauthenticated `/rsvp`
* **Vulnerability ID:** SEC-01 (CWE-200 / OWASP API3:2023)
* **Severity:** **Critical**
* **Root Cause:** The `GET /rsvp` endpoint returned all RSVP records without verifying identity or administrative privileges.
* **Impact:** Any unauthenticated remote actor could scrape all invitee records: full names, personal email addresses, attendance confirmation, dietary constraints, additional guest names, and confidential decline reasons.
* **Remediation:** Enforced admin authentication on `HandleListRSVPs` using `s.GetAuthenticatedAdmin(r)`. Unauthenticated requests now return `401 Unauthorized`.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L401-L415)

---

### 3.2. Arbitrary File Read & Directory Traversal in Static File Handler
* **Vulnerability ID:** SEC-02 (CWE-22 / CWE-23)
* **Severity:** **High**
* **Root Cause:** `HandleStatic` cleaned path input but lacked strict verification that the resolved path remained strictly inside `/static/`. Furthermore, it attempted `os.ReadFile` from the host filesystem before consulting embedded files.
* **Impact:** Risk of reading source code, configuration files, or sensitive server files via relative path traversal (`/static/..`).
* **Remediation:** 
  1. Enforced strict prefix check: path must begin with `/static/`.
  2. Explicitly rejected paths containing directory traversal segments (`..`).
  3. Restricted file reads to the embedded filesystem `staticFS` and validated static disk path only.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L245-L270)

---

### 3.3. Plaintext Session Token Storage at Rest
* **Vulnerability ID:** SEC-03 (CWE-312 / CWE-256)
* **Severity:** **High**
* **Root Cause:** Generated 64-character session tokens were stored directly as plaintext strings in the `admin_sessions` table.
* **Impact:** In the event of a database compromise or backup leak, active session tokens could be immediately hijacked to impersonate administrators.
* **Remediation:** 
  1. Implemented `hashToken(token string) string` utilizing cryptographic **SHA-256**.
  2. Database stores only the SHA-256 digest of the session token.
  3. Authentication queries lookup sessions via `hashToken(cookieOrHeaderToken)`.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L60-L75)

---

### 3.4. Credential Brute-Force & Bcrypt Denial of Service
* **Vulnerability ID:** SEC-04 (CWE-307 / CWE-400)
* **Severity:** **High**
* **Root Cause:** No rate limiting was enforced on `/admin/login` or `/rsvp`. Bcrypt password hashing requires ~100ms of CPU time; rapid repeated requests could exhaust server CPU cores and crack passwords.
* **Impact:** Password brute-force attacks and CPU denial-of-service against the application.
* **Remediation:** Created a thread-safe, in-memory `RateLimiter` with visitor tracking and automatic background cleanup:
  * `/admin/login`: Throttled to 15 attempts/minute per IP.
  * `/rsvp`: Throttled to 60 submissions/minute per IP.
* **Code Reference:** [`middleware.go`](file:///home/michaelochieng0/go-rsvp/middleware.go#L95-L160)

---

### 3.5. Username Enumeration via Timing Analysis
* **Vulnerability ID:** SEC-05 (CWE-208)
* **Severity:** **Medium**
* **Root Cause:** When an invalid username was supplied, the server failed immediately (~1ms). When a valid username was supplied with an invalid password, `bcrypt.CompareHashAndPassword` executed (~120ms).
* **Impact:** Attackers could measure network response time to determine which administrator usernames exist.
* **Remediation:** Generated a package-level `dummyPasswordHash` during startup. When a username does not exist, a dummy bcrypt comparison is executed, ensuring uniform execution time.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L25-L35)

---

### 3.6. Missing Defensive HTTP Security Headers
* **Vulnerability ID:** SEC-06 (CWE-693 / CWE-1021)
* **Severity:** **Medium**
* **Root Cause:** Responses omitted standard browser defense headers.
* **Impact:** Increased susceptibility to clickjacking, MIME-type sniffing, cross-site script injection, and unauthorized frame embedding.
* **Remediation:** Implemented `SecurityHeadersMiddleware` adding:
  * `X-Content-Type-Options: nosniff`
  * `X-Frame-Options: DENY`
  * `Referrer-Policy: strict-origin-when-cross-origin`
  * `X-XSS-Protection: 1; mode=block`
  * `Permissions-Policy: camera=(), microphone=(), geolocation=()`
  * `Content-Security-Policy: default-src 'self'; ...`
  * `Strict-Transport-Security: max-age=31536000; includeSubDomains` (on HTTPS)
* **Code Reference:** [`middleware.go`](file:///home/michaelochieng0/go-rsvp/middleware.go#L37-L55)

---

### 3.7. Slowloris Denial of Service (Missing HTTP Server Timeouts)
* **Vulnerability ID:** SEC-07 (CWE-400)
* **Severity:** **Medium**
* **Root Cause:** The `http.Server` instance did not configure `ReadHeaderTimeout`.
* **Impact:** Slowloris attacks could tie up connection worker goroutines by sending HTTP headers extremely slowly, starving legitimate traffic.
* **Remediation:** Added `ReadHeaderTimeout: 3 * time.Second` and `MaxHeaderBytes: 1 << 20` (1MB).
* **Code Reference:** [`main.go`](file:///home/michaelochieng0/go-rsvp/main.go#L125-L135)

---

### 3.8. Internal Database Schema & Error Leaks
* **Vulnerability ID:** SEC-08 (CWE-209)
* **Severity:** **Medium**
* **Root Cause:** Handlers used `http.Error(w, "Database error: " + err.Error(), 500)`, leaking table names, constraints, and query structures to clients.
* **Impact:** Reconnaissance aid for attackers discovering database architecture.
* **Remediation:** Detailed errors are logged server-side via `slog.Error` with unique request tracing IDs; clients receive sanitized, generic error responses.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L390-L400)

---

### 3.9. Unvalidated `X-Request-Id` (Log & Header Injection)
* **Vulnerability ID:** SEC-09 (CWE-113 / CWE-117)
* **Severity:** **Medium**
* **Root Cause:** Client-supplied `X-Request-Id` headers were accepted verbatim without character set or length validation.
* **Impact:** Potential CRLF injection in HTTP headers and log forgery.
* **Remediation:** Added `isValidRequestID` validating alphanumeric characters and dashes/underscores with a maximum length of 64 characters. Non-conforming IDs are discarded and regenerated via cryptographically secure random bytes.
* **Code Reference:** [`middleware.go`](file:///home/michaelochieng0/go-rsvp/middleware.go#L24-L35)

---

### 3.10. Missing Cookie `Secure` Flag for HTTPS
* **Vulnerability ID:** SEC-10 (CWE-614)
* **Severity:** **Low**
* **Root Cause:** Session cookies lacked dynamic `Secure` configuration.
* **Impact:** Transmission of session cookies over unencrypted HTTP channels if TLS is terminated at a reverse proxy.
* **Remediation:** Dynamically detects TLS status and `X-Forwarded-Proto` header, marking cookies `Secure: true` whenever HTTPS is in use.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L150-L165)

---

### 3.11. Unchecked Foreign Keys & Missing Event Field Bounds
* **Vulnerability ID:** SEC-11 (CWE-20)
* **Severity:** **Low**
* **Root Cause:** `POST /rsvp` did not pre-validate whether the referenced `event_id` existed, triggering raw Postgres foreign key violations. Furthermore, event text fields lacked upper-bound string lengths.
* **Impact:** Postgres `value too long` exceptions and unhandled 500 errors.
* **Remediation:** Pre-validates event existence before insertion (returns `400 Bad Request` if invalid) and enforces strict length boundaries (`Title`: 200, `Slug`: 100, `EventDate`: 100, `Location`: 200, `RespondBy`: 100, `Description`: 2000). Sanitizes inputs against control characters.
* **Code Reference:** [`handlers.go`](file:///home/michaelochieng0/go-rsvp/handlers.go#L350-L365)

---

### 3.12. Incomplete Database Migration & Missing Indexes
* **Vulnerability ID:** SEC-12 (CWE-1188 / CWE-703)
* **Severity:** **Low**
* **Root Cause:** `MigrateDB` only executed `ALTER TABLE rsvps` assuming manual out-of-band table creation. Lookup queries also lacked indexes.
* **Impact:** Fresh installations failed migrations; unindexed lookups caused full table scans under load.
* **Remediation:** `MigrateDB` now creates `rsvps` idempotently with all columns, and creates indexes on `event_id`, `attending`, `slug`, and session `expires_at`.
* **Code Reference:** [`db.go`](file:///home/michaelochieng0/go-rsvp/db.go#L14-L65)

---

### 3.13. Incomplete Single-Quote Escaping in Client Template
* **Vulnerability ID:** SEC-13 (CWE-79)
* **Severity:** **Low**
* **Root Cause:** `escapeHtml` in `static/index.html` escaped `&`, `<`, `>`, and `"`, but omitted single quotes (`'`).
* **Impact:** Attribute-context XSS risk if single-quoted HTML attributes were used.
* **Remediation:** Added `.replace(/'/g, '&#039;')` and null/undefined handling.
* **Code Reference:** [`static/index.html`](file:///home/michaelochieng0/go-rsvp/static/index.html#L645-L655)

---

### 3.14. Hardcoded Database Fallback Credentials
* **Vulnerability ID:** SEC-14 (CWE-798)
* **Severity:** **Low**
* **Root Cause:** Redundant duplicate `InitDB` function in `db.go` hardcoded default Postgres credentials.
* **Impact:** Potential connection to unintended local instances if environment variables were misconfigured.
* **Remediation:** Replaced redundant code in `db.go` to delegate to `initDB()`, requiring explicit environment configuration, and added a prominent console warning when default admin passwords are used.
* **Code Reference:** [`db.go`](file:///home/michaelochieng0/go-rsvp/db.go#L60-L75)

---

## 4. Verification & Testing

All security fixes were validated through automated test runs in [`rsvp_test.go`](file:///home/michaelochieng0/go-rsvp/rsvp_test.go):

1. **Unit & Integration Suite:**
   * Health check and request ID tracing: **PASS**
   * Unauthenticated vs. Authenticated `/rsvp` access: **PASS**
   * Negative input validation & edge-case rejection: **PASS**
   * Idempotent upsert & headcount computation: **PASS**
   * Concurrent upsert race conditions: **PASS**
   * Admin authentication, session creation & invalidation: **PASS**
   * Multi-event creation & attendee breakdown: **PASS**
2. **Dedicated Security Test Cases:**
   * `Security_Headers_Enforcement`: **PASS**
   * `Path_Traversal_and_File_Inclusion_Prevention`: **PASS**
   * `Referenced_Event_Integrity_Validation`: **PASS**
   * `Rate_Limiting_Defense`: **PASS**
3. **Data Race Analysis:**
   * Executed `go test -v -race ./...`: **0 Data Races Detected (Clean Exit Code 0)**.

---

## 5. Deployment Recommendations for Production

1. **Set Strong Environment Variables:**
   * Ensure `ADMIN_PASSWORD` is a strong random secret (>= 16 characters).
   * Ensure `DATABASE_URL` connects over TLS (`sslmode=require` or `verify-full`).
2. **Reverse Proxy Configuration:**
   * If terminating TLS at Nginx, Cloudflare, or AWS ALB, ensure `X-Forwarded-Proto: https` is forwarded to ensure cookies receive the `Secure` flag and HSTS headers are emitted.
3. **Log Monitoring:**
   * Monitor `slog` events for `rsvps_query_error`, `session_creation_failed`, and `crypto_rand_read_failed` alerts.
