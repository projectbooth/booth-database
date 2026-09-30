package nodepin

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	ns  = "booth-database"
	sts = "db-booth-database-postgres"
)

func statefulSet(selector map[string]string) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sts}}
	s.Spec.Template.Spec.NodeSelector = selector
	return s
}

func pod(node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sts + "-0"}, Spec: corev1.PodSpec{NodeName: node}}
}

func selectorOf(t *testing.T, c *fake.Clientset) map[string]string {
	t.Helper()
	s, err := c.AppsV1().StatefulSets(ns).Get(context.Background(), sts, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Spec.Template.Spec.NodeSelector
}

func patches(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func TestEnsure_NothingToPinYet(t *testing.T) {
	for name, objs := range map[string][]runtime.Object{
		"no pod":         {statefulSet(nil)},
		"pod unassigned": {statefulSet(nil), pod("")},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientset(objs...)
			pinned, err := Ensure(context.Background(), c, ns, sts)
			if pinned || err != nil {
				t.Fatalf("pinned=%v err=%v, want false/nil (too early, not a failure)", pinned, err)
			}
			if patches(c) != 0 {
				t.Fatal("patched with nothing to pin to")
			}
		})
	}
}

func TestEnsure_PinsAndKeepsOtherSelectors(t *testing.T) {
	c := fake.NewClientset(statefulSet(map[string]string{"disk": "ssd"}), pod("agent-1"))
	pinned, err := Ensure(context.Background(), c, ns, sts)
	if !pinned || err != nil {
		t.Fatalf("pinned=%v err=%v", pinned, err)
	}
	got := selectorOf(t, c)
	if got[NodeHostnameLabel] != "agent-1" || got["disk"] != "ssd" {
		t.Fatalf("nodeSelector = %v, want hostname pinned and the operator's own entry kept", got)
	}
}

func TestEnsure_Idempotent(t *testing.T) {
	c := fake.NewClientset(statefulSet(map[string]string{NodeHostnameLabel: "agent-1"}), pod("agent-1"))
	pinned, err := Ensure(context.Background(), c, ns, sts)
	if !pinned || err != nil || patches(c) != 0 {
		t.Fatalf("pinned=%v err=%v patches=%d: an already-correct pin must not be re-patched", pinned, err, patches(c))
	}
}

// Self-healing: a stale pin (the volume moved) is corrected to wherever the pod actually runs.
func TestEnsure_RepinsStale(t *testing.T) {
	c := fake.NewClientset(statefulSet(map[string]string{NodeHostnameLabel: "old-node"}), pod("agent-2"))
	if pinned, err := Ensure(context.Background(), c, ns, sts); !pinned || err != nil {
		t.Fatalf("pinned=%v err=%v", pinned, err)
	}
	if selectorOf(t, c)[NodeHostnameLabel] != "agent-2" {
		t.Fatal("stale pin not corrected")
	}
}

func TestEnsure_PatchErrorSurfaces(t *testing.T) {
	c := fake.NewClientset(statefulSet(nil), pod("agent-1"))
	c.PrependReactor("patch", "statefulsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if pinned, err := Ensure(context.Background(), c, ns, sts); pinned || err == nil {
		t.Fatalf("pinned=%v err=%v, want the patch failure reported", pinned, err)
	}
}

func TestRun_PinsOnceThePodIsScheduled(t *testing.T) {
	c := fake.NewClientset(statefulSet(nil), pod(""))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go Run(ctx, c, ns, sts, Options{MaxBackoff: 50 * time.Millisecond, Steady: time.Hour})

	time.Sleep(200 * time.Millisecond)
	if patches(c) != 0 {
		t.Fatal("pinned before the pod had a node")
	}
	// The scheduler assigns the pod; the loop's backoff retry notices.
	if _, err := c.CoreV1().Pods(ns).Update(ctx, pod("agent-1"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for selectorOf(t, c)[NodeHostnameLabel] != "agent-1" {
		if ctx.Err() != nil {
			t.Fatal("never pinned after the pod was scheduled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
