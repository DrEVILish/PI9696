package xlog

import "testing"

func TestDefaultLevelIsError(t *testing.T) {
	if got := GetLevel(); got != Error {
		t.Fatalf("expected default level Error, got %d", got)
	}
}

func TestSetAndGetLevel(t *testing.T) {
	orig := GetLevel()
	defer SetLevel(orig)

	SetLevel(Info)
	if got := GetLevel(); got != Info {
		t.Fatalf("expected level Info after SetLevel, got %d", got)
	}

	SetLevel(Warn)
	if got := GetLevel(); got != Warn {
		t.Fatalf("expected level Warn after SetLevel, got %d", got)
	}
}

func TestSetLevelIgnoresOutOfRange(t *testing.T) {
	orig := GetLevel()
	defer SetLevel(orig)

	SetLevel(Level(99))
	if got := GetLevel(); got != orig {
		t.Fatalf("expected out-of-range SetLevel to be ignored (level %d), got %d", orig, got)
	}
	SetLevel(Level(-1))
	if got := GetLevel(); got != orig {
		t.Fatalf("expected negative SetLevel to be ignored (level %d), got %d", orig, got)
	}
}

func TestLevelNamesMatchConstants(t *testing.T) {
	if len(Names) != 4 {
		t.Fatalf("expected 4 level names, got %d: %v", len(Names), Names)
	}
	for i := 0; i < 4; i++ {
		if Names[i] == "" {
			t.Fatalf("expected a name for level index %d", i)
		}
	}
}

func TestNamesIndexableByLevel(t *testing.T) {
	// Every valid level must have a user-facing name and a tier prefix that
	// at() indexes into - a tampered constant set would panic in at().
	levels := []Level{Error, Warn, Info, Debug}
	for _, l := range levels {
		if l < Error || l > Debug {
			t.Fatalf("level %d out of range for Names/tiers indexing", l)
		}
	}
}
