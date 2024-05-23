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

package rollout

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/api/v1beta1"
	"github.com/openkruise/rollouts/pkg/trafficrouting"
	"github.com/openkruise/rollouts/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ReleaseManager interface {
	runCanary(c *RolloutContext) error
	doCanaryFinalising(c *RolloutContext) (bool, error)
	fetchBatchRelease(ns, name string) (*v1beta1.BatchRelease, error)
	removeBatchRelease(c *RolloutContext) (bool, error)
}

type blueGreenReleaseManager struct {
	client.Client
	trafficRoutingManager *trafficrouting.Manager
	recorder              record.EventRecorder
}

func (m *blueGreenReleaseManager) runCanary(c *RolloutContext) error {
	blueGreenStatus := c.NewStatus.BlueGreenStatus
	if br, err := m.fetchBatchRelease(c.Rollout.Namespace, c.Rollout.Name); err != nil && !errors.IsNotFound(err) {
		klog.Errorf("rollout(%s/%s) fetch batchRelease failed: %s", c.Rollout.Namespace, c.Rollout.Name, err.Error())
		return err
	} else if err == nil {
		// This line will do something important:
		// - sync status from br to Rollout: to better observability;
		// - sync rollout-id from Rollout to br: to make BatchRelease
		//   relabels pods in the scene where only rollout-id is changed.
		if err = m.syncBatchRelease(br, blueGreenStatus); err != nil {
			klog.Errorf("rollout(%s/%s) sync batchRelease failed: %s", c.Rollout.Namespace, c.Rollout.Name, err.Error())
			return err
		}
	}
	// update podTemplateHash, Why is this position assigned?
	// Because If workload is deployment, only after canary pod already was created,
	// we can get the podTemplateHash from pod.annotations[pod-template-hash]
	// PodTemplateHash is used to select a new version of the Pod.

	// Note:
	// In a scenario of successive releases v1->v2->v3, It is possible that this is the PodTemplateHash value of v2,
	// so it needs to be set later in the stepUpgrade
	//FIXME - c.Workload.PodTemplateHash已经计算好了，在哪里赋值没有意义
	if blueGreenStatus.PodTemplateHash == "" {
		blueGreenStatus.PodTemplateHash = c.Workload.PodTemplateHash
	}
	// When the first batch is trafficRouting rolling and the next steps are rolling release,
	// We need to clean up the canary-related resources first and then rollout the rest of the batch.
	currentStep := c.Rollout.Spec.Strategy.BlueGreen.Steps[blueGreenStatus.CurrentStepIndex-1]
	if currentStep.Traffic == nil && len(currentStep.Matches) == 0 {
		tr := newTrafficRoutingContext(c)
		done, err := m.trafficRoutingManager.FinalisingTrafficRouting(tr)
		blueGreenStatus.LastUpdateTime = tr.LastUpdateTime
		if err != nil {
			return err
		} else if !done {
			klog.Infof("rollout(%s/%s) cleaning up canary-related resources", c.Rollout.Namespace, c.Rollout.Name)
			expectedTime := time.Now().Add(time.Duration(defaultGracePeriodSeconds) * time.Second)
			c.RecheckTime = &expectedTime
			return nil
		}
	}

	switch blueGreenStatus.CurrentStepState {
	// 在upgrade之前，处理一些特殊情况，以防止流量丢失
	case v1beta1.CanaryStepStateInit:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateInit)
		tr := newTrafficRoutingContext(c)
		if tr.Strategy.Traffic == nil {
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateUpgrade
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateInit, blueGreenStatus.CurrentStepState)
			return nil
		}
		if blueGreenStatus.CurrentStepIndex == 1 || *tr.Strategy.Traffic == "0%" || *tr.Strategy.Traffic == "0" {
			done, err := m.trafficRoutingManager.PatchStableService(tr)
			if err != nil || !done {
				return err
			}
		}
		blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
		blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateUpgrade
		klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
			blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateInit, blueGreenStatus.CurrentStepState)

	case v1beta1.CanaryStepStateUpgrade:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateUpgrade)
		done, err := m.doCanaryUpgrade(c)
		if err != nil {
			return err
		} else if done {
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateTrafficRouting
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateUpgrade, blueGreenStatus.CurrentStepState)
		}

	case v1beta1.CanaryStepStateTrafficRouting:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateTrafficRouting)
		tr := newTrafficRoutingContext(c)
		done, err := m.trafficRoutingManager.DoTrafficRouting(tr)
		blueGreenStatus.LastUpdateTime = tr.LastUpdateTime
		if err != nil {
			return err
		} else if done {
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateMetricsAnalysis
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateTrafficRouting, blueGreenStatus.CurrentStepState)
		}
		expectedTime := time.Now().Add(time.Duration(defaultGracePeriodSeconds) * time.Second)
		c.RecheckTime = &expectedTime

	case v1beta1.CanaryStepStateMetricsAnalysis:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateMetricsAnalysis)
		done, err := m.doCanaryMetricsAnalysis(c)
		if err != nil {
			return err
		} else if done {
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStatePaused
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateMetricsAnalysis, blueGreenStatus.CurrentStepState)
		}

	case v1beta1.CanaryStepStatePaused:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStatePaused)
		done, err := m.doCanaryPaused(c)
		if err != nil {
			return err
		} else if done {
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateReady
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStatePaused, blueGreenStatus.CurrentStepState)
		}

	case v1beta1.CanaryStepStateReady:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateReady)
		// run next step
		if len(c.Rollout.Spec.Strategy.BlueGreen.Steps) > int(blueGreenStatus.CurrentStepIndex) {
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepIndex++
			blueGreenStatus.NextStepIndex = util.NextBatchIndex(c.Rollout, blueGreenStatus.CurrentStepIndex)
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateInit
			klog.Infof("rollout(%s/%s) canary step from(%d) -> to(%d)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.CurrentStepIndex-1, blueGreenStatus.CurrentStepIndex)
		} else {
			klog.Infof("rollout(%s/%s) canary run all steps, and completed", c.Rollout.Namespace, c.Rollout.Name)
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateCompleted
			return nil
		}
		klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
			blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStateReady, blueGreenStatus.CurrentStepState)
		// canary completed
	case v1beta1.CanaryStepStateCompleted:
		klog.Infof("rollout(%s/%s) run canary strategy, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, v1beta1.CanaryStepStateCompleted)
	}

	return nil
}

