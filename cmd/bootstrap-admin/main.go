// bootstrap-admin assembles the offline CLI with identity's public port.
// Dependencies: identity and standard I/O; never handles SQL or logs credentials.
// Invariant: passwords are stdin/environment only and errors contain no input.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.LookupEnv, identity.BootstrapAdmin))
}

func run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), consume func(context.Context, identity.BootstrapRequest) (string, error)) int {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // Flag errors can contain user-supplied secrets.
	email := flags.String("email", "", "owner email")
	tenant := flags.String("tenant-name", "", "initial tenant name")
	if flags.Parse(args) != nil || flags.NArg() != 0 || strings.TrimSpace(*email) == "" || strings.TrimSpace(*tenant) == "" {
		fmt.Fprintln(stderr, "usage: bootstrap-admin --email EMAIL --tenant-name NAME; password via stdin or OSC_BOOTSTRAP_PASSWORD")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dsn, _ := lookupEnv("OSC_BOOTSTRAP_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		dsn, _ = lookupEnv("DATABASE_URL")
	}
	password, exists := lookupEnv("OSC_BOOTSTRAP_PASSWORD")
	if !exists {
		if !safePasswordInput(input) {
			fmt.Fprintln(stderr, "bootstrap input failed")
			return 1
		}
		type result struct {
			data []byte
			err  error
		}
		read := make(chan result, 1)
		go func() { data, err := io.ReadAll(io.LimitReader(input, 1027)); read <- result{data, err} }()
		select {
		case got := <-read:
			if got.err != nil {
				fmt.Fprintln(stderr, "bootstrap input failed")
				return 1
			}
			password = string(got.data)
			if strings.HasSuffix(password, "\n") {
				password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
			}
		case <-ctx.Done():
			fmt.Fprintln(stderr, "bootstrap timed out")
			return 1
		}
	}
	if len(password) < 12 || len(password) > 1024 || !utf8.ValidString(password) || strings.ContainsAny(password, "\r\n\x00") || strings.TrimSpace(dsn) == "" || ctx.Err() != nil {
		fmt.Fprintln(stderr, "bootstrap input failed")
		return 1
	}
	id, err := consume(ctx, identity.BootstrapRequest{Email: *email, TenantName: *tenant, Password: password, DatabaseURL: dsn})
	if err != nil || ctx.Err() != nil {
		fmt.Fprintln(stderr, "bootstrap failed")
		return 1
	}
	fmt.Fprintf(stdout, "bootstrap complete: %s\n", id)
	return 0
}

// Files must be redirected: terminal echo can expose a typed password. The
// CLI accepts pipes/files or environment input without a terminal dependency.
func safePasswordInput(input io.Reader) bool {
	file, ok := input.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return true
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}
