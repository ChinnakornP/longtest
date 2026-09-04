// Command migrate applies and rolls back the database schema.
//
// Stage-1 placeholder: the migration set and its runner are owned by T02.
// The command already validates its arguments and its database configuration
// so `make migrate-up` / `make migrate-down` are wired from the first commit.
package main

import (
	"fmt"
	"os"

	"github.com/ChinnakornP/longtest/server/pkg/db"
)

const usage = `usage: migrate <up|down>

  up     apply every pending migration
  down   roll back the most recent migration

Configuration is read from DATABASE_URL.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("expected exactly one direction argument")
	}

	direction := args[0]
	if direction != "up" && direction != "down" {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown direction %q", direction)
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set (copy .env.example to .env)")
	}

	// Never print the DSN as-is: it carries the database password.
	fmt.Printf("migrate %s against %s\n", direction, db.RedactDSN(dsn))
	fmt.Println("no migrations defined yet - the schema in contract G lands in T02")
	return nil
}
