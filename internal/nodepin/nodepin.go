// Package nodepin implements ADR 0090 (ADR 0083's fix, duplicated for this module's own bundled
// server): pin the bundled PostgreSQL StatefulSet's pod template to the node its data volume
// actually lives on, once that's known.
//
// On k3s's default `local-path` storage class a PersistentVolume is bound to whichever node first
// created it, and nothing in the chart keeps the pod on that node. With node_count=2 the platform's
// everyday default (ADR 0082), an unpinned reschedule — a rollout restart, a `helm upgrade` touching
// the pod spec, a node drain — can strand the pod on a node that can't mount its data. The node
// isn't known until the pod has been scheduled once, so this can't be a static chart value; it is
// patched in afterwards, against the live cluster.
//
// Deliberately a copy of booth-core's internal/dbprov.EnsureNodeAffinity and its
// pinBundledPostgresNode loop (same label, same idempotence contract, same backoff-then-recheck
// shape), per the ADR 0090 ruling to duplicate rather than wait on a shared mechanism. Written
// against client-go's typed clientset rather than controller-runtime, which this module otherwise
// has no use for. The RBAC it needs is exactly get on one pod and get/patch on one StatefulSet, in
// this release's namespace (charts/booth-database/templates/rbac.yaml).
package nodepin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// NodeHostnameLabel is the built-in label every distribution sets on its nodes (k3s included), the
// same one booth-core pins on — no step that labels Node objects is needed.
const NodeHostnameLabel = "kubernetes.io/hostname"

// FieldManager names this module's change to the StatefulSet, so it's attributable in
// managedFields (and, under Helm's server-side apply, not claimed by the chart).
const FieldManager = "booth-database-nodepin"

// Ensure pins the StatefulSet's pod template to its ordinal-0 pod's node. Returns pinned=true once
// the nodeSelector matches the pod's actual node (whether this call set it or it already did).
// pinned=false with a nil error means there's nothing to pin to yet — the pod doesn't exist, or
// hasn't been assigned a node — and the caller should retry. Idempotent, and safe to race: a patch
// that loses just gets recomputed on the next call.
func Ensure(ctx context.Context, c kubernetes.Interface, namespace, statefulSet string) (pinned bool, err error) {
	pod, err := c.CoreV1().Pods(namespace).Get(ctx, statefulSet+"-0", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading pod %s/%s-0: %w", namespace, statefulSet, err)
	}
	node := pod.Spec.NodeName
	if node == "" {
		return false, nil
	}

	sts, err := c.AppsV1().StatefulSets(namespace).Get(ctx, statefulSet, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading statefulset %s/%s: %w", namespace, statefulSet, err)
	}
	if sts.Spec.Template.Spec.NodeSelector[NodeHostnameLabel] == node {
		return true, nil
	}

	// A merge patch touching only this one key: any other nodeSelector entries (an operator's own
	// bundled.nodeSelector) are left exactly as they are.
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"nodeSelector": map[string]string{NodeHostnameLabel: node},
		}}},
	})
	if _, err := c.AppsV1().StatefulSets(namespace).Patch(ctx, statefulSet, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		return false, fmt.Errorf("pinning statefulset %s/%s to node %q: %w", namespace, statefulSet, node, err)
	}
	return true, nil
}

// Options tunes Run; zero values take booth-core's own constants.
type Options struct {
	MaxBackoff time.Duration // default 30s
	Steady     time.Duration // default 5m
}

// Run retries Ensure with exponential backoff until it succeeds (on a fresh install the pod may not
// exist or have a node for a while), then keeps re-checking on a slow steady interval for the life
// of the process — cheap self-healing if the pin is ever removed or goes stale (e.g. a volume
// manually migrated to another node). Mirrors booth-core's pinBundledPostgresNode exactly.
func Run(ctx context.Context, c kubernetes.Interface, namespace, statefulSet string, opts Options) {
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Steady <= 0 {
		opts.Steady = 5 * time.Minute
	}
	backoff := time.Second
	wasPinned := false
	for {
		pinned, err := Ensure(ctx, c, namespace, statefulSet)
		var wait time.Duration
		switch {
		case err != nil:
			log.Printf("pinning bundled PostgreSQL to its node failed (%v); retrying in %s", err, backoff)
			fallthrough
		case !pinned:
			wait = backoff
			if backoff *= 2; backoff > opts.MaxBackoff {
				backoff = opts.MaxBackoff
			}
		default:
			if !wasPinned {
				log.Print("bundled PostgreSQL pinned to its node (ADR 0090)")
				wasPinned = true
			}
			backoff = time.Second // a future transient failure retries quickly again
			wait = opts.Steady
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
