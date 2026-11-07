// Command migrate applies pending database migrations. Run it with the
// migration role, not the application role.
package main

import (
	"context"
	"fmt"
	"os"

	"navo-api/db/migrations"
	"navo-api/internal/db"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	applied, err := db.Migrate(ctx, pool, migrations.FS)
	for _, v := range applied {
		fmt.Println("applied", v)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(applied) == 0 {
		fmt.Println("database is up to date")
	}
}
