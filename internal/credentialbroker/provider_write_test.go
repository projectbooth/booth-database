package credentialbroker

// access "tablewrite" (ADR 0117): the wire shape, refusals and the revoke request, against a
// fake issuer. The privileges themselves are proved against PostgreSQL in internal/provision.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-database/internal/provision"
)

type writeCall struct {
	ws, leaseID string
	scope       provision.WriteScope
	ttl         time.Duration
	audit       provision.WriteAudit
}

type revokeCall struct {
	ws, grant string
	audit     provision.WriteAudit
}

// The fakeIssuer's tablewrite side: records calls; writeErr is returned by both.
var (
	writeCalls  []writeCall
	revokeCalls []revokeCall
	writeErr    error
)

func (f *fakeIssuer) IssueWrite(_ context.Context, ws, leaseID string, scope provision.WriteScope, ttl time.Duration, audit provision.WriteAudit) (provision.Lease, []provision.GrantedTable, error) {
	writeCalls = append(writeCalls, writeCall{ws, leaseID, scope, ttl, audit})
	if writeErr != nil {
		return provision.Lease{}, nil, writeErr
	}
	var granted []provision.GrantedTable
	for _, t := range scope.Tables {
		granted = append(granted, provision.GrantedTable{Schema: t.Schema, Name: t.Name, Operations: t.Operations, Columns: t.Columns, Sequences: []string{}})
	}
	return provision.Lease{
		Database: "bdb_ws_0123456789abcdef01234567", Username: "bdb_lease_11111111222233334444555555555555",
		Password: testPassword, ExpiresAt: time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC),
	}, granted, nil
}

func (f *fakeIssuer) RevokeWrite(_ context.Context, ws, grant string, audit provision.WriteAudit) (string, error) {
	revokeCalls = append(revokeCalls, revokeCall{ws, grant, audit})
	if writeErr != nil {
		return "", writeErr
	}
	return "bdb_ws_0123456789abcdef01234567", nil
}

func resetWrite() { writeCalls, revokeCalls, writeErr = nil, nil, nil }

var financeTables = []any{map[string]any{
	"schema": "finance_input", "name": "transactions",
	"operations": []string{"insert", "update"}, "columns": []string{"amount", "entered_by"},
}}

// writeBody is a tablewrite request as booth-core's broker forwards it from booth-api's write
// sidecar (subject apiwrite:<id>, acting for app:<id>).
func writeBody(overrides map[string]any) []byte {
	o := map[string]any{
		"access":    "tablewrite",
		"scope":     map[string]any{"workspace": "acme", "write": map[string]any{"grant": "api:finance", "tables": financeTables}},
		"requester": map[string]any{"subject": "apiwrite:finance", "workspace": "acme", "role": "editor", "onBehalfOf": "app:budget"},
	}
	for k, v := range overrides {
		o[k] = v
	}
	return validBody(o)
}