func (m *blueGreenReleaseManager) doCanaryUpgrade(c *RolloutContext) (bool, error) {
	// verify whether batchRelease configuration is the latest
	steps := len(c.Rollout.Spec.Strategy.BlueGreen.Steps)
	blueGreenStatus := c.NewStatus.BlueGreenStatus
	cond := util.GetRolloutCondition(*c.NewStatus, v1beta1.RolloutConditionProgressing)
	cond.Message = fmt.Sprintf("Rollout is in step(%d/%d), and upgrade workload to new version", blueGreenStatus.CurrentStepIndex, steps)
	c.NewStatus.Message = cond.Message
	// run batch release to upgrade the workloads
	done, br, err := m.runBatchRelease(c.Rollout, getRolloutID(c.Workload), blueGreenStatus.CurrentStepIndex, c.Workload.IsInRollback)
	if err != nil {
		return false, err
	} else if !done {
		return false, nil
	}
	// 和workload一样，底层的BatchRelease也有自己独立的控制逻辑，所以需对它修改后应该考虑它的控制器是否已经将它
	// 更新到最新了
	if br.Status.ObservedReleasePlanHash != util.HashReleasePlanBatches(&br.Spec.ReleasePlan) ||
		br.Generation != br.Status.ObservedGeneration {
		klog.Infof("rollout(%s/%s) batchRelease status is inconsistent, and wait a moment", c.Rollout.Namespace, c.Rollout.Name)
		return false, nil
	}
	// check whether batchRelease is ready(whether new pods is ready.)
	// 前面那个CurrentBatch是从0开始的，后面那个是从1开始的
	if br.Status.CanaryStatus.CurrentBatchState != v1beta1.ReadyBatchState ||
		br.Status.CanaryStatus.CurrentBatch+1 < blueGreenStatus.CurrentStepIndex {
		klog.Infof("rollout(%s/%s) batchRelease status(%s) is not ready, and wait a moment", c.Rollout.Namespace, c.Rollout.Name, util.DumpJSON(br.Status))
		return false, nil
	}
	m.recorder.Eventf(c.Rollout, corev1.EventTypeNormal, "Progressing", fmt.Sprintf("upgrade step(%d) canary pods with new versions done", blueGreenStatus.CurrentStepIndex))
	klog.Infof("rollout(%s/%s) batch(%s) state(%s), and success",
		c.Rollout.Namespace, c.Rollout.Name, util.DumpJSON(br.Status), br.Status.CanaryStatus.CurrentBatchState)
	// set the latest PodTemplateHash to selector the latest pods.
	// 对应上面那个注释
	//FIXME - c.Workload.PodTemplateHash已经计算好了，在哪里赋值没有意义
	blueGreenStatus.PodTemplateHash = c.Workload.PodTemplateHash
	return true, nil
}

