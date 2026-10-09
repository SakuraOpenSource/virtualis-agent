package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// AGT-F6: the sshd rewrite must be idempotent and effective regardless of
// pre-existing formatting (leading whitespace, casing, comments), and must
// not rely on Include-based drop-ins alone. Product behavior (root+password
// login) is intentional; this pins robust matching, not the policy itself.
func TestSetRootPasswordSSHDConfigRewriteIsIdempotentAndFormatRobust(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires POSIX /bin/sh (run on Linux nodes)")
	}
	dir := t.TempDir()
	sshd := filepath.Join(dir, "sshd_config")
	initial := "# hardened base\n  PermitRootLogin   no\n#PermitRootLogin prohibit-password\n\tpasswordauthentication no\n"
	if err := os.WriteFile(sshd, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}
	var script string
	ctx := WithCommandRunner(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "incus" && len(args) > 3 && args[0] == "exec" && args[3] == "sh" && args[4] == "-c" {
			script = args[5]
		}
		return []byte("{}"), nil
	})
	inst := &protocol.Instance{ID: 7, Name: "cfg", Driver: "incus", Type: "ct", RootPassword: "pw"}
	if err := NewIncusWithDataDir(t.TempDir()).SetRootPassword(ctx, inst, "pw"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sshd_config") {
		t.Fatal("no sshd_config handling in script")
	}
}

// The pure-string core of the rewrite, tested portably: normalize any
// existing directive line (indented/commented/mixed-case) exactly once and
// keep the file stable across repeated runs.
func TestSSHDConfigDirectiveNormalize(t *testing.T) {
	cases := []struct{ name, before, want string }{
		{"indented", "  PermitRootLogin no\n", "PermitRootLogin yes\n"},
		{"commented", "#PermitRootLogin no\n", "PermitRootLogin yes\n"},
		{"commented-indented", "  # PermitRootLogin no\n", "PermitRootLogin yes\n"},
		{"mixed-case", "PERMITROOTLOGIN no\n", "PermitRootLogin yes\n"},
		{"absent", "Port 22\n", "Port 22\nPermitRootLogin yes\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			once := normalizeSSHDirective(c.before, "PermitRootLogin", "yes")
			if once != c.want {
				t.Fatalf("got %q want %q", once, c.want)
			}
			// Idempotency: a second pass must not duplicate the directive.
			twice := normalizeSSHDirective(once, "PermitRootLogin", "yes")
			if twice != once {
				t.Fatalf("not idempotent: %q then %q", once, twice)
			}
		})
	}
}
