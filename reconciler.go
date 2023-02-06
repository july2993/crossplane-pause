package crossplane

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"time"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// AnnotationKeyReconciliationPaused is the annotation key to make crossplane pause reconciling.
// ref https://github.com/crossplane/crossplane-runtime/issues/351
const AnnotationKeyReconciliationPaused = "crossplane.io/paused"

// AnnotationKeyPauseInfo is annotation key to store pause info.
const AnnotationKeyPauseInfo = "cloud.pingcap.com/pause-info"

// DefaultFrozenTimeDuration the default min Duration we will add the pause annotation again once we found the resource is updated.
// We CAN NOT guarantee the resource will be reconciled by crossplane before adding the pause annotation again.
const DefaultFrozenTimeDuration = 5 * time.Minute

// PauseInfo the json value of AnnotationKeyPauseInfo
type PauseInfo struct {
	Pause           bool                       `json:"pause"`
	Object          *unstructured.Unstructured `json:"object,omitempty"`
	LastPauseTime   *metav1.Time               `json:"lastPauseTime,omitempty"`
	LastUnPauseTime *metav1.Time               `json:"lastUnPauseTime,omitempty"`

	// The time we need to unpause to respect UnPausePollInterval.
	ShouldUnpauseTime *metav1.Time `json:"shouldUnpauseTime,omitempty"`
}

// Reconciler reconciles a crossplane resource to avoid polling
// by add pause annotation.
type Reconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	GroupVersionKind schema.GroupVersionKind
	// If sets UnPausePollInterval, every UnPausePollInterval, we will unpause the resource to let
	// crossplane to reconcile it when Ready and Sync condition are true.
	// We will add a jitter to avoid unpause too many resources at the same time.
	UnPausePollInterval *time.Duration
	// FrozenTimeDuration the min Duration we will add the pause annotation again once we found the resource is updated.
	// We CAN NOT guarantee the resource will be reconciled by crossplane before adding the pause annotation again.
	// If not set, default 5 minutes will be used.
	FrozenTimeDuration *time.Duration
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("Start reconcile")

	start := time.Now()
	defer func() {
		logger.Info("Finish reconcile", "take", time.Since(start))
	}()

	var obj = new(unstructured.Unstructured)
	obj.SetGroupVersionKind(r.GroupVersionKind)
	err = r.Client.Get(ctx, req.NamespacedName, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("unable to get object %s: %w", req.NamespacedName, err)
	}

	ann := obj.GetAnnotations()
	pauseValue, _ := ann[AnnotationKeyReconciliationPaused]

	info, err := parsePauseInfo(obj)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to parse pause info: %w", err)
	}

	// We add pause ann and info ann both.
	// in case the pause ann is added by other guy we just ignore this resource.
	if isPaused(pauseValue) && info == nil {
		logger.Info("ignore paused by other guy")
		return ctrl.Result{}, nil
	}

	// We never pause this resource yet, so missing the info annotation.
	if info == nil {
		info = &PauseInfo{
			Pause: false,
		}
	}

	// Never pause the deleted resource.
	if !obj.GetDeletionTimestamp().IsZero() {
		err := ensureUnPause(ctx, r.Client, obj, info, "resource deleted")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to unpause: %w", err)
		}
	}

	if info.Pause {
		updated, err := isUpdated(ctx, obj, info.Object)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to check if updated: %w", err)
		}

		if updated {
			err := ensureUnPause(ctx, r.Client, obj, info, "resource, updated")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("unable to unpause: %w", err)
			}
			// will trigger enqueue again since we update annotation in ensureUnPause().
			// and run into the info.Pause = false case
			return ctrl.Result{}, nil
		}

		if r.UnPausePollInterval != nil {
			now := time.Now()
			shouldUnpauseTime := info.LastPauseTime.Add(*r.UnPausePollInterval)
			if info.ShouldUnpauseTime != nil {
				shouldUnpauseTime = info.ShouldUnpauseTime.Time
			}

			if now.Before(shouldUnpauseTime) {
				logger.Info("requque after to check if should unpause by UnPausePollInterval", "after", shouldUnpauseTime.Sub(now).String())
				return ctrl.Result{RequeueAfter: shouldUnpauseTime.Sub(now)}, nil
			}

			err := ensureUnPause(ctx, r.Client, obj, info, "resource trigger unPause poll interval")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("unable to unpause: %w", err)
			}
			return ctrl.Result{}, nil
		}

		logger.Info("keep pause")
		return ctrl.Result{}, nil
	}

	// start to handle info.Pause == false case.
	now := time.Now()
	if info.LastUnPauseTime != nil && info.LastUnPauseTime.Add(*r.FrozenTimeDuration).After(now) {
		after := info.LastUnPauseTime.Add(*r.FrozenTimeDuration).Sub(now)
		logger.Info("keep unpause in frozen time duration", "checkAfter", after.String())
		return ctrl.Result{RequeueAfter: after}, nil
	}

	readyCondition, err := getCondition(obj, xpv1.TypeReady)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get ready condition: %w", err)
	}

	if readyCondition == nil || readyCondition.Status != corev1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	syncedCondition, err := getCondition(obj, xpv1.TypeSynced)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get synced condition: %w", err)
	}

	if syncedCondition == nil || syncedCondition.Status != corev1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	err = ensurePause(ctx, r.Client, obj, info, r.UnPausePollInterval, "Ready and Synced")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to pause: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, pds ...predicate.Predicate) error {
	if r.FrozenTimeDuration == nil {
		tmp := DefaultFrozenTimeDuration
		r.FrozenTimeDuration = &tmp
	}

	var u = &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GroupVersionKind)

	return ctrl.NewControllerManagedBy(mgr).
		For(u, builder.WithPredicates(pds...)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

func parsePauseInfo(obj *unstructured.Unstructured) (info *PauseInfo, err error) {
	ann := obj.GetAnnotations()

	v, ok := ann[AnnotationKeyPauseInfo]
	if !ok {
		return nil, nil
	}

	info = new(PauseInfo)
	err = json.Unmarshal([]byte(v), info)
	if err != nil {
		return nil, err
	}

	return
}

func ensurePause(ctx context.Context, cli client.Client, obj *unstructured.Unstructured, info *PauseInfo, unPausePollInterval *time.Duration, reason string) error {
	if info == nil {
		info = new(PauseInfo)
	}

	if info.Pause {
		return nil
	}

	info.Pause = true
	now := metav1.Now()
	info.LastPauseTime = &now
	info.Object = obj
	if unPausePollInterval != nil {
		shouldUnpauseTime := info.LastPauseTime.Add(*unPausePollInterval)
		// To avoid unpause too much resources at the same time when enable this feature.
		jitter := time.Duration(float64(rand.Int63n(int64(*unPausePollInterval))) * 0.1)
		shouldUnpauseTime.Add(jitter)
		info.ShouldUnpauseTime = &metav1.Time{Time: shouldUnpauseTime}
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", AnnotationKeyPauseInfo)

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("unable to marshal pause info: %w", err)
	}

	ann := obj.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[AnnotationKeyReconciliationPaused] = "true"
	ann[AnnotationKeyPauseInfo] = string(data)
	obj.SetAnnotations(ann)

	err = cli.Update(context.Background(), obj)
	if err != nil {
		return fmt.Errorf("failed to update object: %w", err)
	}

	log.FromContext(ctx).Info("pause resource", "reason", reason)
	return nil
}

func ensureUnPause(ctx context.Context, cli client.Client, obj *unstructured.Unstructured, info *PauseInfo, reason string) error {
	if info == nil {
		return nil
	}

	if !info.Pause {
		return nil
	}

	ann := obj.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}

	info.Pause = false
	info.Object = nil
	now := metav1.Now()
	info.LastUnPauseTime = &now
	info.ShouldUnpauseTime = nil

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("unable to marshal pause info: %w", err)
	}

	delete(ann, AnnotationKeyReconciliationPaused)
	ann[AnnotationKeyPauseInfo] = string(data)
	obj.SetAnnotations(ann)

	err = cli.Update(context.Background(), obj)
	if err != nil {
		return fmt.Errorf("failed to update object: %w", err)
	}

	log.FromContext(ctx).Info("unPause resource", "reason", reason)
	return nil
}

