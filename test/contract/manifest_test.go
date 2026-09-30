// Package contract validates booth-database's own manifest — the BoothModule custom resource its
// Helm chart templates — against contracts/module-manifest.md and contracts/credential-broker.md,
// plus the chart properties this module's security story depends on. Per
// contracts/testing-strategy.md this runs against rendered templates (`helm template`), not a
// cluster. Skips without helm unless BOOTH_TEST_REQUIRE_HELM is set (CI sets it).
package contract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type boothModule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ID                  string         `yaml:"id"`
		DisplayName         string         `yaml:"displayName"`
		Version             string         `yaml:"version"`
		ContractVersion     string         `yaml:"contractVersion"`
		HasOwnUI            bool           `yaml:"hasOwnUi"`
		UIIntegrationMode   string         `yaml:"uiIntegrationMode"`
		HealthCheckPath     string         `yaml:"healthCheckPath"`
		NavPath             string         `yaml:"navPath"`
		Database            map[string]any `yaml:"database"`
		WorkloadIdentity    map[string]any `yaml:"workloadIdentity"`
		Events              map[string]any `yaml:"events"`
		ProvidesCredentials *struct {
			Kinds []string `yaml:"kinds"`
		} `yaml:"providesCredentials"`
		ServiceRef struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"serviceRef"`
	} `yaml:"spec"`
}

var externalValues = []string{
	"--set", "mode=external",
	"--set", "external.host=pg.example.internal",
	"--set", "external.username=booth_admin",
	"--set", "external.passwordSecret.name=pg-admin",
}

func helmTemplate(t *testing.T, extra ...string) []byte {
	t.Helper()
	out, err := runHelm(extra...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

func runHelm(extra ...string) ([]byte, error) {
	args := append([]string{"template", "db", filepath.Join("..", "..", "charts", "booth-database"), "--namespace", "booth-database"}, extra...)
	return exec.Command("helm", args...).CombinedOutput()
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("BOOTH_TEST_REQUIRE_HELM") != "" {
			t.Fatal("helm not installed but BOOTH_TEST_REQUIRE_HELM is set")
		}
		t.Skip("helm not installed; CI runs this")
	}
}

// docs splits a multi-document render into generic objects.
func docs(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("parsing rendered chart: %v", err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func kinds(t *testing.T, rendered []byte) map[string]int {
	k := map[string]int{}
	for _, d := range docs(t, rendered) {
		k[d["kind"].(string)]++
	}
	return k
}

func renderBoothModule(t *testing.T, extra ...string) boothModule {
	t.Helper()
	out := helmTemplate(t, append(extra, "--show-only", "templates/boothmodule.yaml")...)
	var m boothModule
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsing BoothModule: %v\n%s", err, out)
	}
	return m
}

func TestManifest_RequiredFields(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if m.APIVersion != "booth.projectbooth.io/v1alpha1" || m.Kind != "BoothModule" {
		t.Errorf("apiVersion/kind = %s/%s (ADR 0019)", m.APIVersion, m.Kind)
	}
	if m.Spec.ID != "database" { // "the repo name minus booth-"
		t.Errorf("spec.id = %q, want database", m.Spec.ID)
	}
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+`)
	if m.Spec.DisplayName == "" || !semver.MatchString(m.Spec.Version) || !semver.MatchString(m.Spec.ContractVersion) {
		t.Errorf("displayName/version/contractVersion = %q/%q/%q", m.Spec.DisplayName, m.Spec.Version, m.Spec.ContractVersion)
	}
	if m.Spec.HealthCheckPath != "/healthz" {
		t.Errorf("healthCheckPath = %q, want /healthz (the route the server actually serves)", m.Spec.HealthCheckPath)
	}
	if m.Spec.ServiceRef.Name == "" || m.Spec.ServiceRef.Port != 8080 {
		t.Errorf("serviceRef = %+v", m.Spec.ServiceRef)
	}
	// hasOwnUi: false means the UI-conditional fields must be absent, not half-filled.
	if m.Spec.HasOwnUI || m.Spec.UIIntegrationMode != "" || m.Spec.NavPath != "" {
		t.Errorf("v0 ships no UI, but manifest says hasOwnUi=%v mode=%q navPath=%q", m.Spec.HasOwnUI, m.Spec.UIIntegrationMode, m.Spec.NavPath)
	}
}

