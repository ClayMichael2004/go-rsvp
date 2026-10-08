package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
	"github.com/jackc/pgx/v5/pgxpool"
)

func InitDB() *pgxpool.Pool{
	connStr:= os.Getenv("DATABASE_URL")
	if connStr==""{
		connStr = "postgres://postgres:postgres@localhost:5433/rsvp_db?sslmode=disable"
	}

	config, err:= pgxpool.ParseConfig(connStr)
	if err!=nil{
		log.Fatalf("Unable to parse DB config: %v\n", err)
	}
	config.MaxConns = 10
	config.MinConns = 2
	config.MaxConnLifetime = 1 * time.Hour

	ctx, cancel:= context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err:=pgxpool.NewWithConfig(ctx, config)
	if err!=nil{
		log.Fatalf("Unable to create connection pool %v\n", err)
	}

	if err:= pool.Ping(ctx); err!=nil{
		log.Fatalf("Unable to connect to database: %v\n", err)
	}
	fmt.Println("postgres database connected successfuly")
	return pool
}