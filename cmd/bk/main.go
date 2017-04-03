// cmd/bk/main.go
// Copyright(c) 2017 Matt Pharr
// BSD licensed; see LICENSE for details.

package main

import (
	"flag"
	"fmt"
	"github.com/mmp/bk/storage"
	u "github.com/mmp/bk/util"
	"io"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"time"
)

var log *u.Logger

func usage() {
	fmt.Printf(`usage: bk [bk flags...] <command> [command args...]
where <command> is: backup, fsck, help, init, list, restore, restorebits, savebits.
Run "bk help" for more detailed help.
`)
	os.Exit(1)
}

func help() {
	fmt.Printf(`
bk is a tool for backing up data.  It can be given the path to a directory
to recursively back up the contents of, or alternatively, it can be given a
bitstream to be backed up from standard input (typically, the output of the
"tar" command). In either case, it stores the resulting data in manner that
minimizes storage requirements for data that is unchanged across multiple
backups and/or is repeated within a single backup.

Environment variables:
- BK_DIR: Directory where backups are stored. If prefixed with "gs://", is taken
  to refer to a Google Cloud Storage bucket.
- BK_GCS_PROJECT_ID: If Google Cloud Storage is being used, the name of the
  project you're using for billing. (Create using the Google Cloud console).
- BK_PASSPHRASE: if encryption is being used, the encryption passphrase.

usage: bk [bk flags...] <command> [command_options ...]

General bk flags are: [--verbose] [--debug] [--profile]

Commands and their options are:
  backup [--split-bits count] [--base base] <backup name> <directory>
      Make a back up of <directory>, including the contents of all
      subdirectories, with the given name in the given bk repository.  The
      --split-bits option can be used to control how large the blobs
      generated by the splitting algorithm are, and --base can be used to
      specify a base backup for incremental backups. Backup names
      must be unique.
           
  fsck
      Check integrity of the bk repository.

  help
      Prints this help message.

  init [--encrypt]
      Initialize a new backup repository in the given directory. If backups
      in this repository should be encrypted, the --encrypt option should
      be given.

  list
      List names of all backups and archived bitstreams.

  restore <backup name> <target dir>
      Restore the named backup to the specified target directory.

  restorebits <bits name>
      Restore the named bitstream, printing its contents to standard output.

  savebits [--split-bits bits] <bits name>
      Save the bitstream given in standard input to the given name.

`)
	os.Exit(0)
}

func lookupHash(name string, backend storage.Backend) (hash storage.Hash) {
	if !backend.MetadataExists(name) {
		fmt.Fprintf(os.Stderr, "%s: backup not found\n", name)
		os.Exit(1)
	}

	b := backend.ReadMetadata(name)
	return storage.NewHash(b)
}

func Error(s string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, s, args...)
	os.Exit(1)
}

func InitStorage(encrypt bool) {
	backend := getBaseBackend()
	if encrypt {
		passphrase := os.Getenv("BK_PASSPHRASE")
		if passphrase == "" {
			Error("BK_PASSPHRASE environment variable not set.\n")
		}
		backend = storage.NewEncrypted(backend, passphrase)
	}
	backend = storage.NewCompressed(backend)

	backend.WriteMetadata("readme_bk.txt", []byte(readmeText))
	backend.SyncWrites()
}

func getBaseBackend() storage.Backend {
	path := os.Getenv("BK_DIR")
	if path == "" {
		Error("BK_DIR: environment variable not set.\n")
	}

	if strings.HasPrefix(path, "gs://") {
		projectId := os.Getenv("BK_GCS_PROJECT_ID")
		if projectId == "" {
			Error("BK_GCS_PROJECT_ID environment variable not set.\n")
		}
		return storage.NewGCS(storage.GCSOptions{
			BucketName: strings.TrimPrefix(path, "gs://"),
			ProjectId:  projectId,
			// TODO: make it possible to specify these via command-line
			// args.
			MaxUploadBytesPerSecond:   900 * 1024,
			MaxDownloadBytesPerSecond: 5 * 1024 * 1024,
		})
	}
	return storage.NewDisk(path)
}

