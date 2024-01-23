package databasenodeset

import (
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ydbv1alpha1 "github.com/ydb-platform/ydb-kubernetes-operator/api/v1alpha1"
	. "github.com/ydb-platform/ydb-kubernetes-operator/internal/controllers/constants" //nolint:revive,stylecheck
	"github.com/ydb-platform/ydb-kubernetes-operator/internal/resources"
)

func (r *Reconciler) Sync(ctx context.Context, crDatabaseNodeSet *ydbv1alpha1.DatabaseNodeSet) (ctrl.Result, error) {
	var stop bool
	var result ctrl.Result
	var err error

	databaseNodeSet := resources.NewDatabaseNodeSet(crDatabaseNodeSet)
	stop, result, err = databaseNodeSet.SetStatusOnFirstReconcile()
	if stop {
		return result, err
	}

	stop, result = r.checkDatabaseNodeSetFrozen(&databaseNodeSet)
	if stop {
		return result, nil
	}

	stop, result, err = r.handlePauseResume(ctx, &databaseNodeSet)
	if stop {
		return result, err
	}

	stop, result, err = r.handleResourcesSync(ctx, &databaseNodeSet)
	if stop {
		return result, err
	}

	stop, result, err = r.waitForStatefulSetToScale(ctx, &databaseNodeSet)
	if stop {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) handleResourcesSync(
	ctx context.Context,
	databaseNodeSet *resources.DatabaseNodeSetResource,
) (bool, ctrl.Result, error) {
	r.Log.Info("running step handleResourcesSync")

	for _, builder := range databaseNodeSet.GetResourceBuilders(r.Config) {
		newResource := builder.Placeholder(databaseNodeSet)

		result, err := resources.CreateOrUpdateOrMaybeIgnore(ctx, r.Client, newResource, func() error {
			var err error

			err = builder.Build(newResource)
			if err != nil {
				r.Recorder.Event(
					databaseNodeSet,
					corev1.EventTypeWarning,
					"ProvisioningFailed",
					fmt.Sprintf("Failed building resources: %s", err),
				)
				return err
			}
			err = ctrl.SetControllerReference(databaseNodeSet.Unwrap(), newResource, r.Scheme)
			if err != nil {
				r.Recorder.Event(
					databaseNodeSet,
					corev1.EventTypeWarning,
					"ProvisioningFailed",
					fmt.Sprintf("Error setting controller reference for resource: %s", err),
				)
				return err
			}

			return nil
		}, shouldIgnoreDatabaseNodeSetChange(databaseNodeSet))

		eventMessage := fmt.Sprintf(
			"Resource: %s, Namespace: %s, Name: %s",
			reflect.TypeOf(newResource),
			newResource.GetNamespace(),
			newResource.GetName(),
		)
		if err != nil {
			r.Recorder.Event(
				databaseNodeSet,
				corev1.EventTypeWarning,
				"ProvisioningFailed",
				eventMessage+fmt.Sprintf(", failed to sync, error: %s", err),
			)
			return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, err
		} else if result == controllerutil.OperationResultCreated || result == controllerutil.OperationResultUpdated {
			r.Recorder.Event(
				databaseNodeSet,
				corev1.EventTypeNormal,
				string(DatabaseNodeSetProvisioning),
				eventMessage+fmt.Sprintf(", changed, result: %s", result),
			)
		}
	}
	r.Log.Info("resource sync complete")
	return Continue, ctrl.Result{Requeue: false}, nil
}

func (r *Reconciler) waitForStatefulSetToScale(
	ctx context.Context,
	databaseNodeSet *resources.DatabaseNodeSetResource,
) (bool, ctrl.Result, error) {
	r.Log.Info("running step waitForStatefulSetToScale for DatabaseNodeSet")

	if databaseNodeSet.Status.State == DatabaseNodeSetPending {
		msg := fmt.Sprintf("Starting to track number of running databaseNodeSet pods, expected: %d", databaseNodeSet.Spec.Nodes)
		r.Recorder.Event(
			databaseNodeSet,
			corev1.EventTypeNormal,
			string(DatabaseNodeSetProvisioning),
			msg,
		)
		databaseNodeSet.Status.State = DatabaseNodeSetProvisioning
		return r.setState(ctx, databaseNodeSet)
	}

	found := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      databaseNodeSet.Name,
		Namespace: databaseNodeSet.Namespace,
	}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			r.Recorder.Event(
				databaseNodeSet,
				corev1.EventTypeWarning,
				"Syncing",
				fmt.Sprintf("Failed to found StatefulSet: %s", err),
			)
			return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, nil
		}
		r.Recorder.Event(
			databaseNodeSet,
			corev1.EventTypeWarning,
			"Syncing",
			fmt.Sprintf("Failed to get StatefulSets: %s", err),
		)
		return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, err
	}

	matchingLabels := client.MatchingLabels{}
	for k, v := range databaseNodeSet.Labels {
		matchingLabels[k] = v
	}

	podList := &corev1.PodList{}
	opts := []client.ListOption{
		client.InNamespace(databaseNodeSet.Namespace),
		matchingLabels,
	}
	if err = r.List(ctx, podList, opts...); err != nil {
		r.Recorder.Event(
			databaseNodeSet,
			corev1.EventTypeWarning,
			"Syncing",
			fmt.Sprintf("Failed to list databaseNodeSet pods: %s", err),
		)
		return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, err
	}

	runningPods := 0
	for _, e := range podList.Items {
		if e.Status.Phase == "Running" {
			runningPods++
		}
	}

	if runningPods != int(databaseNodeSet.Spec.Nodes) {
		r.Recorder.Event(
			databaseNodeSet,
			corev1.EventTypeNormal,
			string(DatabaseNodeSetProvisioning),
			fmt.Sprintf("Waiting for number of running databaseNodeSet pods to match expected: %d != %d", runningPods, databaseNodeSet.Spec.Nodes))
		return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, nil
	}

	if databaseNodeSet.Spec.Pause {
		meta.SetStatusCondition(&databaseNodeSet.Status.Conditions, metav1.Condition{
			Type:    DatabasePausedCondition,
			Status:  "True",
			Reason:  ReasonCompleted,
			Message: "Scaled DatabaseNodeSet to 0 successfully",
		})
		databaseNodeSet.Status.State = DatabaseNodeSetPaused
	} else {
		meta.SetStatusCondition(&databaseNodeSet.Status.Conditions, metav1.Condition{
			Type:    DatabaseNodeSetReadyCondition,
			Status:  "True",
			Reason:  ReasonCompleted,
			Message: fmt.Sprintf("Scaled DatabaseNodeSet to %d successfully", databaseNodeSet.Spec.Nodes),
		})
		databaseNodeSet.Status.State = DatabaseNodeSetReady
	}

	return r.setState(ctx, databaseNodeSet)
}

