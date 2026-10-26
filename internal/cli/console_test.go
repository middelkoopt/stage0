package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeConsole replays text as a node's serial output.
func fakeConsole(t *testing.T, text string) func(string, int) (io.ReadCloser, error) {
	return func(device string, baud int) (io.ReadCloser, error) {
		if device != "/dev/serial/by-id/probe" || baud != 115200 {
			t.Errorf("opened %s at %d", device, baud)
		}
		return io.NopCloser(strings.NewReader(text)), nil
	}
}

func TestConsoleStreamsAndLogs(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runApp(t, &App{OpenConsole: fakeConsole(t, "U-Boot\r\nk53 login: ")}, "console", "k53")
	if err != nil {
		t.Fatal(err)
	}
	if out != "U-Boot\r\nk53 login: " {
		t.Errorf("stdout = %q", out)
	}
	log, err := os.ReadFile(filepath.Join("logs", "k53.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "=== stage0 console k53") || !strings.HasSuffix(string(log), "U-Boot\r\nk53 login: ") {
		t.Errorf("log = %q; want a session header, then the output", log)
	}
}

func TestConsoleLogAppends(t *testing.T) {
	t.Chdir(t.TempDir())
	app := func() *App { return &App{OpenConsole: fakeConsole(t, "boot\r\n")} }
	for range 2 {
		if _, err := runApp(t, app(), "console", "k53"); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.ReadFile(filepath.Join("logs", "k53.log"))
	if n := strings.Count(string(log), "=== stage0 console k53"); n != 2 {
		t.Errorf("log has %d sessions, want 2:\n%s", n, log)
	}
}

func TestConsoleUntil(t *testing.T) {
	log := filepath.Join(t.TempDir(), "k53.log")
	out, err := runApp(t, &App{OpenConsole: fakeConsole(t, "Rocky\r\nk53 login: ")},
		"console", "k53", "--until", `login: `, "--log", log, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Node, Device, Log, Match string
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON (console output must not be streamed with -o json): %v\n%s", err, out)
	}
	if r.Node != "k53" || r.Device != "/dev/serial/by-id/probe" || r.Log != log || r.Match != "login: " {
		t.Errorf("result = %+v", r)
	}
}

func TestConsoleUntilNoMatch(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := runApp(t, &App{OpenConsole: fakeConsole(t, "Kernel panic\r\n")}, "console", "k53", "--until", "login:")
	if err == nil {
		t.Error("want an error when the pattern never appears")
	}
}

func TestConsoleNodeWithoutConsole(t *testing.T) {
	_, err := runApp(t, &App{OpenConsole: fakeConsole(t, "")}, "console", "k41")
	if err == nil || !strings.Contains(err.Error(), "no console") {
		t.Errorf("err = %v", err)
	}
}

func TestConsoleBadPattern(t *testing.T) {
	_, err := runApp(t, &App{OpenConsole: fakeConsole(t, "")}, "console", "k53", "--until", "(")
	if err == nil {
		t.Error("want an error for an invalid --until pattern")
	}
}
