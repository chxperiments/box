package runtime

import (
	"os"
	"testing"

	"box/internal/sandbox"
)

// The token cache is what lets a run skip the double VM boot, so its
// invalidation is the part that must be right: a matching identity is trusted,
// any other identity (a changed runtime) is not, and a missing cache never
// reads as verified.
func TestIsolationCache(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())

	if isolationCurrent("A") {
		t.Fatal("empty cache must not read as verified")
	}

	storeToken("A", "6.12.0", "6.18.0")
	if !isolationCurrent("A") {
		t.Error("the stored identity should be current")
	}
	if isolationCurrent("B") {
		t.Error("a different runtime identity must not be trusted")
	}

	// A later verification of a new runtime replaces the old one.
	storeToken("B", "6.12.0", "6.18.0")
	if isolationCurrent("A") {
		t.Error("the superseded identity must no longer be current")
	}
	if !isolationCurrent("B") {
		t.Error("the newly stored identity should be current")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	storeToken("id-123", "guest-k", "base-k")
	got, ok := loadToken()
	if !ok {
		t.Fatal("token should load after store")
	}
	if got.Identity != "id-123" || got.Guest != "guest-k" || got.Baseline != "base-k" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.VerifiedAt == "" {
		t.Error("VerifiedAt should be stamped")
	}
}

// A corrupt cache file must degrade to "not verified" (forcing a fresh check),
// never crash or falsely clear a run.
func TestCorruptTokenIsNotCurrent(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	p, err := sandbox.VerifyCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadToken(); ok {
		t.Error("a corrupt token must not load")
	}
	if isolationCurrent("anything") {
		t.Error("a corrupt cache must not read as verified")
	}
}