func (m *blueGreenReleaseManager) doCanaryMetricsAnalysis(c *RolloutContext) (bool, error) {
	// todo
	return true, nil
}

func (m *blueGreenReleaseManager) doCanaryPaused(c *RolloutContext) (bool, error) {

	blueGreenStatus := c.NewStatus.BlueGreenStatus
	currentIndex := blueGreenStatus.CurrentStepIndex
	nextIndex := blueGreenStatus.NextStepIndex
	currentStep := c.Rollout.Spec.Strategy.BlueGreen.Steps[currentIndex-1]
	steps := len(c.Rollout.Spec.Strategy.BlueGreen.Steps)
	if nextIndex != util.NextBatchIndex(c.Rollout, currentIndex) && nextIndex != 0 || nextIndex < 0 {
		if nextIndex < 0 {
			nextIndex = util.NextBatchIndex(c.Rollout, currentIndex)
		}
		currentIndexBackup := blueGreenStatus.CurrentStepIndex
		blueGreenStatus.CurrentStepIndex = nextIndex
		blueGreenStatus.NextStepIndex = util.NextBatchIndex(c.Rollout, nextIndex)
		nextStep := c.Rollout.Spec.Strategy.BlueGreen.Steps[nextIndex-1]
		//TODO - 或许应该再比较一下int和百分比的情况是否相等？
		if reflect.DeepEqual(nextStep.Replicas, currentStep.Replicas) {
			// 不要更新LastUpdateTime，减少一次等待grace time
			// blueGreen.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateTrafficRouting
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStatePaused, blueGreenStatus.CurrentStepState)
		} else {
			blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
			blueGreenStatus.CurrentStepState = v1beta1.CanaryStepStateInit
			klog.Infof("rollout(%s/%s) step(%d) state from(%s) -> to(%s)", c.Rollout.Namespace, c.Rollout.Name,
				blueGreenStatus.CurrentStepIndex, v1beta1.CanaryStepStatePaused, v1beta1.CanaryStepStateInit)
		}
		klog.Infof("rollout(%s/%s) canary step from(%d) -> to(%d)", c.Rollout.Namespace, c.Rollout.Name, currentIndexBackup, blueGreenStatus.CurrentStepIndex)
		return false, nil
	}

	cond := util.GetRolloutCondition(*c.NewStatus, v1beta1.RolloutConditionProgressing)
	// need manual confirmation
	if currentStep.Pause.Duration == nil {
		klog.Infof("rollout(%s/%s) don't set pause duration, and need manual confirmation", c.Rollout.Namespace, c.Rollout.Name)
		cond.Message = fmt.Sprintf("Rollout is in step(%d/%d), and you need manually confirm to enter the next step", blueGreenStatus.CurrentStepIndex, steps)
		c.NewStatus.Message = cond.Message
		return false, nil
	}
	cond.Message = fmt.Sprintf("Rollout is in step(%d/%d), and wait duration(%d seconds) to enter the next step", blueGreenStatus.CurrentStepIndex, steps, *currentStep.Pause.Duration)
	c.NewStatus.Message = cond.Message
	// wait duration time, then go to next step
	duration := time.Second * time.Duration(*currentStep.Pause.Duration)
	// 是从上次更新时间开始计时的
	expectedTime := blueGreenStatus.LastUpdateTime.Add(duration)
	if expectedTime.Before(time.Now()) {
		klog.Infof("rollout(%s/%s) canary step(%d) paused duration(%d seconds), and go to the next step",
			c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.CurrentStepIndex, *currentStep.Pause.Duration)
		return true, nil
	}
	c.RecheckTime = &expectedTime
	return false, nil
}