func (r *Reconciler) setState(
	ctx context.Context,
	databaseNodeSet *resources.DatabaseNodeSetResource,
) (bool, ctrl.Result, error) {
	crdatabaseNodeSet := &ydbv1alpha1.DatabaseNodeSet{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: databaseNodeSet.Namespace,
		Name:      databaseNodeSet.Name,
	}, crdatabaseNodeSet)
	if err != nil {
		r.Recorder.Event(
			crdatabaseNodeSet,
			corev1.EventTypeWarning,
			"ControllerError",
			"Failed fetching CR before status update",
		)
		return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, err
	}

	oldStatus := crdatabaseNodeSet.Status.State
	crdatabaseNodeSet.Status.State = databaseNodeSet.Status.State
	crdatabaseNodeSet.Status.Conditions = databaseNodeSet.Status.Conditions

	err = r.Status().Update(ctx, crdatabaseNodeSet)
	if err != nil {
		r.Recorder.Event(
			crdatabaseNodeSet,
			corev1.EventTypeWarning,
			"ControllerError",
			fmt.Sprintf("Failed setting status: %s", err),
		)
		return Stop, ctrl.Result{RequeueAfter: DefaultRequeueDelay}, err
	} else if oldStatus != databaseNodeSet.Status.State {
		r.Recorder.Event(
			crdatabaseNodeSet,
			corev1.EventTypeNormal,
			"StatusChanged",
			fmt.Sprintf("DatabaseNodeSet moved from %s to %s", oldStatus, databaseNodeSet.Status.State),
		)
	}

	return Stop, ctrl.Result{RequeueAfter: StatusUpdateRequeueDelay}, nil
}

func shouldIgnoreDatabaseNodeSetChange(databaseNodeSet *resources.DatabaseNodeSetResource) resources.IgnoreChangesFunction {
	return func(oldObj, newObj runtime.Object) bool {
		if _, ok := newObj.(*appsv1.StatefulSet); ok {
			if databaseNodeSet.Spec.Pause && *oldObj.(*appsv1.StatefulSet).Spec.Replicas == 0 {
				return true
			}
		}
		return false
	}
}

func (r *Reconciler) handlePauseResume(
	ctx context.Context,
	databaseNodeSet *resources.DatabaseNodeSetResource,
) (bool, ctrl.Result, error) {
	r.Log.Info("running step handlePauseResume for Database")
	if databaseNodeSet.Status.State == DatabaseReady && databaseNodeSet.Spec.Pause {
		r.Log.Info("`pause: true` was noticed, moving DatabaseNodeSet to state `Paused`")
		meta.RemoveStatusCondition(&databaseNodeSet.Status.Conditions, DatabaseNodeSetReadyCondition)
		meta.SetStatusCondition(&databaseNodeSet.Status.Conditions, metav1.Condition{
			Type:    DatabasePausedCondition,
			Status:  "False",
			Reason:  ReasonInProgress,
			Message: "Transitioning DatabaseNodeSet to Paused state",
		})
		databaseNodeSet.Status.State = DatabaseNodeSetPaused
		return r.setState(ctx, databaseNodeSet)
	}

	if databaseNodeSet.Status.State == DatabaseNodeSetPaused && !databaseNodeSet.Spec.Pause {
		r.Log.Info("`pause: false` was noticed, moving DatabaseNodeSet to state `Ready`")
		meta.RemoveStatusCondition(&databaseNodeSet.Status.Conditions, DatabasePausedCondition)
		meta.SetStatusCondition(&databaseNodeSet.Status.Conditions, metav1.Condition{
			Type:    DatabaseNodeSetReadyCondition,
			Status:  "False",
			Reason:  ReasonInProgress,
			Message: "Recovering DatabaseNodeSet from Paused state",
		})
		databaseNodeSet.Status.State = DatabaseNodeSetReady
		return r.setState(ctx, databaseNodeSet)
	}

	return Continue, ctrl.Result{}, nil
}

func (r *Reconciler) checkDatabaseNodeSetFrozen(
	databaseNodeSet *resources.DatabaseNodeSetResource,
) (bool, ctrl.Result) {
	r.Log.Info("running step checkStorageFrozen for DatabaseNodeSet parent object")
	if !databaseNodeSet.Spec.OperatorSync {
		r.Log.Info("`operatorSync: false` is set, no further steps will be run")
		return Stop, ctrl.Result{}
	}

	return Continue, ctrl.Result{}
}