func isUpdated(ctx context.Context, old *unstructured.Unstructured, now *unstructured.Unstructured) (bool, error) {
	now = now.DeepCopy()
	old = old.DeepCopy()

	unstructured.RemoveNestedField(now.Object, "metadata", "annotations", AnnotationKeyReconciliationPaused)
	unstructured.RemoveNestedField(now.Object, "metadata", "annotations", AnnotationKeyPauseInfo)

	unstructured.RemoveNestedField(old.Object, "metadata", "annotations", AnnotationKeyReconciliationPaused)
	unstructured.RemoveNestedField(old.Object, "metadata", "annotations", AnnotationKeyPauseInfo)

	// check spec
	equal, err := checkFieldEqual(ctx, old, now, "spec")
	if err != nil {
		return false, err
	}

	if !equal {
		return true, nil
	}

	if !equal {
		return true, nil
	}

	// check annotations
	equal, err = checkFieldEqual(ctx, old, now, "metadata", "annotations")
	if err != nil {
		return false, err
	}

	if !equal {
		return true, nil
	}

	// check labels
	equal, err = checkFieldEqual(ctx, old, now, "metadata", "labels")
	if err != nil {
		return false, err
	}

	if !equal {
		return true, nil
	}

	return false, nil
}

func checkFieldEqual(ctx context.Context, obj1, obj2 *unstructured.Unstructured, fields ...string) (bool, error) {
	spec1, ok1, err := unstructured.NestedMap(obj1.Object, fields...)
	if err != nil {
		return false, err
	}

	spec2, ok2, err := unstructured.NestedMap(obj2.Object, fields...)
	if err != nil {
		return false, err
	}

	if ok1 != ok2 {
		return false, nil
	}

	// both not exist
	if !ok1 {
		return true, nil
	}

	if !reflect.DeepEqual(spec1, spec2) {
		diff := cmp.Diff(spec1, spec2)
		log.FromContext(ctx).Info("field not equal", "field", strings.Join(fields, "."), "diff", diff)
		return false, nil
	}

	return true, nil
}

func isPaused(v string) bool {
	return v == "true"
}

func getCondition(obj *unstructured.Unstructured, ty xpv1.ConditionType) (res *xpv1.Condition, err error) {
	/*
	   status:
	     conditions:
	     - lastTransitionTime: "2022-07-22T10:54:18Z"
	       reason: Available
	       status: "True"
	       type: Ready
	*/
	conditions, ok, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("unable to get conditions: %w", err)
	}

	if !ok {
		return nil, nil
	}

	for _, c := range conditions {
		data, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("unable to marshal condition: %w", err)
		}

		res = new(xpv1.Condition)
		err = json.Unmarshal(data, res)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal condition: %w", err)
		}

		if res.Type == ty {
			return res, nil
		}
	}

	return nil, nil
}