// cleanup after rollout is completed or finished
func (m *blueGreenReleaseManager) doCanaryFinalising(c *RolloutContext) (bool, error) {
	blueGreenStatus := c.NewStatus.BlueGreenStatus
	// when blueGreenStatus is nil, which means canary action hasn't started yet, don't need doing cleanup
	if blueGreenStatus == nil {
		return true, nil
	}
	// 1. rollout progressing complete, remove rollout progressing annotation in workload
	// 删除这个annotation后，workload的该变化会被rollout和batchRealse同时捕捉到
	err := m.removeRolloutProgressingAnnotation(c)
	if err != nil {
		return false, err
	}
	tr := newTrafficRoutingContext(c)
	klog.Infof("---zyb says: enter the swith-case, the start state is %s", blueGreenStatus.FinalisingStep)
	switch blueGreenStatus.FinalisingStep {
	default:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		blueGreenStatus.FinalisingStep = v1beta1.FinalisingStepTypePreparing
		fallthrough
	case v1beta1.FinalisingStepTypePreparing:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		// 2. route all traffic to pods won't be scaled down
		done, err := m.trafficRoutingManager.RouteAllTrafficToCanaryORStable(tr, c.FinalizeReason)
		if err != nil || !done {
			blueGreenStatus.LastUpdateTime = tr.LastUpdateTime
			klog.Infof("---zyb says: out the swith-case, the final state is %s", blueGreenStatus.FinalisingStep)
			return done, err
		}
		blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
		blueGreenStatus.FinalisingStep = v1beta1.FinalisingStepTypeBatchRelease
		fallthrough
	case v1beta1.FinalisingStepTypeBatchRelease:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		// 3. set workload.pause=false; set workload.partition=0
		done, err := m.finalizingBatchRelease(c)
		if err != nil || !done {
			klog.Infof("---zyb says: out the swith-case, the final state is %s", blueGreenStatus.FinalisingStep)
			return done, err
		}
		// blueGreen.LastUpdateTime = &metav1.Time{Time: time.Now()}
		blueGreenStatus.FinalisingStep = v1beta1.FinalisingStepTypeStableService
		// fallthrough
	case v1beta1.FinalisingStepTypeStableService:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		// 4. modify network api(ingress or gateway api) configuration, and route 100% traffic to stable pods.
		done, err := m.trafficRoutingManager.FinalisingTrafficRouting(tr)
		if err != nil || !done {
			blueGreenStatus.LastUpdateTime = tr.LastUpdateTime
			klog.Infof("---zyb says: out the swith-case, the final state is %s", blueGreenStatus.FinalisingStep)
			return done, err
		}
		blueGreenStatus.LastUpdateTime = &metav1.Time{Time: time.Now()}
		blueGreenStatus.FinalisingStep = v1beta1.FinalisingStepTypeConf
		// fallthrough
	case v1beta1.FinalisingStepTypeConf:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		if blueGreenStatus.LastUpdateTime != nil {
			// After restore the network configuration, give network provider 3 seconds to react
			if verifyTime := blueGreenStatus.LastUpdateTime.Add(time.Second * time.Duration(3)); verifyTime.After(time.Now()) {
				klog.Infof("---zyb says: restoring network configuration, but we need to wait %d seconds", 3)
				klog.Infof("---zyb says: out the swith-case, the final state is %s", blueGreenStatus.FinalisingStep)
				return false, nil
			}
		}
		blueGreenStatus.FinalisingStep = v1beta1.FinalisingStepTypeDeleteBR
		fallthrough
	case v1beta1.FinalisingStepTypeDeleteBR:
		klog.Infof("---rollout(%s/%s) run canary finalising, and state(%s)", c.Rollout.Namespace, c.Rollout.Name, blueGreenStatus.FinalisingStep)
		// 5. delete batchRelease crd
		done, err := m.removeBatchRelease(c)
		if err != nil {
			klog.Errorf("rollout(%s/%s) Finalize batchRelease failed: %s", c.Rollout.Namespace, c.Rollout.Name, err.Error())
			return false, err
		} else if !done {
			klog.Infof("---zyb says: out the swith-case, the final state is %s", blueGreenStatus.FinalisingStep)
			return false, nil
		}
		klog.Infof("rollout(%s/%s) doCanaryFinalising success", c.Rollout.Namespace, c.Rollout.Name)
		return true, nil
	}

	return false, nil
}

