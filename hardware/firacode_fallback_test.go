package hardware

import "testing"

// The system-font fallback once had no size table, so every context switch
// loaded a 0pt face and the panel rendered nothing after the first switch.
func TestFallbackConfigHasContextSizes(t *testing.T) {
	for _, ctx := range []string{"statusbar", "header", "menu", "selected", "details", "alert", "recording", "idle", "unknown"} {
		if got := fontForContext(ctx).pt; got <= 0 {
			t.Errorf("size for %q = %v, want > 0", ctx, got)
		}
	}
}
