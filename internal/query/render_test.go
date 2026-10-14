package query

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestRender(t *testing.T) {
	big := strings.Repeat("ab", 100)
	for _, tc := range []struct {
		name string
		oid  uint32
		in   []byte
		want any
		cut  bool
	}{
		{"null", pgtype.TextOID, nil, nil, false},
		{"bool true", pgtype.BoolOID, []byte("t"), true, false},
		{"bool false", pgtype.BoolOID, []byte("f"), false, false},
		{"int4 as a number", pgtype.Int4OID, []byte("-42"), int64(-42), false},
		{"int8 stays exact text", pgtype.Int8OID, []byte("9007199254740993"), "9007199254740993", false},
		{"numeric stays exact text", pgtype.NumericOID, []byte("12345678901234567890.000000000001"), "12345678901234567890.000000000001", false},
		{"float as a number", pgtype.Float8OID, []byte("1.5"), 1.5, false},
		{"float NaN as text", pgtype.Float8OID, []byte("NaN"), "NaN", false},
		{"float Infinity as text", pgtype.Float4OID, []byte("Infinity"), "Infinity", false},
		{"timestamptz as text", pgtype.TimestamptzOID, []byte("2026-10-14 12:00:00+00"), "2026-10-14 12:00:00+00", false},
		{"jsonb as text", pgtype.JSONBOID, []byte(`{"a": 1}`), `{"a": 1}`, false},
		{"short bytea whole", pgtype.ByteaOID, []byte(`\xdeadbeef`), `\xdeadbeef`, false},
		{"long bytea cut", pgtype.ByteaOID, []byte(`\x` + big), `\x` + big[:128] + "… (100 bytes)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := render(tc.oid, tc.in)
			if got != tc.want || cut != tc.cut {
				t.Errorf("render = %#v (cut %v), want %#v (cut %v)", got, cut, tc.want, tc.cut)
			}
		})
	}

	long := strings.Repeat("é", MaxValueBytes) // 2 bytes each: the cut must land on a rune boundary
	got, cut := render(pgtype.TextOID, []byte(long))
	s := got.(string)
	if !cut || len(s) > MaxValueBytes+len("…") || !strings.HasSuffix(s, "…") || strings.ContainsRune(s, '�') {
		t.Errorf("long text: cut=%v len=%d", cut, len(s))
	}
}

func TestLimiter_OnePerUser(t *testing.T) {
	var l Limiter
	a, ok := l.acquire("alice")
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := l.acquire("alice"); ok {
		t.Fatal("a second concurrent query for the same user was allowed")
	}
	if _, ok := l.acquire("bob"); !ok {
		t.Fatal("another user was blocked")
	}
	a.release()
	if _, ok := l.acquire("alice"); !ok {
		t.Fatal("released slot not reusable")
	}
	if l.cancel("nobody", time.Millisecond) {
		t.Error("cancel reported a query for a user with none")
	}
}

// A cancel that lands before the query has registered its cancel func still cancels it.
func TestLimiter_CancelBeforeStart(t *testing.T) {
	var l Limiter
	r, _ := l.acquire("alice")
	go func() { l.cancel("alice", time.Second) }()
	deadline := time.Now().Add(time.Second)
	for !r.cancelled() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	called := false
	r.setCancel(func() { called = true })
	if !called {
		t.Error("an early cancel was lost")
	}
	r.release()
}
