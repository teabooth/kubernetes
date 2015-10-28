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

package operations

import (
	"net/http"
	"time"

	log "github.com/golang/glog"
	"k8s.io/kubernetes/contrib/mesos/pkg/backoff"
	"k8s.io/kubernetes/contrib/mesos/pkg/queue"
	"k8s.io/kubernetes/contrib/mesos/pkg/runtime"
	"k8s.io/kubernetes/contrib/mesos/pkg/scheduler/config"
	"k8s.io/kubernetes/contrib/mesos/pkg/scheduler/podtask"
	"k8s.io/kubernetes/contrib/mesos/pkg/scheduler/queuer"
	types "k8s.io/kubernetes/contrib/mesos/pkg/scheduler/types"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	client "k8s.io/kubernetes/pkg/client/unversioned"
)

const (
	recoveryDelay = 100 * time.Millisecond // delay after scheduler plugin crashes, before we resume scheduling

	FailedScheduling = "FailedScheduling"
	Scheduled        = "Scheduled"
)

type SchedulerLoopInterface interface {
	ReconcilePodTask(t *podtask.T)

	// execute the Scheduling plugin, should start a go routine and return immediately
	Run(<-chan struct{})
}

type SchedulerLoop struct {
	algorithm *SchedulerAlgorithm
	binder    *Binder
	nextPod   func() *api.Pod
	error     func(*api.Pod, error)
	recorder  record.EventRecorder
	client    *client.Client
	pr        *PodReconciler
	starting  chan struct{} // startup latch
}

func NewSchedulerLoop(c *config.Config, fw types.Framework, client *client.Client, recorder record.EventRecorder,
	terminate <-chan struct{}, mux *http.ServeMux, podsWatcher *cache.ListWatch) *SchedulerLoop {

	// Watch and queue pods that need scheduling.
	updates := make(chan queue.Entry, c.UpdatesBacklog)
	podUpdates := &podStoreAdapter{queue.NewHistorical(updates)}
	reflector := cache.NewReflector(podsWatcher, &api.Pod{}, podUpdates, 0)

	// lock that guards critial sections that involve transferring pods from
	// the store (cache) to the scheduling queue; its purpose is to maintain
	// an ordering (vs interleaving) of operations that's easier to reason about.

	q := queuer.New(podUpdates)
	podDeleter := NewDeleter(fw, q)
	podReconciler := NewPodReconciler(fw, client, q, podDeleter)
	bo := backoff.New(c.InitialPodBackoff.Duration, c.MaxPodBackoff.Duration)
	eh := NewErrorHandler(fw, bo, q)
	startLatch := make(chan struct{})
	eventBroadcaster := record.NewBroadcaster()
	runtime.On(startLatch, func() {
		eventBroadcaster.StartRecordingToSink(client.Events(""))
		reflector.Run() // TODO(jdef) should listen for termination
		podDeleter.Run(updates, terminate)
		q.Run(terminate)

		q.InstallDebugHandlers(mux)
		podtask.InstallDebugHandlers(fw.Tasks(), mux)
	})
	return &SchedulerLoop{
		algorithm: NewSchedulerAlgorithm(fw, podUpdates),
		binder:    NewBinder(fw),
		nextPod:   q.Yield,
		error:     eh.Error,
		recorder:  recorder,
		client:    client,
		pr:        podReconciler,
		starting:  startLatch,
	}
}

func (s *SchedulerLoop) Run(done <-chan struct{}) {
	defer close(s.starting)
	go runtime.Until(s.scheduleOne, recoveryDelay, done)
}

// hacked from GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/scheduler.go,
// with the Modeler stuff removed since we don't use it because we have mesos.
func (s *SchedulerLoop) scheduleOne() {
	pod := s.nextPod()

	// pods which are pre-scheduled (i.e. NodeName is set) are deleted by the kubelet
	// in upstream. Not so in Mesos because the kubelet hasn't see that pod yet. Hence,
	// the scheduler has to take care of this:
	if pod.Spec.NodeName != "" && pod.DeletionTimestamp != nil {
		log.V(3).Infof("deleting pre-scheduled, not yet running pod: %s/%s", pod.Namespace, pod.Name)
		s.client.Pods(pod.Namespace).Delete(pod.Name, api.NewDeleteOptions(0))
		return
	}

	log.V(3).Infof("Attempting to schedule: %+v", pod)
	dest, err := s.algorithm.Schedule(pod)
	if err != nil {
		log.V(1).Infof("Failed to schedule: %+v", pod)
		s.recorder.Eventf(pod, FailedScheduling, "Error scheduling: %v", err)
		s.error(pod, err)
		return
	}
	b := &api.Binding{
		ObjectMeta: api.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
		Target: api.ObjectReference{
			Kind: "Node",
			Name: dest,
		},
	}
	if err := s.binder.Bind(b); err != nil {
		log.V(1).Infof("Failed to bind pod: %+v", err)
		s.recorder.Eventf(pod, FailedScheduling, "Binding rejected: %v", err)
		s.error(pod, err)
		return
	}
	s.recorder.Eventf(pod, Scheduled, "Successfully assigned %v to %v", pod.Name, dest)
}

func (s *SchedulerLoop) ReconcilePodTask(t *podtask.T) {
	s.pr.Reconcile(t)
}