func GetStorageBackend() storage.Backend {
	backend := getBaseBackend()
	if backend.MetadataExists("encrypt.txt") {
		passphrase := os.Getenv("BK_PASSPHRASE")
		if passphrase == "" {
			Error("BK_PASSPHRASE environment variable not set.\n")
		}
		backend = storage.NewEncrypted(backend, passphrase)
	}
	backend = storage.NewCompressed(backend)

	if !backend.MetadataExists("readme_bk.txt") {
		Error("%s: destination hasn't been initialized. Run 'bk init'.\n",
			backend.String())
	}

	return backend
}

///////////////////////////////////////////////////////////////////////////

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	debug := false
	verbose := false
	profile := false
	idx := 1
	for os.Args[idx][0] == '-' {
		switch os.Args[idx] {
		case "--debug":
			debug = true
			idx++
		case "--verbose":
			verbose = true
			idx++
		case "--profile":
			profile = true
			idx++
		default:
			usage()
		}
	}
	log = u.NewLogger(verbose, debug)
	storage.SetLogger(log)

	cmd := os.Args[idx]
	idx++

	if profile {
		log.Print("Starting profiling.")
		f, err := os.Create("bk.prof")
		log.CheckError(err)
		pprof.StartCPUProfile(f)
	}

	// Dispatch to the appropriate command.
	switch cmd {
	case "help":
		help()
	case "backup":
		backup(os.Args[idx:])
	case "fsck":
		fsck(os.Args[idx:])
	case "init":
		initcmd(os.Args[idx:])
	case "list":
		list(os.Args[idx:])
	case "restore":
		restore(os.Args[idx:])
	case "restorebits":
		restorebits(os.Args[idx:])
	case "savebits":
		savebits(os.Args[idx:])
	default:
		usage()
	}

	if profile {
		pprof.StopCPUProfile()
	}

	os.Exit(log.NErrors)
}

///////////////////////////////////////////////////////////////////////////

func backup(args []string) {
	// Parse args
	flags := flag.NewFlagSet("backup", flag.ExitOnError)
	flags.Usage = func() {
		Error("usage: bk backup [--base name] [--split-bits count] <name> <dir>\n")
	}
	base := flags.String("base", "", "base backup (for incremental backups)")
	splitBits := flags.Uint("split-bits", 14,
		"matching bits for rolling checksum")
	err := flags.Parse(args)
	if err == flag.ErrHelp || flags.NArg() != 2 {
		flags.Usage()
	} else if err != nil {
		log.Fatal("%s", err)
	}

	backend := GetStorageBackend()
	name := flags.Arg(0) + "-" + time.Now().Format("20060102-150405")
	dir := flags.Arg(1)

	log.Check(!backend.MetadataExists("backup-" + name))

	var hash storage.Hash
	if *base != "" {
		baseHash := lookupHash("backup-"+*base, backend)
		hash = BackupDirIncremental(dir, baseHash, backend, *splitBits)
	} else {
		hash = BackupDir(dir, backend, *splitBits)
	}

	// Get all of the data on disk before we save the named hash.
	backend.SyncWrites()

	backend.WriteMetadata("backup-"+name, hash[:])
	backend.SyncWrites()

	log.Print("%s: successfully saved backup", name)
	backend.LogStats()
}

///////////////////////////////////////////////////////////////////////////

func fsck(args []string) {
	if len(args) != 0 {
		Error("usage: bk fsck <bk dir>\n")
	}

	backend := GetStorageBackend()

	for name := range backend.ListMetadata() {
		if strings.HasPrefix(name, "bits-") {
			log.Debug("Checking %s", name)
			b := backend.ReadMetadata(name)
			sh := storage.NewMerkleHash(b)
			sh.Fsck(backend)
		} else if strings.HasPrefix(name, "backup-") {
			log.Debug("Checking %s", name)
			h := lookupHash(name, backend)
			r, err := NewBackupReader(h, backend)
			if err != nil {
				log.Error("%s", err)
			}
			r.Fsck()
		}
	}

	// Let the storage do its thing.
	backend.Fsck()

	backend.LogStats()
}