func (m *blueGreenReleaseManager) removeRolloutProgressingAnnotation(c *RolloutContext) error {
	if c.Workload == nil {
		return nil
	}
	if _, ok := c.Workload.Annotations[util.InRolloutProgressingAnnotation]; !ok {
		return nil
	}
	workloadRef := c.Rollout.Spec.WorkloadRef
	workloadGVK := schema.FromAPIVersionAndKind(workloadRef.APIVersion, workloadRef.Kind)
	obj := util.GetEmptyWorkloadObject(workloadGVK)
	obj.SetNamespace(c.Workload.Namespace)
	obj.SetName(c.Workload.Name)
	body := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, util.InRolloutProgressingAnnotation)
	if err := m.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		klog.Errorf("rollout(%s/%s) patch workload(%s) failed: %s", c.Rollout.Namespace, c.Rollout.Name, c.Workload.Name, err.Error())
		return err
	}
	klog.Infof("remove rollout(%s/%s) workload(%s) annotation[%s] success", c.Rollout.Namespace, c.Rollout.Name, c.Workload.Name, util.InRolloutProgressingAnnotation)
	return nil
}

func (m *blueGreenReleaseManager) runBatchRelease(rollout *v1beta1.Rollout, rolloutId string, batch int32, isRollback bool) (bool, *v1beta1.BatchRelease, error) {
	batch = batch - 1
	br, err := m.fetchBatchRelease(rollout.Namespace, rollout.Name)
	if errors.IsNotFound(err) {
		// create new BatchRelease Crd
		br = createBatchRelease_BG(rollout, rolloutId, batch, isRollback)
		// 刚创建完，直接返回false了
		if err = m.Create(context.TODO(), br); err != nil && !errors.IsAlreadyExists(err) {
			klog.Errorf("rollout(%s/%s) create BatchRelease failed: %s", rollout.Namespace, rollout.Name, err.Error())
			return false, nil, err
		}
		klog.Infof("rollout(%s/%s) create BatchRelease(%s) success", rollout.Namespace, rollout.Name, util.DumpJSON(br))
		return false, br, nil
	} else if err != nil {
		klog.Errorf("rollout(%s/%s) fetch BatchRelease failed: %s", rollout.Namespace, rollout.Name, err.Error())
		return false, nil, err
	}

	// check whether batchRelease configuration is the latest
	// createBatchRelease_BG得到的一定是最新的：除了status都修改/创建了
	newBr := createBatchRelease_BG(rollout, rolloutId, batch, isRollback)
	if reflect.DeepEqual(br.Spec, newBr.Spec) && reflect.DeepEqual(br.Annotations, newBr.Annotations) {
		klog.Infof("rollout(%s/%s) do batchRelease batch(%d) success", rollout.Namespace, rollout.Name, batch+1)
		return true, br, nil
	}
	// update batchRelease to the latest version
	if err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err = m.Get(context.TODO(), client.ObjectKey{Namespace: newBr.Namespace, Name: newBr.Name}, br); err != nil {
			klog.Errorf("error getting BatchRelease(%s/%s) from client", newBr.Namespace, newBr.Name)
			return err
		}
		br.Spec = newBr.Spec
		br.Annotations = newBr.Annotations
		return m.Client.Update(context.TODO(), br)
	}); err != nil {
		klog.Errorf("rollout(%s/%s) update batchRelease failed: %s", rollout.Namespace, rollout.Name, err.Error())
		return false, nil, err
	}
	klog.Infof("rollout(%s/%s) update batchRelease(%s) configuration to latest", rollout.Namespace, rollout.Name, util.DumpJSON(br))
	return false, br, nil
}