func TestManifest_CredentialProvider(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if m.Spec.ProvidesCredentials == nil || strings.Join(m.Spec.ProvidesCredentials.Kinds, ",") != "postgres" {
		t.Fatalf("providesCredentials = %+v, want {kinds: [postgres]} (ADR 0080/0088)", m.Spec.ProvidesCredentials)
	}
	if off := renderBoothModule(t, "--set", "credentialBroker.enabled=false"); off.Spec.ProvidesCredentials != nil {
		t.Error("credentialBroker.enabled=false must drop providesCredentials")
	}
}

// ADR 0081 / the brief: this is not an ADR 0053 module-internal database, and must never ask
// core to provision one on core's own Postgres. It also needs no event bus or workload minting.
func TestManifest_DoesNotAskCoreForAModuleDatabase(t *testing.T) {
	requireHelm(t)
	for _, extra := range [][]string{nil, externalValues} {
		m := renderBoothModule(t, extra...)
		if m.Spec.Database != nil {
			t.Error("manifest declares `database` — that's booth-core's module-internal Postgres (ADR 0053), not this module's")
		}
		if m.Spec.WorkloadIdentity != nil || m.Spec.Events != nil {
			t.Error("manifest declares capabilities this module doesn't use")
		}
	}
}

func TestChart_BundledMode(t *testing.T) {
	requireHelm(t)
	out := helmTemplate(t)
	k := kinds(t, out)
	for kind, want := range map[string]int{"StatefulSet": 1, "CronJob": 1, "Deployment": 1, "BoothModule": 1, "Secret": 1} {
		if k[kind] != want {
			t.Errorf("bundled mode renders %d %s, want %d", k[kind], kind, want)
		}
	}
	s := string(out)
	// The admin password must survive upgrades and uninstall (the data volume does).
	for _, d := range docs(t, out) {
		if d["kind"] != "Secret" {
			continue
		}
		ann, _ := d["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann["helm.sh/resource-policy"] != "keep" {
			t.Error("bundled admin Secret isn't kept across uninstall")
		}
	}
	// Lease passwords are SCRAM verifiers; the server must require SCRAM for network logins.
	if !strings.Contains(s, "POSTGRES_HOST_AUTH_METHOD") || !strings.Contains(s, "scram-sha-256") {
		t.Error("bundled server doesn't require scram-sha-256")
	}
	// Backups must never capture role passwords (lease verifiers).
	if !strings.Contains(s, "--no-role-passwords") {
		t.Error("backup script dumps role passwords")
	}
	// It is this module's own server, never booth-core's.
	if strings.Contains(s, "booth-database-credentials") {
		t.Error("chart references booth-core's module-database Secret (ADR 0053)")
	}
}

func TestChart_ExternalMode(t *testing.T) {
	requireHelm(t)
	out := helmTemplate(t, externalValues...)
	k := kinds(t, out)
	if k["StatefulSet"] != 0 || k["CronJob"] != 0 {
		t.Errorf("external mode must not run a server or back one up: %v", k)
	}
	s := string(out)
	for _, want := range []string{"pg.example.internal", "name: pg-admin", "BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS"} {
		if !strings.Contains(s, want) {
			t.Errorf("external render lacks %q", want)
		}
	}
	for _, missing := range [][]string{
		{"--set", "mode=external"},
		{"--set", "mode=external", "--set", "external.host=h", "--set", "external.username=u"},
		{"--set", "mode=sideways"},
	} {
		if out, err := runHelm(missing...); err == nil {
			t.Errorf("rendered with %v, want a clear failure:\n%.200s", missing, out)
		}
	}
}

