package main

import (
	"log"

	"finnri/internal/database"
)

// migrate is the only production schema-change entry point. Railway runs this
// binary before replacing the live server, so a schema disagreement fails the
// deployment without turning an otherwise healthy server restart into an
// outage. The server binary deliberately performs no schema writes.
func main() {
	database.Connect()
	if err := database.Migrate(); err != nil {
		log.Fatalf("database migration failed: %v", err)
	}
	log.Println("database migration completed")
}
