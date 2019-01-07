/*
Copyright 2018 The Kubernetes Authors.

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

package core

import (
	"fmt"
	"strings"

	extensions "k8s.io/api/extensions/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/initialization"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/apis/core"
	apisextensions "k8s.io/kubernetes/pkg/apis/extensions"
	k8s_api_v1alpha1 "k8s.io/kubernetes/pkg/apis/extensions/v1alpha1"
	k8sfeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubeapiserver/admission/util"
	"k8s.io/kubernetes/pkg/quota"
	"k8s.io/kubernetes/pkg/quota/generic"
)

const (
	RawResourceName                   = "nvidia.com-gpu"
	ResourceExtendedResourceGPUPrefix = "extended.resource.gpu."
	MatchLabel                        = "type"
)

var ercResources = []core.ResourceName{
	core.ResourceExtendedResourceGPU,
}

// ensure we implement required interface
var _ quota.Evaluator = &ercEvaluator{}

type ercEvaluator struct {
	// listFuncByNamespace knows how to list extended resource claims
	listFuncByNamespace generic.ListFuncByNamespace
}

// NewExtendedResourceClaimsEvaluator returns an evaluator that can evaluate extended resource, but now only can evaluate gpu.
func NewExtendedResourceClaimEvaluator(f quota.ListerForResourceFunc) quota.Evaluator {
	listFuncByNamespace := generic.ListResourceUsingListerFunc(f, extensions.SchemeGroupVersion.WithResource("extendedresourceclaims"))
	return &ercEvaluator{listFuncByNamespace: listFuncByNamespace}
}

// Constraints verifies that all required resources are present on the item.
func (e *ercEvaluator) Constraints(required []core.ResourceName, item runtime.Object) error {
	erc, err := toInternalExtendedResourceClaimOrError(item)
	if err != nil {
		return err
	}
	nameMap := make(map[string]interface{})
	for _, name := range required {
		if name == core.ResourceExtendedResourceGPU {
			return nil
		}
		if strings.HasPrefix(string(name), core.ResourceExtendedResourceGPUPrefix) {
			nameMap[string(name)] = nil
		}
	}
	resourceType := fmt.Sprintf("%s%s", ResourceExtendedResourceGPUPrefix, erc.Spec.MetadataRequirements.MatchLabels[MatchLabel])
	if _, ok := nameMap[resourceType]; !ok {
		return fmt.Errorf("%s do not allow", resourceType)
	}
	return nil
}

// GroupResource that this evaluator tracks
func (*ercEvaluator) GroupResource() schema.GroupResource {
	return extensions.SchemeGroupVersion.WithResource("extendedresourceclaims").GroupResource()
}

// Handles returns true if the evaluator should handle the specified operation.
func (e *ercEvaluator) Handles(a admission.Attributes) bool {
	op := a.GetOperation()
	if op == admission.Create {
		return true
	}

	if op == admission.Update && utilfeature.DefaultFeatureGate.Enabled(k8sfeatures.DevicePlugins) {
		initialized, err := initialization.IsObjectInitialized(a.GetObject())
		if err != nil {
			// fail closed, will try to give an evaluation.
			utilruntime.HandleError(err)
			return true
		}
		// only handle the update if the object is initialized after the update.
		return initialized
	}
	// TODO: when the DevicePlugins feature gate is removed, remove
	// the initializationCompletion check as well, because it will become a
	// subset of the "initialized" condition.
	initializationCompletion, err := util.IsInitializationCompletion(a)
	if err != nil {
		// fail closed, will try to give an evaluation.
		utilruntime.HandleError(err)
		return true
	}
	return initializationCompletion
}

// Matches returns true if the evaluator matches the specified quota with the provided input item
func (e *ercEvaluator) Matches(resourceQuota *core.ResourceQuota, item runtime.Object) (bool, error) {
	return generic.Matches(resourceQuota, item, e.MatchingResources, generic.MatchesNoScopeFunc)
}

// MatchingResources takes the input specified list of resources and returns the set of resources it matches.
func (*ercEvaluator) MatchingResources(items []core.ResourceName) []core.ResourceName {
	result := make([]core.ResourceName, 0)
	for _, item := range items {
		// match erc resources
		if quota.Contains(ercResources, item) {
			result = append(result, item)
			continue
		}
		// match "extended.resource.gpu.k80-1024"
		if strings.HasPrefix(string(item), ResourceExtendedResourceGPUPrefix) {
			result = append(result, item)
			continue
		}
	}
	return result
}

// Usage knows how to measure usage associated with item.
func (e *ercEvaluator) Usage(item runtime.Object) (core.ResourceList, error) {
	result := core.ResourceList{}
	erc, err := toInternalExtendedResourceClaimOrError(item)
	if err != nil {
		return result, err
	}

	// charge for gpu
	switch erc.Spec.RawResourceName {
	case RawResourceName:
		erType := erc.Spec.MetadataRequirements.MatchLabels[MatchLabel]
		result = quota.Add(result, core.ResourceList{
			TypeWrapper(erType): *(resource.NewQuantity(erc.Spec.ExtendedResourceNum, resource.DecimalSI)),
		})
	default:
		return result, nil
	}
	return result, nil
}

func TypeWrapper(t string) core.ResourceName {
	return core.ResourceName(ResourceExtendedResourceGPUPrefix + t)
}

func (e *ercEvaluator) UsageStats(options quota.UsageStatsOptions) (quota.UsageStats, error) {
	return generic.CalculateUsageStats(options, e.listFuncByNamespace, generic.MatchesNoScopeFunc, e.Usage)
}

func toInternalExtendedResourceClaimOrError(obj runtime.Object) (*extensions.ExtendedResourceClaim, error) {
	erc := &extensions.ExtendedResourceClaim{}
	switch t := obj.(type) {
	case *extensions.ExtendedResourceClaim:
		erc = t
	case *apisextensions.ExtendedResourceClaim:
		if err := k8s_api_v1alpha1.Convert_extensions_ExtendedResourceClaim_To_v1alpha1_ExtendedResourceClaim(t, erc, nil); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("expect *extensions.ExtendedResourceClaim, got %v", t)
	}
	return erc, nil
}

func toInternalExtendedResourceOrError(obj runtime.Object) (*extensions.ExtendedResource, error) {
	er := &extensions.ExtendedResource{}
	switch t := obj.(type) {
	case *extensions.ExtendedResource:
		er = t
	case *apisextensions.ExtendedResource:
		if err := k8s_api_v1alpha1.Convert_extensions_ExtendedResource_To_v1alpha1_ExtendedResource(t, er, nil); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("expect *extensions.ExtendedResource, got %v", t)
	}
	return er, nil
}
