// Command migrate applies and rolls back the database schema.
//
// The migration set is embedded (server/migrations), so this binary is the
// only thing that has to be deployed to move a database forward.
//
//	migrate up         apply every pending migration
//	migrate down       roll back the most recent migration
//	migrate down-all   roll back every migration (development only)
//	migrate status     show which migrations are applied
//	migrate version    print the current schema version
//
// Configuration comes from DATABASE_URL. Nothing is ever printed with the
// password in it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ChinnakornP/longtest/server/pkg/db"
)

const usage = `usage: migrate <up|down|down-all|status|version>

  up         apply every pending migration
  down       roll back the most recent migration
  down-all   roll back every migration (development only)
  status     show which migrations are applied
  version    print the current schema version

Configuration is read from DATABASE_URL.
`

// migrationTimeout bounds the whole command. A migration that has not finished
// by then is almost certainly blocked behind a lock, and hanging forever in a
// deploy pipeline is worse than failing.
const migrationTimeout = 5 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("expected exactly one subcommand")
	}
	command := args[0]

	dsn, err := db.DSNFromEnv()
	if err != nil {
		return fmt.Errorf("%w (copy .env.example to .env)", err)
	}

	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	// Never print the DSN as-is: it carries the database password.
	fmt.Printf("migrate %s against %s\n", command, db.RedactDSN(dsn))

	migrator, err := db.NewMigrator(ctx, dsn, os.Stdout)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := migrator.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "migrate:", cerr)
		}
	}()

	switch command {
	case "up":
		return migrator.Up(ctx)
	case "down":
		return migrator.Down(ctx)
	case "down-all":
		return migrator.DownAll(ctx)
	case "status":
		return migrator.Status(ctx)
	case "version":
		v, verr := migrator.Version(ctx)
		if verr != nil {
			return verr
		}
		fmt.Println(v)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown subcommand %q", command)
	}
}
