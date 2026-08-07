// Command modelctl downloads and verifies the ONNX model files the service
// needs.
//
// Models are not committed to the repository: they are large, and they carry
// licences of their own. This command is what makes a fresh checkout able to
// reproduce the exact same set of files.
//
//	modelctl download   fetch anything missing, verify against the manifest
//	modelctl verify     check what is on disk, without touching the network
//	modelctl pin        fetch everything and record the digests observed
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "modelctl: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("modelctl", flag.ContinueOnError)
	fs.SetOutput(errOut)

	manifestPath := fs.String("manifest", "models/manifest.json", "path to the manifest file")
	dir := fs.String("dir", "models", "directory the model files live in")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall timeout for network work")
	force := fs.Bool("force", false, "pin: re-download and re-record artifacts that are already pinned")

	fs.Usage = func() {
		fmt.Fprint(errOut, "usage: modelctl [flags] <download|verify|pin>\n\n"+
			"  download  fetch anything missing, verify against the manifest\n"+
			"  verify    check what is on disk, without touching the network\n"+
			"  pin       fetch everything and record the digests observed\n\nflags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one command")
	}

	m, err := loadManifest(*manifestPath)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	switch cmd := fs.Arg(0); cmd {
	case "download":
		if err := ensure(ctx, m, *dir, client, out); err != nil {
			return err
		}
		return verify(m, *dir, out)

	case "verify":
		return verify(m, *dir, out)

	case "pin":
		if err := pin(ctx, m, *dir, client, out, *force); err != nil {
			return err
		}
		if err := saveManifest(*manifestPath, m); err != nil {
			return err
		}
		fmt.Fprintf(out, "updated  %s\n", *manifestPath)
		return nil

	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}