///////////////////////////////////////////////////////////////////////////

func initcmd(args []string) {
	if len(args) == 1 && args[0] == "--encrypt" {
		InitStorage(true)
	} else if len(args) == 0 {
		InitStorage(false)
	} else {
		Error("usage: bk init [--encrypt]\n")
	}
}

///////////////////////////////////////////////////////////////////////////

func list(args []string) {
	if len(args) != 0 {
		Error("usage: bk list\n")
	}

	backend := GetStorageBackend()
	md := backend.ListMetadata()

	var backups, bits []string
	for n := range md {
		if strings.HasPrefix(n, "bits-") {
			bits = append(bits, n)
		} else if strings.HasPrefix(n, "backup-") {
			backups = append(backups, n)
		}
	}

	if len(backups) > 0 {
		sort.Strings(backups)
		fmt.Printf("Total of %d backups:\n", len(backups))
		for _, name := range backups {
			fmt.Printf("  %-30s %s\n",
				strings.TrimPrefix(name, "backup-"), md[name].String())
		}
	}
	if len(bits) > 0 {
		sort.Strings(bits)
		fmt.Printf("Total of %d bitstreams:\n", len(bits))
		for _, name := range bits {
			fmt.Printf("  %-30s %s\n",
				strings.TrimPrefix(name, "bits-"), md[name].String())
		}
	}
}

///////////////////////////////////////////////////////////////////////////

func restore(args []string) {
	if len(args) != 2 {
		Error("usage: bk restore <name> <dir>\n")
	}

	backend := GetStorageBackend()
	if !backend.MetadataExists("backup-" + args[0]) {
		Error("%s: backup not found\n", args[0])
	}
	b := backend.ReadMetadata("backup-" + args[0])
	r, err := NewBackupReader(storage.NewHash(b), backend)
	if err != nil {
		log.Error("%s", err)
	}

	err = r.Restore("/", args[1])
	if err != nil {
		log.Error("%s", err)
	}
	backend.LogStats()
}

///////////////////////////////////////////////////////////////////////////

func restorebits(args []string) {
	if len(args) != 1 {
		Error("usage: bk restorebits <backup name>\n")
	}

	backend := GetStorageBackend()

	name := args[0]
	if !backend.MetadataExists("bits-" + name) {
		Error("%s: named backup not found\n", name)
	}

	hash := storage.NewMerkleHash(backend.ReadMetadata("bits-" + name))

	r := hash.NewReader(nil, backend)
	// Write the blob contents to stdout.
	rr := &u.ReportingReader{R: r, Msg: "Restored"}
	_, err := io.Copy(os.Stdout, rr)
	if err != nil {
		log.Fatal("%s: %s", name, err)
	}
	err = rr.Close()
	if err != nil {
		log.Fatal("%s: %s", name, err)
	}

	backend.LogStats()
}

///////////////////////////////////////////////////////////////////////////

func savebits(args []string) {
	// Parse args
	flags := flag.NewFlagSet("savebits", flag.ExitOnError)
	flags.Usage = func() {
		Error("usage: bk savebits [--split-bits bits] <backup name>\n")
	}
	splitBits := flags.Uint("split-bits", 14,
		"matching bits for rolling checksum")
	err := flags.Parse(args)
	if err == flag.ErrHelp || flags.NArg() != 1 {
		flags.Usage()
	} else if err != nil {
		log.Fatal("%s", err)
	}

	backend := GetStorageBackend()
	name := flags.Arg(0) + "-" + time.Now().Format("20060102-150405")
	log.Check(!backend.MetadataExists("bits-" + name))

	r := &u.ReportingReader{R: os.Stdin, Msg: "Read"}
	backupHash := storage.SplitAndStore(r, backend, *splitBits)
	r.Close()

	// Sync before saving the named hash.
	backend.SyncWrites()

	backend.WriteMetadata("bits-"+name, backupHash.Bytes())
	backend.SyncWrites()

	log.Print("%s: successfully saved bits", name)
	backend.LogStats()
}
