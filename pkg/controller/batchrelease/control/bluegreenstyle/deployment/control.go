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

package deployment

import (
	"context"
	"fmt"

	"github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/api/v1beta1"
	batchcontext "github.com/openkruise/rollouts/pkg/controller/batchrelease/context"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/bluegreenstyle"
	deploymentutil "github.com/openkruise/rollouts/pkg/controller/deployment/util"
	"github.com/openkruise/rollouts/pkg/util"
	"github.com/openkruise/rollouts/pkg/util/patch"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type realController struct {
	*util.WorkloadInfo
	client client.Client
	pods   []*corev1.Pod
	key    types.NamespacedName
	object *apps.Deployment
}

func NewController(cli client.Client, key types.NamespacedName, _ schema.GroupVersionKind) bluegreenstyle.Interface {
	return &realController{
		key:    key,
		client: cli,
	}
}

func (rc *realController) GetWorkloadInfo() *util.WorkloadInfo {
	return rc.WorkloadInfo
}

func (rc *realController) BuildController() (bluegreenstyle.Interface, error) {
	if rc.object != nil {
		return rc, nil
	}
	object := &apps.Deployment{}
	if err := rc.client.Get(context.TODO(), rc.key, object); err != nil {
		return rc, err
	}
	rc.object = object
	//NOTE - 在调用getWorkloadInfo之前，需要等待以确保stable rs处于available状态，否则
	// 连续发布场景下可能会有一些问题（连续发布场景下，如果v1也是unavailable那么会缩容v1，否则缩容v2）
	// 确定性行为：应该只缩容v2，所以保证v1是available状态
	// 此时应该返回一个error，这个error会最终返回给Reconcile函数，然后触发retry机制
	// 也可以直接patch minReadys为0，等待observedGeneration和generation一致
	// “这种情况可能发生在，当前workload还没有准备好，用户就进行新版本发布了”
	// 实际上不会出现这种情况，因为修改minReadys后，v1的rs的minReady还没改变，所以还是会正常available
	rc.WorkloadInfo = rc.getWorkloadInfo(object)
	return rc, nil
}

func (rc *realController) ListOwnedPods() ([]*corev1.Pod, error) {
	if rc.pods != nil {
		return rc.pods, nil
	}
	var err error
	rc.pods, err = util.ListOwnedPods(rc.client, rc.object)
	return rc.pods, err
}

// 插入标签；插入v1alpha1.DeploymentStrategyAnnotation注释；
// 暂停；更改为Recreate；现在由advanced deployment controller控制
func (rc *realController) Initialize(release *v1beta1.BatchRelease) error {
	if control.IsControlledByBatchRelease(release, rc.object) {
		return nil
	}

	// Set strategy to deployment annotations
	// strategy := util.GetDeploymentStrategy(rc.object)
	// rollingUpdate := strategy.RollingUpdate
	// // 不是空的，代表是第一次发布，否则一定是Recreate
	// if rc.object.Spec.Strategy.RollingUpdate != nil {
	// 	rollingUpdate = rc.object.Spec.Strategy.RollingUpdate
	// }
	// strategy = v1alpha1.DeploymentStrategy{
	// 	Paused:        false,
	// 	Partition:     intstr.FromInt(0),
	// 	RollingStyle:  v1alpha1.PartitionRollingStyle,
	// 	RollingUpdate: rollingUpdate,
	// }
	// v1alpha1.SetDefaultDeploymentStrategy(&strategy)

	setting := util.GetOriginalSetting(rc.object)
	// 如果是nil代表一定是没有注释，因为只要有注释就不可能是空的，至少赋了默认值
	if setting.Strategy == nil {
		setting.Strategy = rc.object.Spec.Strategy.DeepCopy()
		setting.MinReadySeconds = rc.object.Spec.MinReadySeconds
		setting.ProgressDeadlineSeconds = rc.object.Spec.ProgressDeadlineSeconds
	}
	v1beta1.SetDefaultSetting(&setting)

	d := rc.object.DeepCopy()
	patchData := patch.NewDeploymentPatch()
	// patchData.InsertLabel(v1alpha1.AdvancedDeploymentControlLabel, "true")
	patchData.InsertAnnotation(v1beta1.OriginalSettingAnnotation, util.DumpJSON(&setting))
	klog.Infof("--zyb says: inserted annotaion %s: %s", v1beta1.OriginalSettingAnnotation, util.DumpJSON(&setting))
	patchData.InsertAnnotation(util.BatchReleaseControlAnnotation, util.DumpJSON(metav1.NewControllerRef(
		release, release.GetObjectKind().GroupVersionKind())))
	klog.Infof("--zyb says: inserted annotaion %s", util.BatchReleaseControlAnnotation)
	patchData.InsertAnnotation(util.BatchReleaseControlAnnotation, util.DumpJSON(metav1.NewControllerRef(
		release, release.GetObjectKind().GroupVersionKind())))
	// Pause the Deployment, update: MinReadySeconds, ProgressDeadlineSeconds, Strategy
	patchData.UpdatePaused(true)
	maxSurge := intstr.FromString("100%") //初始值可以随便，不过不能是0%
	maxUnavailable := intstr.FromString("0%")
	patchData.UpdateStrategy(apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	})
	patchData.UpdateMinReadySeconds(v1beta1.MaxReadySeconds)
	patchData.UpdateProgressDeadlineSeconds(v1beta1.MaxProgressSeconds)
	klog.Infof("--zyb says: paused: true; maxSurge:100%%; maxUnavaliable:0%%; MinReady and Progess")
	return rc.client.Patch(context.TODO(), d, patchData)
}

