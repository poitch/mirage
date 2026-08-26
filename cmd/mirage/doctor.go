package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

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

		fi, err := os.Stat(u.Home)
		switch {
		case os.IsNotExist(err):
			fmt.Printf("  FAIL  home does not exist inside the container.\n")
			fmt.Printf("        Check the bind mount for %s.\n", filepath.Dir(u.Home))
			problems++
		case err != nil:
			fmt.Printf("  FAIL  cannot stat home: %v\n", err)
			problems++
		case !fi.IsDir():
			fmt.Printf("  FAIL  home exists but is not a directory\n")
			problems++
		default:
			if uid, gid, ok := fsx.Owner(fi); ok {
				fmt.Printf("  disk  uid %d gid %d mode %s\n", uid, gid, fi.Mode().Perm())
				if uid != u.UID || gid != u.GID {
					// Not fatal: Mirage stamps its configured uid/gid onto what
					// it writes regardless. But a mismatch usually means the
					// config was copied from another user's block.
					fmt.Printf("  WARN  home is owned by %d:%d but config says %d:%d;\n",
						uid, gid, u.UID, u.GID)
					fmt.Printf("        new files will not match their own directory\n")
				}
			}
			if err := checkWritable(u.Home); err != nil {
				fmt.Printf("  FAIL  home is not writable: %v\n", err)
				problems++
			} else {
				fmt.Printf("  ok    readable and writable\n")
			}
		}
		fmt.Println()
	}

	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	log.Debug("doctor completed with no problems")
	fmt.Println("No problems found.")
	return nil
}

// checkWritable confirms the directory accepts a create-and-remove cycle.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".mirage-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
