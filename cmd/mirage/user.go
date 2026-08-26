package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/store"
	"golang.org/x/term"
)

const userUsage = `Usage:
  mirage user list                 List accounts and their storage mapping
  mirage user passwd <username>    Set an account password
  mirage user enable <username>    Re-enable a disabled account
  mirage user disable <username>   Disable an account without deleting it

Accounts themselves are defined in the config file. To add or remove one, edit
the config and restart; these commands manage credentials and state only.
`

func cmdUser(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, userUsage)
		return errors.New("no subcommand given")
	}

	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	configPath, verbose := configFlags(fs)

	// The username is positional and precedes the flags, so split it out before
	// parsing.
	var username string
	if sub != "list" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprint(os.Stderr, userUsage)
			return fmt.Errorf("user %s requires a username", sub)
		}
		username, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}

	log := newLogger(*verbose)
	_, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		return userList(ctx, db)
	case "passwd":
		return userPasswd(ctx, db, username)
	case "enable", "disable":
		return userSetDisabled(ctx, db, username, sub == "disable")
	default:
		fmt.Fprint(os.Stderr, userUsage)
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func userList(ctx context.Context, db *store.DB) error {
	users, err := db.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("No users. Define them in the config file.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USERNAME\tDISPLAY\tUID:GID\tQUOTA\tPASSWORD\tSTATE\tHOME")
	for _, u := range users {
		quota := "unlimited"
		if u.Quota > 0 {
			quota = formatBytes(u.Quota)
		}
		password := "set"
		if u.PasswordHash == "" {
			password = "NOT SET"
		}
		state := "enabled"
		if u.Disabled {
			state = "disabled"
		}
		fmt.Fprintf(w, "%s\t%s\t%d:%d\t%s\t%s\t%s\t%s\n",
			u.Username, u.DisplayName, u.UID, u.GID, quota, password, state, u.Home)
	}
	return w.Flush()
}

func userPasswd(ctx context.Context, db *store.DB, username string) error {
	u, err := db.UserByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q; accounts are defined in the config file", username)
	} else if err != nil {
		return err
	}

	password, err := readNewPassword(username)
	if err != nil {
		return err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := db.SetPasswordHash(ctx, u.ID, hash); err != nil {
		return err
	}
	fmt.Printf("Password updated for %s.\n", username)
	return nil
}

func userSetDisabled(ctx context.Context, db *store.DB, username string, disabled bool) error {
	u, err := db.UserByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q", username)
	} else if err != nil {
		return err
	}
	if err := db.SetDisabled(ctx, u.ID, disabled); err != nil {
		return err
	}
	if disabled {
		fmt.Printf("%s disabled. The lock survives restarts and config reloads;\n", username)
		fmt.Printf("run \"mirage user enable %s\" to undo it.\n", username)
	} else {
		fmt.Printf("%s enabled.\n", username)
	}
	return nil
}

// minPasswordLen is the shortest account password accepted. Account passwords
// are only used for interactive login; sync clients authenticate with generated
// app passwords instead.
const minPasswordLen = 8

// readNewPassword collects a password for username.
//
// On a terminal it prompts twice with echo disabled and requires the two to
// match. When stdin is a pipe it reads a single line and skips confirmation,
// because a second prompt could never be satisfied from a one-line pipe and
// makes the command unusable from a script or a `docker exec`.
func readNewPassword(username string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("read password from stdin: %w", err)
			}
			return "", errors.New("no password on stdin")
		}
		password := strings.TrimRight(scanner.Text(), "\r")
		if len(password) < minPasswordLen {
			return "", fmt.Errorf("password must be at least %d characters", minPasswordLen)
		}
		return password, nil
	}

	password, err := promptPassword(fmt.Sprintf("New password for %s: ", username))
	if err != nil {
		return "", err
	}
	if len(password) < minPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	confirm, err := promptPassword("Retype password: ")
	if err != nil {
		return "", err
	}
	if password != confirm {
		return "", errors.New("passwords do not match")
	}
	return password, nil
}

// promptPassword reads one line from the terminal with echo disabled.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
