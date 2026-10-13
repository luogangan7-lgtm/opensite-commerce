// Tests pin CLI secret handling, source precedence and bounded context.
// Dependency: the public identity port; the consumer does not assert SQL behavior.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
)

func TestCLIInputSourcesAndTimeout(t *testing.T) {
	for _, fromEnv := range []bool{false, true} {
		env := map[string]string{"DATABASE_URL": "fallback-dsn", "OSC_BOOTSTRAP_DATABASE_URL": "bootstrap-dsn"}
		if fromEnv {
			env["OSC_BOOTSTRAP_PASSWORD"] = "env-passphrase"
		}
		var out bytes.Buffer
		code := run(context.Background(), []string{"--email", "a@example.com", "--tenant-name", "Store"}, strings.NewReader("stdin-passphrase\n"), &out, &out, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, func(ctx context.Context, input identity.BootstrapRequest) (string, error) {
			if input.DatabaseURL != "bootstrap-dsn" {
				t.Fatal("dsn precedence")
			}
			want := "stdin-passphrase"
			if fromEnv {
				want = "env-passphrase"
			}
			if input.Password != want {
				t.Fatal("password source")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Fatal("missing bounded context")
			}
			return "owner-id", nil
		})
		if code != 0 || out.String() != "bootstrap complete: owner-id\n" {
			t.Fatal("unexpected success output")
		}
	}
}

type statReader struct {
	io.Reader
	info  os.FileInfo
	reads int
}

func (r *statReader) Stat() (os.FileInfo, error) { return r.info, nil }
func (r *statReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }

func TestCLICharacterDeviceGuardAndEnvBypass(t *testing.T) {
	device, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	info, err := device.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, envPassword := range []bool{false, true} {
		input := &statReader{Reader: strings.NewReader("visible-terminal-password"), info: info}
		var out bytes.Buffer
		called := false
		code := run(context.Background(), []string{"--email", "a@example.com", "--tenant-name", "Store"}, input, &out, &out, func(k string) (string, bool) {
			if k == "DATABASE_URL" {
				return "dsn", true
			}
			if k == "OSC_BOOTSTRAP_PASSWORD" && envPassword {
				return "env-passphrase", true
			}
			return "", false
		}, func(context.Context, identity.BootstrapRequest) (string, error) {
			called = true
			return "owner-id", nil
		})
		if called != envPassword || (code == 0) != envPassword || input.reads != 0 {
			t.Fatal("character device read or environment path denied")
		}
		if strings.Contains(out.String(), "visible-terminal-password") {
			t.Fatal("terminal secret printed")
		}
	}
}

func TestSafePasswordInputUsesFileModeAndStatFailure(t *testing.T) {
	device, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	if safePasswordInput(device) {
		t.Fatal("character device accepted")
	}
	device.Close()
	if safePasswordInput(device) {
		t.Fatal("stat failure accepted")
	}
	file, err := os.Open("main.go")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if !safePasswordInput(file) {
		t.Fatal("regular file denied")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if !safePasswordInput(reader) || !safePasswordInput(strings.NewReader("password")) {
		t.Fatal("pipe or non-file reader denied")
	}
}

func TestCLIRedactsFlagsAndConsumerErrors(t *testing.T) {
	secret := "secret-dsn-password-PHC"
	env := func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return secret, true
		}
		return "", false
	}
	for _, args := range [][]string{{"--password", secret}, {"--email", secret, "--unknown", secret}, {"--email", "a@example.com", "--tenant-name", "Store", secret}, {"--email", "a@example.com", "--tenant-name", "Store"}} {
		var out bytes.Buffer
		code := run(context.Background(), args, strings.NewReader("long-passphrase\n"), &out, &out, env, func(context.Context, identity.BootstrapRequest) (string, error) { return "", errors.New(secret) })
		if code == 0 || strings.Contains(out.String(), secret) || strings.Contains(out.String(), "long-passphrase") {
			t.Fatal("secret leaked or failure accepted")
		}
	}
}

func TestCLIInputReadAndContextFailure(t *testing.T) {
	for _, input := range []string{"", strings.Repeat("x", 1025), "long-passphrase\r", "long-pass\nphrase", "long-pass\x00phrase"} {
		var out bytes.Buffer
		if run(context.Background(), []string{"--email", "a@example.com", "--tenant-name", "Store"}, strings.NewReader(input), &out, &out, func(k string) (string, bool) {
			if k == "DATABASE_URL" {
				return "dsn", true
			}
			return "", false
		}, func(context.Context, identity.BootstrapRequest) (string, error) {
			t.Fatal("invalid stdin reached consumer")
			return "", nil
		}) == 0 {
			t.Fatal("invalid input accepted")
		}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if run(ctx, []string{"--email", "a@example.com", "--tenant-name", "Store"}, reader, &out, &out, func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return "dsn", true
		}
		return "", false
	}, func(context.Context, identity.BootstrapRequest) (string, error) {
		t.Fatal("cancelled stdin reached consumer")
		return "", nil
	}) == 0 {
		t.Fatal("read cancellation ignored")
	}
}

func TestCLIEnvEmptyPasswordAndDSNFallback(t *testing.T) {
	for _, password := range []string{"", "long-passphrase"} {
		var out bytes.Buffer
		code := run(context.Background(), []string{"--email", "a@example.com", "--tenant-name", "Store"}, strings.NewReader("unused-stdin-passphrase"), &out, &out, func(k string) (string, bool) {
			switch k {
			case "OSC_BOOTSTRAP_PASSWORD":
				return password, true
			case "DATABASE_URL":
				return "fallback-dsn", true
			}
			return "", false
		}, func(ctx context.Context, input identity.BootstrapRequest) (string, error) {
			if input.Password != password || input.DatabaseURL != "fallback-dsn" {
				t.Fatal("fallback or explicit env password not respected")
			}
			return "owner-id", nil
		})
		if (code == 0) != (password != "") {
			t.Fatal("empty explicit env password accepted")
		}
	}
}

func TestCLIConsumerCancellationFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	code := run(ctx, []string{"--email", "a@example.com", "--tenant-name", "Store"}, strings.NewReader("long-passphrase"), &out, &out, func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return "dsn", true
		}
		return "", false
	}, func(context.Context, identity.BootstrapRequest) (string, error) { cancel(); return "owner-id", nil })
	if code == 0 || strings.Contains(out.String(), "complete") {
		t.Fatal("cancelled consumer reported success")
	}
}
