package cli

import (
	"strings"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/media-and-entertainment/game-goat/internal/cliutil"
)

// TestResolveITADKeyPrecedence proves the stored credential is used when no env
// override is set, and that an explicit env var still wins.
func TestResolveITADKeyPrecedence(t *testing.T) {
	restore, err := cliutil.SetHomeOverride(t.TempDir())
	if err != nil {
		t.Fatalf("SetHomeOverride: %v", err)
	}
	defer restore()
	t.Setenv("ITAD_API_KEY", "")

	// No key anywhere -> empty, no error.
	key, err := resolveITADKey(&rootFlags{})
	if err != nil || key != "" {
		t.Fatalf("resolveITADKey with no key = (%q, %v), want empty", key, err)
	}

	// Stored credential is used.
	if err := cliutil.SaveITADCredential("stored-itad"); err != nil {
		t.Fatalf("SaveITADCredential: %v", err)
	}
	key, err = resolveITADKey(&rootFlags{})
	if err != nil || key != "stored-itad" {
		t.Fatalf("resolveITADKey stored = (%q, %v), want stored-itad", key, err)
	}

	// Env override wins over the stored credential.
	t.Setenv("ITAD_API_KEY", "env-itad")
	key, err = resolveITADKey(&rootFlags{})
	if err != nil || key != "env-itad" {
		t.Fatalf("resolveITADKey env = (%q, %v), want env-itad", key, err)
	}
}

// TestNewITADClientUsesStoredCredential proves prices/price-history authenticate
// from the stored credential with no ITAD_API_KEY in the environment.
func TestNewITADClientUsesStoredCredential(t *testing.T) {
	restore, err := cliutil.SetHomeOverride(t.TempDir())
	if err != nil {
		t.Fatalf("SetHomeOverride: %v", err)
	}
	defer restore()
	t.Setenv("ITAD_API_KEY", "")

	if err := cliutil.SaveITADCredential("stored-itad"); err != nil {
		t.Fatalf("SaveITADCredential: %v", err)
	}
	if _, err := newITADClient(&rootFlags{}, "US"); err != nil {
		t.Fatalf("newITADClient with stored credential: %v", err)
	}
}

// TestNewITADClientMissingKeyGuidance points the user at both the env var and
// the one-time auth command when no key is configured.
func TestNewITADClientMissingKeyGuidance(t *testing.T) {
	restore, err := cliutil.SetHomeOverride(t.TempDir())
	if err != nil {
		t.Fatalf("SetHomeOverride: %v", err)
	}
	defer restore()
	t.Setenv("ITAD_API_KEY", "")

	_, err = newITADClient(&rootFlags{}, "US")
	if err == nil {
		t.Fatal("expected an auth error with no ITAD key configured")
	}
	if !strings.Contains(err.Error(), "auth set-token --provider itad") {
		t.Fatalf("error should name the stored-credential path, got: %v", err)
	}
}
