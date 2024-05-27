/*
Copyright 2023 The Kruise Authors.

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

package v1beta1

import (
	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// DeploymentStrategyAnnotation is annotation for deployment,
	// which is strategy fields of Advanced Deployment.
	DeploymentStrategyAnnotation = "rollouts.kruise.io/deployment-strategy"

	// DeploymentExtraStatusAnnotation is annotation for deployment,
	// which is extra status field of Advanced Deployment.
	DeploymentExtraStatusAnnotation = "rollouts.kruise.io/deployment-extra-status"

	// DeploymentStableRevisionLabel is label for deployment,
	// which record the stable revision during the current rolling process.
	DeploymentStableRevisionLabel = "rollouts.kruise.io/stable-revision"

	// AdvancedDeploymentControlLabel is label for deployment,
	// which labels whether the deployment is controlled by advanced-deployment-controller.
	AdvancedDeploymentControlLabel = "rollouts.kruise.io/controlled-by-advanced-deployment-controller"

	// OriginalSettingAnnotation is annotation for workload in BlueGreen Release,
	// it will storage the original setting of the workload, which will be used to restore the workload
	OriginalSettingAnnotation = "rollouts.kruise.io/original-setting"

	// MaxProgressSeconds is the value we set for ProgressDeadlineSeconds
	// MaxReadySeconds is the value we set for MinReadySeconds, which is one less than ProgressDeadlineSeconds
	// MaxInt32: 2147483647, ≈ 68 years
	MaxProgressSeconds = 1<<31 - 1
	MaxReadySeconds    = MaxProgressSeconds - 1
)

// DeploymentStrategy is strategy field for Advanced Deployment
type DeploymentStrategy struct {
	// RollingStyle define the behavior of rolling for deployment.
	RollingStyle RollingStyleType `json:"rollingStyle,omitempty"`
	// original deployment strategy rolling update fields
	RollingUpdate *apps.RollingUpdateDeployment `json:"rollingUpdate,omitempty"`
	// Paused = true will block the upgrade of Pods
	Paused bool `json:"paused,omitempty"`
	// Partition describe how many Pods should be updated during rollout.
	// We use this field to implement partition-style rolling update.
	Partition intstr.IntOrString `json:"partition,omitempty"`
}

// OriginalSetting storages part of the fileds of a workload,
// so that it can be restored when finalizing.
// It is only used for BlueGreen Release
// Similar to DeploymentStrategy, it is stored in workload annotation
// However, unlike DeploymentStrategy, it is only used to storage and restore
type OriginalSetting struct {
	// The deployment strategy to use to replace existing pods with new ones.
	// +optional
	// +patchStrategy=retainKeys
	Strategy *apps.DeploymentStrategy `json:"strategy,omitempty" patchStrategy:"retainKeys" protobuf:"bytes,4,opt,name=strategy"`

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty" protobuf:"varint,5,opt,name=minReadySeconds"`

	// The maximum time in seconds for a deployment to make progress before it
	// is considered to be failed. The deployment controller will continue to
	// process failed deployments and a condition with a ProgressDeadlineExceeded
	// reason will be surfaced in the deployment status. Note that progress will
	// not be estimated during the time a deployment is paused. Defaults to 600s.
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty" protobuf:"varint,9,opt,name=progressDeadlineSeconds"`
}

type RollingStyleType string

const (
	// PartitionRollingStyle means rolling in batches just like CloneSet, and will NOT create any extra Deployment;
	PartitionRollingStyle RollingStyleType = "Partition"
	// CanaryRollingStyle means rolling in canary way, and will create a canary Deployment.
	CanaryRollingStyle RollingStyleType = "Canary"
	// BlueGreenRollingStyle means rolling in blue-green way, and will NOT create a extra Deployment.
	BlueGreenRollingStyle RollingStyleType = "BlueGreen"
	// Empty means both Canary and BlueGreen are empty
	EmptyRollingStyle RollingStyleType = "Empty"
)

// DeploymentExtraStatus is extra status field for Advanced Deployment
type DeploymentExtraStatus struct {
	// UpdatedReadyReplicas the number of pods that has been updated and ready.
	UpdatedReadyReplicas int32 `json:"updatedReadyReplicas,omitempty"`
	// ExpectedUpdatedReplicas is an absolute number calculated based on Partition
	// and Deployment.Spec.Replicas, means how many pods are expected be updated under
	// current strategy.
	// This field is designed to avoid users to fall into the details of algorithm
	// for Partition calculation.
	ExpectedUpdatedReplicas int32 `json:"expectedUpdatedReplicas,omitempty"`
}

func SetDefaultDeploymentStrategy(strategy *DeploymentStrategy) {
	if strategy.RollingStyle != PartitionRollingStyle {
		return
	}
	if strategy.RollingUpdate == nil {
		strategy.RollingUpdate = &apps.RollingUpdateDeployment{}
	}
	if strategy.RollingUpdate.MaxUnavailable == nil {
		// Set MaxUnavailable as 25% by default
		maxUnavailable := intstr.FromString("25%")
		strategy.RollingUpdate.MaxUnavailable = &maxUnavailable
	}
	if strategy.RollingUpdate.MaxSurge == nil {
		// Set MaxSurge as 25% by default
		maxSurge := intstr.FromString("25%")
		strategy.RollingUpdate.MaxUnavailable = &maxSurge
	}

	// Cannot allow maxSurge==0 && MaxUnavailable==0, otherwise, no pod can be updated when rolling update.
	maxSurge, _ := intstr.GetScaledValueFromIntOrPercent(strategy.RollingUpdate.MaxSurge, 100, true)
	maxUnavailable, _ := intstr.GetScaledValueFromIntOrPercent(strategy.RollingUpdate.MaxUnavailable, 100, true)
	if maxSurge == 0 && maxUnavailable == 0 {
		strategy.RollingUpdate = &apps.RollingUpdateDeployment{
			MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		}
	}
}

func SetDefaultSetting(setting *OriginalSetting) {
	if setting.ProgressDeadlineSeconds == nil {
		setting.ProgressDeadlineSeconds = new(int32)
		*setting.ProgressDeadlineSeconds = 600
	}
	if setting.Strategy == nil {
		setting.Strategy = &apps.DeploymentStrategy{}
	}
	if setting.Strategy.Type == "" {
		setting.Strategy.Type = apps.RollingUpdateDeploymentStrategyType
	}
	if setting.Strategy.Type == apps.RecreateDeploymentStrategyType {
		return
	}
	strategy := setting.Strategy
	if strategy.RollingUpdate == nil {
		strategy.RollingUpdate = &apps.RollingUpdateDeployment{}
	}
	if strategy.RollingUpdate.MaxUnavailable == nil {
		// Set MaxUnavailable as 25% by default
		maxUnavailable := intstr.FromString("25%")
		strategy.RollingUpdate.MaxUnavailable = &maxUnavailable
	}
	if strategy.RollingUpdate.MaxSurge == nil {
		// Set MaxSurge as 25% by default
		maxSurge := intstr.FromString("25%")
		strategy.RollingUpdate.MaxUnavailable = &maxSurge
	}

	// Cannot allow maxSurge==0 && MaxUnavailable==0, otherwise, no pod can be updated when rolling update.
	maxSurge, _ := intstr.GetScaledValueFromIntOrPercent(strategy.RollingUpdate.MaxSurge, 100, true)
	maxUnavailable, _ := intstr.GetScaledValueFromIntOrPercent(strategy.RollingUpdate.MaxUnavailable, 100, true)
	if maxSurge == 0 && maxUnavailable == 0 {
		strategy.RollingUpdate = &apps.RollingUpdateDeployment{
			MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		}
	}

}
