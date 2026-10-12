package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func MigrateDB(pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
	CREATE TABLE IF NOT EXISTS events (
		id SERIAL PRIMARY KEY,
		title VARCHAR(200) NOT NULL,
		slug VARCHAR(100) UNIQUE NOT NULL,
		description TEXT DEFAULT '',
		event_date VARCHAR(100) DEFAULT '',
		location VARCHAR(200) DEFAULT '',
		respond_by VARCHAR(100) DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);

	ALTER TABLE rsvps ADD COLUMN IF NOT EXISTS event_id INT REFERENCES events(id) ON DELETE SET NULL;
	ALTER TABLE rsvps ADD COLUMN IF NOT EXISTS guest_details TEXT[] DEFAULT '{}';
	ALTER TABLE rsvps ADD COLUMN IF NOT EXISTS decline_reason TEXT DEFAULT '';
	ALTER TABLE rsvps ADD COLUMN IF NOT EXISTS dietary VARCHAR(100) DEFAULT '';

	CREATE TABLE IF NOT EXISTS admin_users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(50) UNIQUE NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS admin_sessions (
		token VARCHAR(64) PRIMARY KEY,
		admin_id INT REFERENCES admin_users(id) ON DELETE CASCADE,
		expires_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := pool.Exec(ctx, query); err != nil {
		return err
	}

	// Seed default admin if no admin user exists
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM admin_users").Scan(&count); err == nil && count == 0 {
		username := os.Getenv("ADMIN_USERNAME")
		if username == "" {
			username = "admin"
		}
		password := os.Getenv("ADMIN_PASSWORD")
		if password == "" {
			password = "admin123"
		}
		hash, hashErr := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if hashErr == nil {
			_, _ = pool.Exec(ctx, "INSERT INTO admin_users (username, password_hash) VALUES ($1, $2)", username, string(hash))
		}
	}

	return nil
}

func InitDB() *pgxpool.Pool {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5433/rsvp_db?sslmode=disable"
	}

	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		log.Fatalf("Unable to parse DB config: %v\n", err)
	}
	config.MaxConns = 10
	config.MinConns = 2
	config.MaxConnLifetime = 1 * time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Fatalf("Unable to create connection pool %v\n", err)
	}

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("Unable to connect to database: %v\n", err)
	}

	if err := MigrateDB(pool); err != nil {
		log.Fatalf("Unable to migrate database: %v\n", err)
	}

	fmt.Println("postgres database connected successfuly")
	return pool
}