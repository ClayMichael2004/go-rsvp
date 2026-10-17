### to start :
first - go mod tidy
psql
### run this query:

CREATE DATABASE rsvp_db;

\c rsvp_db;

CREATE TABLE IF NOT EXISTS rsvps (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    email VARCHAR(150) UNIQUE NOT NULL,
    attending BOOLEAN NOT NULL,
    guests_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);


### windows: 

PS C:\Users\Clay\go-rsvp> $env:DATABASE_URL="postgres://postgres:PASSWORD@localhost:5432/rsvp_db?sslmode=disable"
PS C:\Users\Clay\go-rsvp> go run .                                         

### linux/ubuntu: 

DATABASE_URL="postgres://postgres:PASSWORD@localhost:5432/rsvp_db?sslmode=disable" go run .

## at work use:
 DATABASE_URL="postgres://michaelochieng0@127.0.0.1:5433/rsvp_db?sslmode=disable" go run .

remember: After postgres: <insert your postgres password and not how it is as of now>. Port also to be changed based on the port you are using at the moment

### postgres database connection

## Start the Database Server
pg_ctl -D /home/michaelochieng0/postgres_data -o "-p 5433" start

## Connect to the Database (this also allows you to interact with the db via terminal)
psql -h 127.0.0.1 -p 5433 -U michaelochieng0 -d rsvp_db 

## Stop the Database Server
pg_ctl -D /home/michaelochieng0/postgres_data stop
`

### using tailscale
make sure its running and check the port its running on then rplace it with 8080 if need.
## serve tailscale to the port
tailscale serve 8080 : authenticate, connect the the secure ip
## open a public gateway and serve it to the net
tailscale funnel 8080 on
## close/turn it off
tailscale serve off