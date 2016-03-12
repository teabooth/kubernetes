/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package node

import (
	"strings"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/util"
)

const (
	RunningExecutorCondition api.NodeConditionType = "RunningExecutor"

	SlaveReadyReason  = "SlaveReady"
	SlaveReadyMessage = "mesos reports ready status"

	RunningExecutorMessage = "node is running k8sm executor"
)

const uuidPrefix = "uuid_" // prefix for Condition.Reason of RunningExecutorCondition

func ParseConditionReason(cond api.NodeCondition) (interface{}, bool) {
	switch cond.Type {
	case RunningExecutorCondition:
		if strings.HasPrefix(cond.Reason, uuidPrefix) && len(cond.Reason) > len(uuidPrefix) {
			return cond.Reason[len(uuidPrefix):], true
		}
	default:
		// noop
	}
	return nil, false
}

// SetRunningExecutorCondition serves to associate an executor heartbeat w/ the
// node (we only allow a single executor instance running per node anyway). The Reason
// field is populated with `uuid={mesosContainerID}` in order to allow an external
// process to reconcile pods with active executors.
func SetRunningExecutorCondition(mesosContainerID string, clock util.Clock) func(*api.Node) error {
	return func(node *api.Node) error {
		var (
			currentTime   = unversioned.NewTime(clock.Now())
			nodeCondition *api.NodeCondition
		)

		reasonCode := uuidPrefix + mesosContainerID
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == RunningExecutorCondition {
				nodeCondition = &node.Status.Conditions[i]
			}
		}

		if nodeCondition != nil {
			nodeCondition.LastHeartbeatTime = currentTime
			if nodeCondition.Reason != reasonCode {
				nodeCondition.Reason = reasonCode
				nodeCondition.LastTransitionTime = currentTime
			}
		} else {
			nodeCondition = &api.NodeCondition{
				Type:               RunningExecutorCondition,
				Status:             api.ConditionTrue,
				Reason:             reasonCode,
				Message:            RunningExecutorMessage,
				LastHeartbeatTime:  currentTime,
				LastTransitionTime: currentTime,
			}
			node.Status.Conditions = append(node.Status.Conditions, *nodeCondition)
		}
		return nil
	}
}