// func (rc *realController) UpgradeBatch(ctx *batchcontext.BatchContext) error {
// 	if !deploymentutil.IsUnderRolloutControl(rc.object) {
// 		klog.Warningf("Cannot upgrade batch, because "+
// 			"deployment %v has ridden out of our control", klog.KObj(rc.object))
// 		return nil
// 	}

// 	strategy := util.GetDeploymentStrategy(rc.object)
// 	if control.IsCurrentMoreThanOrEqualToDesired(strategy.Partition, ctx.DesiredPartition) {
// 		return nil // Satisfied, no need patch again.
// 	}

// 	d := rc.object.DeepCopy()
// 	strategy.Partition = ctx.DesiredPartition
// 	patchData := patch.NewDeploymentPatch()
// 	patchData.InsertAnnotation(v1alpha1.DeploymentStrategyAnnotation, util.DumpJSON(&strategy))
// 	return rc.client.Patch(context.TODO(), d, patchData)
// }

func (rc *realController) UpgradeBatch(ctx *batchcontext.BatchContext) error {
	klog.Info("--zyb says: in UpgradeBatch function")
	if !deploymentutil.IsOKForBlueGreen(rc.object) {
		klog.Warningf("Cannot upgrade batch, because "+
			"deployment %v has ridden out of our control", klog.KObj(rc.object))
		return nil
	}
	var body string
	var desired int
	switch partition := ctx.DesiredPartition; partition.Type {
	case intstr.Int:
		desired = int(partition.IntVal)
		body = fmt.Sprintf(`{"spec":{"strategy":{"rollingUpdate":{"maxSurge": %d }},"paused":%v}}`, partition.IntValue(), false)
	case intstr.String:
		desired, _ = intstr.GetScaledValueFromIntOrPercent(&partition, int(ctx.Replicas), true)
		body = fmt.Sprintf(`{"spec":{"strategy":{"rollingUpdate":{"maxSurge":"%s" }},"paused":%v}}`, partition.String(), false)
	}
	current, _ := intstr.GetScaledValueFromIntOrPercent(&ctx.CurrentPartition, int(ctx.Replicas), true)

	if current >= desired {
		klog.Infof("--zyb says: current %d >= desired %d, return nil", current, desired)
		return nil
	}
	klog.Infof("--zyb says: body is %s", body)

	d := util.GetEmptyObjectWithKey(rc.object)
	return rc.client.Patch(context.TODO(), d, client.RawPatch(types.MergePatchType, []byte(body)))
}

