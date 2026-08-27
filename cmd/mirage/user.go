package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
	"golang.org/x/term"
)

const userUsage = `Usage:
  mirage user list                 List accounts and their storage mapping
  mirage user add <username>       Create an account (see flags below)
  mirage user passwd <username>    Set an account password
  mirage user enable <username>    Re-enable a disabled account
  mirage user disable <username>   Disable an account without deleting it

Flags for "add":
  -home   Directory backing the account, as seen inside the container (required)
  -uid    Owner uid for files Mirage creates (required)
  -gid    Owner gid for files Mirage creates (required)
  -name   Display name (defaults to the username)
  -quota  Storage limit in GB (0 or omitted means unlimited)

Accounts are normally managed from the admin page at /admin. These commands
exist for scripting, and so that a server with no admin password set is still
usable. If the config file declares a users: section it is authoritative, and
changes made here are undone on the next start.
`

func cmdUser(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, userUsage)
		return errors.New("no subcommand given")
	}

	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	configPath, verbose := configFlags(fs)

	var addHome, addName string
	var addUID, addGID int
	var addQuota float64
	if sub == "add" {
		fs.StringVar(&addHome, "home", "", "directory backing the account, inside the container")
		fs.StringVar(&addName, "name", "", "display name (defaults to the username)")
		fs.IntVar(&addUID, "uid", -1, "owner uid for files Mirage creates")
		fs.IntVar(&addGID, "gid", -1, "owner gid for files Mirage creates")
		fs.Float64Var(&addQuota, "quota", 0, "storage limit in GB (0 for unlimited)")
	}

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
	cfg, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		return userList(ctx, db)
	case "add":
		return userAdd(ctx, db, log, cfg.Storage.FileMode.Perm(), cfg.Storage.DirMode.Perm(),
			cfg.Storage.Exclude, username, addHome, addUID, addGID, addName, addQuota)
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
		fmt.Println("No accounts yet. Add one from the admin page at /admin,")
		fmt.Println("or here with: mirage user add <username> -home ... -uid ... -gid ...")
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

func userAdd(ctx context.Context, db *store.DB, log *slog.Logger, fileMode, dirMode fs.FileMode,
	exclude []string, username, home string, uid, gid int, displayName string, quotaGB float64) error {
	if home == "" {
		return errors.New("-home is required; give the path as seen inside the container")
	}
	if uid < 0 || gid < 0 {
		return errors.New("-uid and -gid are required; find them with `id <username>` on the NAS")
	}

	created, err := db.CreateUser(ctx, store.UserMapping{
		Username:    username,
		DisplayName: displayName,
		Home:        home,
		UID:         uid,
		GID:         gid,
		Quota:       int64(quotaGB * 1024 * 1024 * 1024),
	})
	if err != nil {
		return err
	}

	fmt.Printf("Created %s -> %s (%d:%d).\n", created.Username, created.Home, created.UID, created.GID)

	p := fsx.ProbeHome(created.Home, created.UID, created.GID)
	switch {
	case !p.OK():
		fmt.Printf("WARN  %s %s\n", p.Path, p.Problem)
	case p.Warning != "":
		fmt.Printf("WARN  %s %s\n", p.Path, p.Warning)
	}

	// Index it now: until an account's root is in the index, every request for
	// it answers 404, and the account would look broken until the next scan.
	if p.OK() {
		excluder, _ := fsx.NewExcluder(exclude)
		storage := fsx.NewManager(fileMode, dirMode, excluder)
		defer storage.Close()
		stats, err := index.NewScanner(db, storage, log).ScanUser(ctx, created)
		if err != nil {
			fmt.Printf("WARN  could not index the account: %v\n", err)
			fmt.Println("      run `mirage scan` once the directory is reachable")
		} else {
			fmt.Printf("Indexed %d files in %d directories (%s).\n",
				stats.Files, stats.Dirs, formatBytes(stats.Bytes))
		}
	}

	// Reported rather than left to be discovered: the account cannot be used
	// until it has one, and a fresh account looks fine in `user list` otherwise.
	fmt.Printf("Set a password before connecting a client:  mirage user passwd %s\n", created.Username)
	return nil
}

func userPasswd(ctx context.Context, db *store.DB, username string) error {
	u, err := db.UserByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account %q", username)
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
