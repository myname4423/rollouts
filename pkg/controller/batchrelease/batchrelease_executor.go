/*
Copyright 2022 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package batchrelease

import (
	"fmt"
	"reflect"
	"time"

	appsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	"github.com/openkruise/rollouts/api/v1beta1"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/bluegreenstyle"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/bluegreenstyle/deployment"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/canarystyle"
	canarydeployment "github.com/openkruise/rollouts/pkg/controller/batchrelease/control/canarystyle/deployment"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle/cloneset"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle/daemonset"
	partitiondeployment "github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle/deployment"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle/statefulset"
	"github.com/openkruise/rollouts/pkg/util"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	DefaultDuration = 2 * time.Second
)

// Executor is the controller that controls the release plan resource
type Executor struct {
	client   client.Client
	recorder record.EventRecorder
}

// NewReleasePlanExecutor creates a RolloutPlanController
func NewReleasePlanExecutor(cli client.Client, recorder record.EventRecorder) *Executor {
	return &Executor{
		client:   cli,
		recorder: recorder,
	}
}

// Do execute the release plan
func (r *Executor) Do(release *v1beta1.BatchRelease) (reconcile.Result, *v1beta1.BatchReleaseStatus, error) {
	klog.InfoS("Starting one round of reconciling release plan",
		"BatchRelease", client.ObjectKeyFromObject(release),
		"phase", release.Status.Phase,
		"current-batch", release.Status.CanaryStatus.CurrentBatch,
		"current-batch-state", release.Status.CanaryStatus.CurrentBatchState)

	newStatus := getInitializedStatus(&release.Status)
	// 传入的release和status都是指针，所以controller的修改是原地修改
	workloadController, err := r.getReleaseController(release, newStatus)
	if err != nil || workloadController == nil {
		return reconcile.Result{}, nil, nil
	}
	// 在此之前，还没有获取过workload；信息只有上一阶段传下来的Status，以及被更新的Spec
	// syncStatusBeforeExecuting利用controller获取workloadInfo，并同步
	stop, result, err := r.syncStatusBeforeExecuting(release, newStatus, workloadController)
	if stop || err != nil {
		return result, newStatus, err
	}

	return r.executeBatchReleasePlan(release, newStatus, workloadController)
}

// 大循环
func (r *Executor) executeBatchReleasePlan(release *v1beta1.BatchRelease, newStatus *v1beta1.BatchReleaseStatus, workloadController control.Interface) (reconcile.Result, *v1beta1.BatchReleaseStatus, error) {
	var err error
	result := reconcile.Result{}

	klog.V(3).Infof("BatchRelease(%v) State Machine into '%s' state", klog.KObj(release), newStatus.Phase)

	switch newStatus.Phase {
	default:
		// for compatibility. if it is an unknown phase, should start from beginning.
		newStatus.Phase = v1beta1.RolloutPhasePreparing
		fallthrough

	case v1beta1.RolloutPhasePreparing:
		// prepare and initialize something before progressing in this state.
		// 调用了底层controller的Initialize
		// 所以Initialize只会调用一次
		err = workloadController.Initialize()
		switch {
		case err == nil:
			newStatus.Phase = v1beta1.RolloutPhaseProgressing
			result = reconcile.Result{RequeueAfter: DefaultDuration}
		default:
			klog.Warningf("Failed to initialize %v, err %v", klog.KObj(release), err)
		}

	case v1beta1.RolloutPhaseProgressing:
		// progress the release plan in this state.
		result, err = r.progressBatches(release, newStatus, workloadController)

	case v1beta1.RolloutPhaseFinalizing:
		err = workloadController.Finalize()
		switch {
		case err == nil:
			newStatus.Phase = v1beta1.RolloutPhaseCompleted
		default:
			klog.Warningf("Failed to finalize %v, err %v", klog.KObj(release), err)
		}

	case v1beta1.RolloutPhaseCompleted:
		// this state indicates that the plan is executed/cancelled successfully, should do nothing in these states.
	}

	return result, newStatus, err
}

// batch循环
// reconcile logic when we are in the middle of release, we have to go through finalizing state before succeed or fail
func (r *Executor) progressBatches(release *v1beta1.BatchRelease, newStatus *v1beta1.BatchReleaseStatus, workloadController control.Interface) (reconcile.Result, error) {
	var err error
	result := reconcile.Result{}

	klog.V(3).Infof("BatchRelease(%v) Canary Batch State Machine into '%s' state", klog.KObj(release), newStatus.CanaryStatus.CurrentBatchState)

	switch newStatus.CanaryStatus.CurrentBatchState {
	default:
		// for compatibility. if it is an unknown state, should start from beginning.
		// 初始状态将是UpgradingBatchState
		newStatus.CanaryStatus.CurrentBatchState = v1beta1.UpgradingBatchState
		fallthrough
	// 任何异常都会回到该状态
	case v1beta1.UpgradingBatchState:
		// modify workload replicas/partition based on release plan in this state.
		// 进入下一层
		err = workloadController.UpgradeBatch()
		switch {
		case err == nil:
			result = reconcile.Result{RequeueAfter: DefaultDuration}
			newStatus.CanaryStatus.CurrentBatchState = v1beta1.VerifyingBatchState
		default:
			klog.Warningf("Failed to upgrade %v, err %v", klog.KObj(release), err)
		}

	case v1beta1.VerifyingBatchState:
		// replicas/partition has been modified, should wait pod ready in this state.
		err = workloadController.CheckBatchReady()
		switch {
		case err != nil:
			// should go to upgrade state to do again to avoid dead wait.
			newStatus.CanaryStatus.CurrentBatchState = v1beta1.UpgradingBatchState
			klog.Warningf("%v current batch is not ready, err %v", klog.KObj(release), err)
		default:
			now := metav1.Now()
			newStatus.CanaryStatus.BatchReadyTime = &now
			result = reconcile.Result{RequeueAfter: DefaultDuration}
			newStatus.CanaryStatus.CurrentBatchState = v1beta1.ReadyBatchState
		}
	// Ready就是一批发布完了，上一层会通过检查是否为该状态来决定是否发布完成
	case v1beta1.ReadyBatchState:
		// replicas/partition may be modified even though ready, should recheck in this state.
		err = workloadController.CheckBatchReady()
		switch {
		case err != nil:
			// if the batch ready condition changed due to some reasons, just recalculate the current batch.
			newStatus.CanaryStatus.BatchReadyTime = nil
			newStatus.CanaryStatus.CurrentBatchState = v1beta1.UpgradingBatchState
			klog.Warningf("%v current batch is not ready, err %v", klog.KObj(release), err)
		case !isPartitioned(release):
			//在ready状态，rollout修改了br的spec.BatchPartition，会导致进入下一次发布
			r.moveToNextBatch(release, newStatus)
			result = reconcile.Result{RequeueAfter: DefaultDuration}
		}
	}

	return result, err
}

// GetWorkloadController pick the right workload controller to work on the workload
func (r *Executor) getReleaseController(release *v1beta1.BatchRelease, newStatus *v1beta1.BatchReleaseStatus) (control.Interface, error) {
	targetRef := release.Spec.WorkloadRef
	gvk := schema.FromAPIVersionAndKind(targetRef.APIVersion, targetRef.Kind)
	if !util.IsSupportedWorkload(gvk) {
		message := fmt.Sprintf("the workload type '%v' is not supported", gvk)
		r.recorder.Event(release, v1.EventTypeWarning, "UnsupportedWorkload", message)
		return nil, fmt.Errorf(message)
	}

	targetKey := types.NamespacedName{
		Namespace: release.Namespace,
		Name:      targetRef.Name,
	}

	rollingStyle := release.Spec.ReleasePlan.RollingStyle
	switch rollingStyle {
	case v1beta1.BlueGreenRollingStyle:
		// if targetRef.APIVersion == appsv1alpha1.GroupVersion.String() && targetRef.Kind == reflect.TypeOf(appsv1alpha1.CloneSet{}).Name() {
		// 	klog.InfoS("Using CloneSet bluegreen-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
		// 	return partitionstyle.NewControlPlane(cloneset.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
		// }
		if targetRef.APIVersion == apps.SchemeGroupVersion.String() && targetRef.Kind == reflect.TypeOf(apps.Deployment{}).Name() {
			klog.InfoS("Using Deployment bluegreen-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
			return bluegreenstyle.NewControlPlane(deployment.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
		}

	case v1beta1.CanaryRollingStyle:
		if targetRef.APIVersion == apps.SchemeGroupVersion.String() && targetRef.Kind == reflect.TypeOf(apps.Deployment{}).Name() {
			klog.InfoS("Using Deployment canary-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
			return canarystyle.NewControlPlane(canarydeployment.NewController, r.client, r.recorder, release, newStatus, targetKey), nil
		}
	// 为什么要允许为空值：是因为懒得修改那么多测试用例。因为之前测试传入的ReleasePlan通常都没有设置EnableExtraWorkloadForCanary（即false，使用partition发布）
	// 在EnableExtraWorkloadForCanary被RollingStyle取代之后，同样的操作就会赋给RollingStyle空值
	//TODO - 在检查所有之后，删除空值
	case v1beta1.PartitionRollingStyle, "":
		if targetRef.APIVersion == appsv1alpha1.GroupVersion.String() && targetRef.Kind == reflect.TypeOf(appsv1alpha1.CloneSet{}).Name() {
			klog.InfoS("Using CloneSet partition-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
			return partitionstyle.NewControlPlane(cloneset.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
		}
		if targetRef.APIVersion == appsv1alpha1.GroupVersion.String() && targetRef.Kind == reflect.TypeOf(appsv1alpha1.DaemonSet{}).Name() {
			klog.InfoS("Using DaemonSet partition-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
			return partitionstyle.NewControlPlane(daemonset.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
		}
		if targetRef.APIVersion == apps.SchemeGroupVersion.String() && targetRef.Kind == reflect.TypeOf(apps.Deployment{}).Name() {
			klog.InfoS("Using Deployment partition-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
			return partitionstyle.NewControlPlane(partitiondeployment.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
		}
	}

	// try to use StatefulSet-like rollout controller by default
	klog.InfoS("Using StatefulSet-Like partition-style release controller for this batch release", "workload name", targetKey.Name, "namespace", targetKey.Namespace)
	return partitionstyle.NewControlPlane(statefulset.NewController, r.client, r.recorder, release, newStatus, targetKey, gvk), nil
}

func (r *Executor) moveToNextBatch(release *v1beta1.BatchRelease, status *v1beta1.BatchReleaseStatus) {
	currentBatch := int(status.CanaryStatus.CurrentBatch)
	if currentBatch >= len(release.Spec.ReleasePlan.Batches)-1 {
		klog.V(3).Infof("BatchRelease(%v) finished all batch, release current batch: %v", klog.KObj(release), status.CanaryStatus.CurrentBatch)
	}
	if release.Spec.ReleasePlan.BatchPartition == nil || *release.Spec.ReleasePlan.BatchPartition > status.CanaryStatus.CurrentBatch {
		status.CanaryStatus.CurrentBatch++
	}
	//开始进入下一次发布
	status.CanaryStatus.CurrentBatchState = v1beta1.UpgradingBatchState
	klog.V(3).Infof("BatchRelease(%v) finished one batch, release current batch: %v", klog.KObj(release), status.CanaryStatus.CurrentBatch)
}

func isPartitioned(release *v1beta1.BatchRelease) bool {
	return release.Spec.ReleasePlan.BatchPartition != nil &&
		// 这两个batch之间的关系：一个是被rollout赋值的spec，一个是记录当前在哪里；前者总是>=后者
		*release.Spec.ReleasePlan.BatchPartition <= release.Status.CanaryStatus.CurrentBatch
}
