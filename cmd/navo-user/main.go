// Command navo-user creates an account from the command line, e.g. the first
// Admin of a new installation. The password is read from NAVO_USER_PASSWORD so
// it never appears in shell history or process lists.
//
//	NAVO_USER_PASSWORD=... go run ./cmd/navo-user -email admin@congty.vn -name "Quản trị" -roles admin
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"navo-api/internal/db"
	"navo-api/internal/users"
)

func main() {
	email := flag.String("email", "", "login email")
	name := flag.String("name", "", "full name")
	roles := flag.String("roles", "", "comma-separated roles: sales,leader,accounting,admin")
	grants := flag.String("grants", "", "comma-separated grants, e.g. purchase_cost.read,finance.read")
	flag.Parse()

	in := users.Input{Email: *email, FullName: *name, Password: os.Getenv("NAVO_USER_PASSWORD"), Roles: split(*roles), Grants: split(*grants)}
	if err := in.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "invalid input:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		id, err := users.Create(ctx, tx, in, nil)
		if err == nil {
			fmt.Println("created user", id)
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