func (m *blueGreenReleaseManager) fetchBatchRelease(ns, name string) (*v1beta1.BatchRelease, error) {
	br := &v1beta1.BatchRelease{}
	// batchRelease.name is equal related rollout.name
	err := m.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, br)
	return br, err
}

func createBatchRelease_BG(rollout *v1beta1.Rollout, rolloutID string, batch int32, isRollback bool) *v1beta1.BatchRelease {
	var batches []v1beta1.ReleaseBatch
	for _, step := range rollout.Spec.Strategy.BlueGreen.Steps {
		batches = append(batches, v1beta1.ReleaseBatch{CanaryReplicas: *step.Replicas})
	}
	br := &v1beta1.BatchRelease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       rollout.Namespace,
			Name:            rollout.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rollout, rolloutControllerKind)},
		},
		Spec: v1beta1.BatchReleaseSpec{
			WorkloadRef: v1beta1.ObjectRef{
				APIVersion: rollout.Spec.WorkloadRef.APIVersion,
				Kind:       rollout.Spec.WorkloadRef.Kind,
				Name:       rollout.Spec.WorkloadRef.Name,
			},
			//以防rollout spec在发布中被更改，所以Batches和其他一些字段也要传过去
			ReleasePlan: v1beta1.ReleasePlan{
				Batches:          batches,
				RolloutID:        rolloutID,
				BatchPartition:   utilpointer.Int32Ptr(batch),
				FailureThreshold: rollout.Spec.Strategy.BlueGreen.FailureThreshold,
				RollingStyle:     v1beta1.BlueGreenRollingStyle,
			},
		},
	}
	annotations := map[string]string{}
	if isRollback {
		annotations[v1alpha1.RollbackInBatchAnnotation] = rollout.Annotations[v1alpha1.RollbackInBatchAnnotation]
	}
	if len(annotations) > 0 {
		br.Annotations = annotations
	}
	return br
}

func (m *blueGreenReleaseManager) removeBatchRelease(c *RolloutContext) (bool, error) {
	batch := &v1beta1.BatchRelease{}
	err := m.Get(context.TODO(), client.ObjectKey{Namespace: c.Rollout.Namespace, Name: c.Rollout.Name}, batch)
	if err != nil && errors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		klog.Errorf("rollout(%s/%s) fetch BatchRelease failed: %s", c.Rollout.Namespace, c.Rollout.Name)
		return false, err
	}
	if !batch.DeletionTimestamp.IsZero() {
		klog.Infof("rollout(%s/%s) BatchRelease is terminating, and wait a moment", c.Rollout.Namespace, c.Rollout.Name)
		return false, nil
	}

	//delete batchRelease
	err = m.Delete(context.TODO(), batch)
	if err != nil {
		klog.Errorf("rollout(%s/%s) delete BatchRelease failed: %s", c.Rollout.Namespace, c.Rollout.Name, err.Error())
		return false, err
	}
	klog.Infof("rollout(%s/%s) deleting BatchRelease, and wait a moment", c.Rollout.Namespace, c.Rollout.Name)
	return false, nil
}

