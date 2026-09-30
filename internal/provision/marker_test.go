package provision

import (
	"testing"
	"time"
)

func TestReadyMarker(t *testing.T) {
	str := func(s string) *string { return &s }
	stamped := readyMarker + createdSep + "2026-09-30T12:00:00Z"

	// A database provisioned before creation times were recorded is still ready, with no time.
	for _, c := range []*string{str(readyMarker), str(stamped)} {
		if !isReady(c) {
			t.Errorf("isReady(%q) = false", *c)
		}
	}
	for _, c := range []*string{nil, str(""), str("something else"), str(readyMarker + "x"), str("prefix " + readyMarker)} {
		if isReady(c) {
			t.Errorf("isReady(%v) = true", c)
		}
	}
	if got := createdAtFromComment(str(stamped)); got == nil || !got.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("createdAt = %v", got)
	}
	for _, c := range []*string{nil, str(readyMarker), str(readyMarker + createdSep + "not a time")} {
		if createdAtFromComment(c) != nil {
			t.Errorf("createdAt(%v) should be unknown", c)
		}
	}
}