func podSpec(t *testing.T, rendered []byte, kind string) map[string]any {
	t.Helper()
	for _, d := range docs(t, rendered) {
		if d["kind"] == kind {
			return d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
	}
	t.Fatalf("no %s rendered", kind)
	return nil
}

// ADR 0090: the module pod carries a Kubernetes token only when node pinning needs one, and the
// Role behind it reaches exactly one pod and one StatefulSet in its own namespace.
func TestChart_NodePinRBACIsMinimal(t *testing.T) {
	requireHelm(t)
	out := helmTemplate(t)
	if podSpec(t, out, "Deployment")["automountServiceAccountToken"] != true {
		t.Fatal("pinning enabled but the module pod gets no token")
	}
	if !strings.Contains(string(out), "BOOTH_DATABASE_PIN_STATEFULSET") {
		t.Error("pinning enabled but the module isn't told which StatefulSet to pin")
	}
	var roles int
	for _, d := range docs(t, out) {
		switch d["kind"] {
		case "ClusterRole", "ClusterRoleBinding":
			t.Errorf("chart renders a %s; pinning must stay namespace-scoped", d["kind"])
		case "Role":
			roles++
			for _, r := range d["rules"].([]any) {
				rule := r.(map[string]any)
				names, _ := rule["resourceNames"].([]any)
				if len(names) != 1 {
					t.Errorf("rule %v is not pinned to exactly one named object", rule)
				}
				res := rule["resources"].([]any)[0]
				verbs := fmt.Sprint(rule["verbs"])
				switch res {
				case "statefulsets":
					if names[0] != "db-booth-database-postgres" || verbs != "[get patch]" {
						t.Errorf("statefulset rule = %v", rule)
					}
				case "pods":
					if names[0] != "db-booth-database-postgres-0" || verbs != "[get]" {
						t.Errorf("pod rule = %v", rule)
					}
				default:
					t.Errorf("unexpected resource %v in Role", res)
				}
			}
		}
	}
	if roles != 1 {
		t.Errorf("rendered %d Roles, want 1", roles)
	}

	for _, extra := range [][]string{{"--set", "bundled.pinToNode=false"}, externalValues} {
		out := helmTemplate(t, extra...)
		if podSpec(t, out, "Deployment")["automountServiceAccountToken"] != false {
			t.Errorf("%v: no pinning, but the module pod still mounts a token", extra)
		}
		if k := kinds(t, out); k["Role"] != 0 || k["RoleBinding"] != 0 {
			t.Errorf("%v: RBAC rendered without pinning", extra)
		}
	}
}

// Core's pattern (its networkpolicy-postgresql.yaml): the bundled server admits only this
// module's pod, its backup Job, and namespaces labelled database-client.
func TestChart_NetworkPolicy(t *testing.T) {
	requireHelm(t)
	var np map[string]any
	for _, d := range docs(t, helmTemplate(t)) {
		if d["kind"] == "NetworkPolicy" {
			np = d
		}
	}
	if np == nil {
		t.Fatal("bundled mode ships no NetworkPolicy for its Postgres")
	}
	spec := np["spec"].(map[string]any)
	if sel := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any); sel["app.kubernetes.io/component"] != "postgres" {
		t.Errorf("policy selects %v, want the postgres pods", sel)
	}
	ingress := spec["ingress"].([]any)
	if len(ingress) != 1 {
		t.Fatalf("want one ingress rule, got %v", ingress)
	}
	rule := ingress[0].(map[string]any)
	if fmt.Sprint(rule["ports"]) != "[map[port:5432 protocol:TCP]]" {
		t.Errorf("ports = %v", rule["ports"])
	}
	var got []string
	for _, f := range rule["from"].([]any) {
		peer := f.(map[string]any)
		if ps, ok := peer["podSelector"]; ok {
			got = append(got, "pod:"+fmt.Sprint(ps.(map[string]any)["matchLabels"].(map[string]any)["app.kubernetes.io/component"]))
		}
		if ns, ok := peer["namespaceSelector"]; ok {
			got = append(got, "ns:"+fmt.Sprint(ns.(map[string]any)["matchLabels"]))
		}
	}
	if want := "[pod:api pod:backup ns:map[booth.projectbooth.io/database-client:true]]"; fmt.Sprint(got) != want {
		t.Errorf("peers = %v, want %s", got, want)
	}
	if k := kinds(t, helmTemplate(t, externalValues...)); k["NetworkPolicy"] != 0 {
		t.Error("external mode renders a NetworkPolicy for a server it doesn't run")
	}
}
