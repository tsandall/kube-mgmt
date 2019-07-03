// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package data

import (
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	opa_client "github.com/open-policy-agent/kube-mgmt/pkg/opa"
	"github.com/open-policy-agent/kube-mgmt/pkg/types"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// GenericSync replicates Kubernetes resources into OPA as raw JSON.
type GenericSync struct {
	kubeconfig *rest.Config
	opa        opa_client.Data
	ns         types.ResourceType
	internal   chan struct{}
}

// The min/max amount of time to wait when resetting the synchronizer.
const (
	backoffMax = time.Second * 30
	backoffMin = time.Second
)

// New returns a new GenericSync that cna be started.
func New(kubeconfig *rest.Config, opa opa_client.Data, ns types.ResourceType) *GenericSync {
	return &GenericSync{
		kubeconfig: kubeconfig,
		ns:         ns,
		opa:        opa.Prefix(ns.Resource),
	}
}

// Run starts the synchronizer. To stop the synchronizer send a message to the
// channel.
func (s *GenericSync) Run() (chan struct{}, error) {

	client, err := dynamic.NewForConfig(s.kubeconfig)
	if err != nil {
		return nil, err
	}

	quit := make(chan struct{})
	go s.loop(client, quit)
	return quit, nil
}

func (s *GenericSync) loop(client dynamic.Interface, quit chan struct{}) {

	defer func() {
		logrus.Infof("Sync for %v finished. Exiting.", s.ns)
	}()

	resource := client.Resource(schema.GroupVersionResource{
		Group:    s.ns.Group,
		Version:  s.ns.Version,
		Resource: s.ns.Resource,
	})

	delay := backoffMin

	for {

		err := s.sync(resource, quit)
		if err == nil {
			return
		}

		switch err.(type) {

		case errChannelClosed:
			logrus.Infof("Sync channel for %v closed. Restarting immediately.", s.ns)
			delay = backoffMin

		case errOPA:
			logrus.Errorf("Sync for %v failed due to OPA error. Trying again in %v. Reason: %v", s.ns, delay, err)
			delay = backoffMin
			t := time.NewTimer(delay)
			select {
			case <-t.C:
				break
			case <-quit:
				return
			}

		case errKubernetes:
			logrus.Errorf("Sync for %v failed due to Kubernetes error. Trying again in %v. Reason: %v", s.ns, delay, err)
			delay *= 2
			if delay > backoffMax {
				delay = backoffMax
			}
			t := time.NewTimer(delay)
			select {
			case <-t.C:
				break
			case <-quit:
				return
			}
		}
	}
}

type errKubernetes error

type errOPA error

type errChannelClosed struct{}

func (errChannelClosed) Error() string {
	return "channel closed"
}

// sync starts replicating Kubernetes resources into OPA. If an error occurs
// during the replication process this function returns and indicates whether
// the synchronizer should backoff. The synchronizer will backoff whenever the
// Kubernetes API returns an error.
func (s *GenericSync) sync(resource dynamic.NamespaceableResourceInterface, quit chan struct{}) error {

	logrus.Infof("Syncing %v.", s.ns)
	tList := time.Now()
	result, err := resource.List(metav1.ListOptions{})
	if err != nil {
		return errKubernetes(errors.Wrap(err, "list"))
	}

	dList := time.Since(tList)
	resourceVersion := result.GetResourceVersion()
	logrus.Infof("Listed %v and got %v resources with resourceVersion %v. Took %v.", s.ns, len(result.Items), resourceVersion, dList)

	tLoad := time.Now()

	// NOTE(tsandall): currently we reset OPA and load the list result in two
	// separate transactions. If this is an issue we can revisit this. One
	// option would be to create a PATCH request that clears the data namespace
	// and then adds all of the objects.
	if err := s.syncReset(); err != nil {
		return errOPA(errors.Wrap(err, "reset"))
	}

	for _, item := range result.Items {
		if err := s.syncAdd(&item); err != nil {
			return errOPA(errors.Wrap(err, "list add"))
		}
	}

	dLoad := time.Since(tLoad)
	logrus.Infof("Loaded %v resources into OPA. Took %v. Starting watch at resourceVersion %v.", s.ns, dLoad, resourceVersion)

	w, err := resource.Watch(metav1.ListOptions{
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return errKubernetes(errors.Wrap(err, "watch"))
	}

	defer w.Stop()

	ch := w.ResultChan()

	for {
		select {
		case evt := <-ch:
			switch evt.Type {
			case watch.Added:
				err := s.syncAdd(evt.Object)
				if err != nil {
					return errOPA(errors.Wrap(err, "add event"))
				}
			case watch.Modified:
				err := s.syncAdd(evt.Object)
				if err != nil {
					return errOPA(errors.Wrap(err, "modify event"))
				}
			case watch.Deleted:
				err := s.syncRemove(evt.Object)
				if err != nil {
					return errOPA(errors.Wrap(err, "delete event"))
				}
			case watch.Error:
				return errKubernetes(fmt.Errorf("error event: %v", evt.Object))
			default:
				return errChannelClosed{}
			}
		case <-quit:
			return nil
		}
	}
}

func (s *GenericSync) syncAdd(obj runtime.Object) error {
	m, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	name := m.GetName()
	var path = m.GetName()
	if s.ns.Namespaced {
		path = m.GetNamespace() + "/" + name
	}
	return s.opa.PutData(path, obj)
}

func (s *GenericSync) syncRemove(obj runtime.Object) error {
	m, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	name := m.GetName()
	var path = m.GetName()
	if s.ns.Namespaced {
		path = m.GetNamespace() + "/" + name
	}
	return s.opa.PatchData(path, "remove", nil)
}

func (s *GenericSync) syncReset() error {
	return s.opa.PutData("/", map[string]interface{}{})
}