func TestTableWrite_Issues(t *testing.T) {
	resetWrite()
	f := &fakeIssuer{}
	rec := do(t, newTestHandler(f, testCredential), testCredential, writeBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(f.calls) != 0 || len(writeCalls) != 1 {
		t.Fatalf("Issue calls %d, IssueWrite calls %d", len(f.calls), len(writeCalls))
	}
	c := writeCalls[0]
	if c.ws != "acme" || c.scope.Grant != "api:finance" || len(c.scope.Tables) != 1 || c.scope.Tables[0].Name != "transactions" ||
		c.ttl != DefaultMinTTL || c.audit != (provision.WriteAudit{Subject: "apiwrite:finance", OnBehalfOf: "app:budget"}) {
		t.Fatalf("IssueWrite called with %+v", c)
	}
	var resp struct {
		Scope struct {
			Workspace, Database, Access string
			Write                       struct {
				Grant   string
				Tables  []provision.GrantedTable
				Revoked bool
			}
		}
		Credential *struct{ Username, Password string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope.Access != "tablewrite" || resp.Scope.Write.Grant != "api:finance" || len(resp.Scope.Write.Tables) != 1 ||
		resp.Scope.Write.Revoked || resp.Credential == nil || resp.Credential.Password != testPassword {
		t.Fatalf("response: %s", rec.Body)
	}
}

func TestTableWrite_Refusals(t *testing.T) {
	grantOnly := func(tables any) map[string]any {
		return map[string]any{"write": map[string]any{"grant": "api:finance", "tables": tables}}
	}
	table := func(kv ...any) []any {
		m := map[string]any{"schema": "s", "name": "t", "operations": []string{"insert"}, "columns": []string{"c"}}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return []any{m}
	}
	for name, c := range map[string]struct {
		body []byte
		code int
	}{
		"readwrite with scope.write":     {writeBody(map[string]any{"access": "readwrite"}), 422},
		"read with scope.write":          {writeBody(map[string]any{"access": "read"}), 422},
		"tablewrite without scope.write": {writeBody(map[string]any{"scope": map[string]any{"workspace": "acme"}}), 422},
		"tablewrite with no scope":       {writeBody(map[string]any{"scope": nil}), 422},
		"tables missing":                 {writeBody(map[string]any{"scope": map[string]any{"write": map[string]any{"grant": "api:finance"}}}), 422},
		"tables empty without revoke":    {writeBody(map[string]any{"scope": grantOnly([]any{})}), 422},
		"unknown field in write":         {writeBody(map[string]any{"scope": map[string]any{"write": map[string]any{"grant": "g", "tables": financeTables, "schema": "x"}}}), 422},
		"unknown field in a table":       {writeBody(map[string]any{"scope": grantOnly(table("rows", 5))}), 422},
		"unknown operation":              {writeBody(map[string]any{"scope": grantOnly(table("operations", []string{"truncate"}))}), 422},
		"insert without columns":         {writeBody(map[string]any{"scope": grantOnly(table("columns", []string{}))}), 422},
		"empty grant":                    {writeBody(map[string]any{"scope": map[string]any{"write": map[string]any{"grant": "", "tables": financeTables}}}), 422},
		"unknown option":                 {writeBody(map[string]any{"options": map[string]any{"force": true}}), 422},
		"revoke with tables":             {writeBody(map[string]any{"options": map[string]any{"revoke": true}}), 422},
		"revoke on readwrite":            {validBody(map[string]any{"options": map[string]any{"revoke": true}}), 422},
		"scope workspace mismatch":       {writeBody(map[string]any{"scope": map[string]any{"workspace": "other", "write": map[string]any{"grant": "g", "tables": financeTables}}}), 422},
		"unknown access":                 {writeBody(map[string]any{"access": "write"}), 400},
	} {
		resetWrite()
		f := &fakeIssuer{}
		rec := do(t, newTestHandler(f, testCredential), testCredential, c.body)
		if rec.Code != c.code {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, c.code, rec.Body)
		}
		if c.code == 422 && !strings.Contains(rec.Body.String(), "scope_not_supported") {
			t.Errorf("%s: body %s", name, rec.Body)
		}
		if len(f.calls)+len(writeCalls)+len(revokeCalls) != 0 {
			t.Errorf("%s: a refused request reached the issuer", name)
		}
	}
}

// A table the database refuses (provision.ScopeError) is 422 with its message; any other
// failure is 502 and says nothing about the database.
func TestTableWrite_IssuerErrors(t *testing.T) {
	resetWrite()
	writeErr = scopeErr(`table "finance_input"."missing" does not exist in the workspace database`)
	rec := do(t, newTestHandler(&fakeIssuer{}, testCredential), testCredential, writeBody(nil))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `\"missing\" does not exist`) {
		t.Fatalf("scope error: %d %s", rec.Code, rec.Body)
	}
	resetWrite()
	writeErr = errors.New("connection refused to 10.0.0.5")
	rec = do(t, newTestHandler(&fakeIssuer{}, testCredential), testCredential, writeBody(nil))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("other error: %d %s", rec.Code, rec.Body)
	}
}

func scopeErr(msg string) error { return provision.NewScopeError(msg) }

func TestTableWrite_Revoke(t *testing.T) {
	resetWrite()
	rec := do(t, newTestHandler(&fakeIssuer{}, testCredential), testCredential, writeBody(map[string]any{
		"scope":   map[string]any{"write": map[string]any{"grant": "api:finance", "tables": []any{}}},
		"options": map[string]any{"revoke": true},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(revokeCalls) != 1 || len(writeCalls) != 0 || revokeCalls[0] != (revokeCall{"acme", "api:finance", provision.WriteAudit{Subject: "apiwrite:finance", OnBehalfOf: "app:budget"}}) {
		t.Fatalf("calls: revoke %+v write %+v", revokeCalls, writeCalls)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["credential"]) != "null" {
		t.Fatalf("credential = %s, want null", raw["credential"])
	}
	var scope struct {
		Access string
		Write  struct {
			Grant   string
			Tables  []any
			Revoked bool
		}
	}
	if err := json.Unmarshal(raw["scope"], &scope); err != nil || scope.Access != "tablewrite" || !scope.Write.Revoked ||
		scope.Write.Grant != "api:finance" || scope.Write.Tables == nil || len(scope.Write.Tables) != 0 {
		t.Fatalf("scope: %s %v", raw["scope"], err)
	}
	for _, k := range []string{"leaseId", "kind", "expiresAt"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("revoke response lacks %s", k)
		}
	}
}

// onBehalfOf is accepted on every access value (core sends it whenever it has one), logged, and
// changes nothing else; the password is never logged.
func TestTableWrite_OnBehalfOfIsAuditOnly(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	resetWrite()
	f := &fakeIssuer{}
	h := newTestHandler(f, testCredential)
	read := validBody(map[string]any{"access": "read", "requester": map[string]any{"subject": "u", "workspace": "acme", "role": "viewer", "onBehalfOf": "app:budget"}})
	if rec := do(t, h, testCredential, read); rec.Code != http.StatusCreated || len(f.calls) != 1 || f.calls[0].readWrite {
		t.Fatalf("read with onBehalfOf: %d %+v", rec.Code, f.calls)
	}
	if rec := do(t, h, testCredential, writeBody(nil)); rec.Code != http.StatusCreated {
		t.Fatalf("tablewrite: %d", rec.Code)
	}
	out := buf.String()
	if strings.Count(out, "onBehalfOf=app:budget") != 2 || !strings.Contains(out, `grant="api:finance"`) || strings.Contains(out, testPassword) {
		t.Fatalf("audit lines:\n%s", out)
	}
}