func (m *blueGreenReleaseManager) finalizingBatchRelease(c *RolloutContext) (bool, error) {
	br, err := m.fetchBatchRelease(c.Rollout.Namespace, c.Rollout.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	waitReady := c.WaitReady
	// The Completed phase means batchRelease controller has processed all it
	// should process. If BatchRelease phase is completed, we can do nothing.
	if br.Spec.ReleasePlan.BatchPartition == nil &&
		br.Status.Phase == v1beta1.RolloutPhaseCompleted {
		klog.Infof("rollout(%s/%s) finalizing batchRelease(%s) done", c.Rollout.Namespace, c.Rollout.Name, util.DumpJSON(br.Status))
		return true, nil
	}

	// If BatchPartition is nil, BatchRelease will directly resume workload via:
	// - * set workload Paused = false if it needs;
	// - * set workload Partition = null if it needs.
	if br.Spec.ReleasePlan.BatchPartition == nil {
		// - If checkReady is true, finalizing policy must be "WaitResume";
		// - If checkReady is false, finalizing policy must be NOT "WaitResume";
		// Otherwise, we should correct it.
		if (br.Spec.ReleasePlan.FinalizingPolicy&v1beta1.WaitResumeFinalizingPolicyType != 0) == waitReady {
			return false, nil // no need to patch again
		}
	}

	// Correct finalizing policy.
	var policy v1beta1.FinalizingPolicyType = 0
	if waitReady {
		policy |= v1beta1.WaitResumeFinalizingPolicyType
	}
	if c.FinalizeReason != v1beta1.FinaliseReasonSuccess {
		policy |= v1beta1.ScaleDownFinalizingPolicyType
	}

	// Patch BatchPartition and FinalizingPolicy, BatchPartition always patch null here.
	body := fmt.Sprintf(`{"spec":{"releasePlan":{"batchPartition":null,"finalizingPolicy":%d}}}`, policy)
	if err = m.Patch(context.TODO(), br, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		return false, err
	}
	klog.Infof("rollout(%s/%s) patch batchRelease(%s) success", c.Rollout.Namespace, c.Rollout.Name, body)
	return false, nil
}

// syncBatchRelease sync status of br to blueGreenStatus, and sync rollout-id of blueGreenStatus to br.
func (m *blueGreenReleaseManager) syncBatchRelease(br *v1beta1.BatchRelease, blueGreenStatus *v1beta1.BlueGreenStatus) error {
	// sync from BatchRelease status to Rollout blueGreenStatus
	blueGreenStatus.CanaryReplicas = br.Status.CanaryStatus.UpdatedReplicas
	blueGreenStatus.CanaryReadyReplicas = br.Status.CanaryStatus.UpdatedReadyReplicas
	// Do not remove this line currently, otherwise, users will be not able to judge whether the BatchRelease works
	// in the scene where only rollout-id changed.
	// TODO: optimize the logic to better understand
	blueGreenStatus.Message = fmt.Sprintf("BatchRelease is at state %s, rollout-id %s, step %d",
		br.Status.CanaryStatus.CurrentBatchState, br.Status.ObservedRolloutID, br.Status.CanaryStatus.CurrentBatch+1)

	// sync rolloutId from blueGreenStatus to BatchRelease
	if blueGreenStatus.ObservedRolloutID != br.Spec.ReleasePlan.RolloutID {
		body := fmt.Sprintf(`{"spec":{"releasePlan":{"rolloutID":"%s"}}}`, blueGreenStatus.ObservedRolloutID)
		return m.Patch(context.TODO(), br, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	return nil
}
