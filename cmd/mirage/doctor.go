package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/fsx"
)

// cmdDoctor checks the parts of the setup that fail at runtime rather than at
// parse time: whether each mapped home actually exists in the container, and
// whether its ownership matches what Mirage has been told to stamp on files.
// Getting this wrong on a Synology is the most likely first-run problem, and it
// otherwise shows up as opaque permission errors mid-sync.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath, verbose := configFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	fmt.Printf("config      %s\n", *configPath)
	fmt.Printf("database    %s\n", cfg.Database.Path)
	fmt.Printf("listen      %s\n", cfg.Server.Listen)
	fmt.Printf("external    %s\n", cfg.Server.ExternalURL)
	fmt.Printf("modes       files %s  dirs %s\n", cfg.Storage.FileMode, cfg.Storage.DirMode)
	fmt.Printf("running as  uid %d gid %d\n\n", os.Getuid(), os.Getgid())

	if os.Getuid() != 0 {
		fmt.Println("WARN  not running as root: Mirage cannot chown files to each user's")
		fmt.Println("      uid/gid, so files will land owned by the current user. This is")
		fmt.Println("      fine for local development but wrong on the NAS.")
		fmt.Println()
	}

	problems := 0
	for _, u := range cfg.Users {
		fmt.Printf("user %s\n", u.Username)
		fmt.Printf("  home  %s\n", u.Home)
		fmt.Printf("  owner uid %d gid %d\n", u.UID, u.GID)

		p := fsx.ProbeHome(u.Home, u.UID, u.GID)
		if p.OwnerKnown {
			fmt.Printf("  disk  uid %d gid %d mode %s\n", p.OwnerUID, p.OwnerGID, p.Mode)
		}
		switch {
		case !p.OK():
			fmt.Printf("  FAIL  %s\n", p.Problem)
			problems++
		default:
			fmt.Printf("  ok    readable and writable\n")
		}
		if p.Warning != "" {
			fmt.Printf("  WARN  %s\n", p.Warning)
		}
		fmt.Println()
	}

	if len(cfg.Users) == 0 {
		fmt.Println("No accounts are declared in the config file.")
		fmt.Println("They are managed through the admin page; run the server and open /admin.")
		fmt.Println()
	}

	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	log.Debug("doctor completed with no problems")
	fmt.Println("No problems found.")
	return nil
}