// TODO - 如果用户设置的MinReadys也很大?这里是假设用户设置的是0
func (rc *realController) Finalize(release *v1beta1.BatchRelease) error {
	klog.Info("--zyb says: in Finalize control function")
	if rc.object == nil {
		return nil // No need to finalize again.
	}
	patchData := patch.NewDeploymentPatch()
	if release.Spec.ReleasePlan.BatchPartition == nil {
		// webhook会设置pause，这里需要修改为false
		patchData.UpdatePaused(false)
		patchData.UpdateMinReadySeconds(0)
		setting := util.GetOriginalSetting(rc.object)
		// patchData.UpdateMinReadySeconds(setting.MinReadySeconds)
		if setting.ProgressDeadlineSeconds != nil {
			patchData.UpdateProgressDeadlineSeconds(*setting.ProgressDeadlineSeconds)
		}
		if setting.Strategy != nil {
			patchData.UpdateStrategy(*setting.Strategy)
		}
		patchData.DeleteAnnotation(v1beta1.OriginalSettingAnnotation)
		patchData.DeleteLabel(v1alpha1.DeploymentStableRevisionLabel)
	}
	d := rc.object.DeepCopy()
	patchData.DeleteAnnotation(util.BatchReleaseControlAnnotation)
	return rc.client.Patch(context.TODO(), d, patchData)
}

func (rc *realController) CalculateBatchContext(release *v1beta1.BatchRelease) (*batchcontext.BatchContext, error) {
	// TODO - 蓝绿不需要分批信息
	rolloutID := release.Spec.ReleasePlan.RolloutID
	// if rolloutID != "" {
	// 	// if rollout-id is set, the pod will be patched batch label,
	// 	// so we have to list pod here.
	// 	if _, err := rc.ListOwnedPods(); err != nil {
	// 		return nil, err
	// 	}
	// }

	currentBatch := release.Status.CanaryStatus.CurrentBatch
	desiredPartition := release.Spec.ReleasePlan.Batches[currentBatch].CanaryReplicas
	PlannedUpdatedReplicas := deploymentutil.NewRSReplicasLimit(desiredPartition, rc.object)
	currentPartition := intstr.FromInt(0)
	if rc.object.Spec.Strategy.RollingUpdate != nil && rc.object.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		currentPartition = *rc.object.Spec.Strategy.RollingUpdate.MaxSurge
		if rc.Status.UpdatedReplicas < 1 && currentPartition.String() == "100%" {
			currentPartition = intstr.FromString("0%")
		}
	}
	return &batchcontext.BatchContext{
		Pods:             rc.pods,
		RolloutID:        rolloutID,
		CurrentBatch:     currentBatch,
		CurrentPartition: currentPartition,
		UpdateRevision:   release.Status.UpdateRevision,
		DesiredPartition: desiredPartition,                          //
		FailureThreshold: release.Spec.ReleasePlan.FailureThreshold, //

		Replicas:               rc.Replicas,                    //
		UpdatedReplicas:        rc.Status.UpdatedReplicas,      //
		UpdatedReadyReplicas:   rc.Status.UpdatedReadyReplicas, //
		PlannedUpdatedReplicas: PlannedUpdatedReplicas,
		DesiredUpdatedReplicas: PlannedUpdatedReplicas, //
	}, nil
}

func (rc *realController) getWorkloadInfo(d *apps.Deployment) *util.WorkloadInfo {
	workloadInfo := util.ParseWorkload(d)
	workloadInfo.Status.UpdatedReadyReplicas = workloadInfo.Status.UpdatedReplicas
	if res, err := rc.getUpdatedReadyReplicas(d); err == nil {
		workloadInfo.Status.UpdatedReadyReplicas = res
	}
	workloadInfo.Status.StableRevision = d.Labels[v1alpha1.DeploymentStableRevisionLabel]
	return workloadInfo
}

func (rc *realController) getUpdatedReadyReplicas(d *apps.Deployment) (int32, error) {
	// Use a client.List to list all ReplicaSets
	rss := &apps.ReplicaSetList{}
	listOpts := []client.ListOption{
		client.InNamespace(d.Namespace),
		client.MatchingLabels(d.Spec.Selector.MatchLabels),
	}
	if err := rc.client.List(context.TODO(), rss, listOpts...); err != nil {
		klog.Warningf("getWorkloadInfo failed, because"+"%s", err.Error())
		return -1, err
	}
	allRSs := rss.Items
	// select rs owner by current deployment
	ownedRSs := make([]*apps.ReplicaSet, 0)
	for i := range allRSs {
		rs := &allRSs[i]
		if !rs.DeletionTimestamp.IsZero() {
			continue
		}

		if metav1.IsControlledBy(rs, d) {
			ownedRSs = append(ownedRSs, rs)
		}
	}
	newRS := deploymentutil.FindNewReplicaSet(d, ownedRSs)
	updatedReadyReplicas := int32(0)
	if newRS != nil {
		updatedReadyReplicas = newRS.Status.ReadyReplicas
	}
	return updatedReadyReplicas, nil
}
