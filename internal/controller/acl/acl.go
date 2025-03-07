/*
Copyright 2020 The Crossplane Authors.

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

package acl

import (
	"context"
	"strings"

	"github.com/crossplane-contrib/provider-kafka/internal/clients/kafka"

	v1 "github.com/crossplane/crossplane-runtime/apis/common/v1"

	"github.com/crossplane-contrib/provider-kafka/internal/clients/kafka/acl"

	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/twmb/franz-go/pkg/kadm"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane-contrib/provider-kafka/apis/acl/v1alpha1"
	apisv1alpha1 "github.com/crossplane-contrib/provider-kafka/apis/v1alpha1"
)

const (
	errNotAccessControlList = "managed resource is not a AccessControlList custom resource"
	errTrackPCUsage         = "cannot track ProviderConfig usage"
	errGetPC                = "cannot get ProviderConfig"
	errGetCreds             = "cannot get credentials"
	errListACL              = "cannot List ACLs"
	errNewClient            = "cannot create new Service"
	errUpdateNotSupported   = "updates are not supported"
)

// Setup adds a controller that reconciles AccessControlList managed resources.
func Setup(mgr ctrl.Manager, l logging.Logger) error {
	name := managed.ControllerName(v1alpha1.AccessControlListGroupKind)

	o := controller.Options{
		RateLimiter: ratelimiter.NewController(),
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.AccessControlListGroupVersionKind),
		managed.WithExternalConnectDisconnecter(&connectDisconnector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			log:          l,
			newServiceFn: kafka.NewAdminClient}),
		managed.WithLogger(l.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithInitializers())

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o).
		For(&v1alpha1.AccessControlList{}).
		Complete(r)
}

// A connectDisconnector is expected to produce an ExternalClient when its Connect method
// is called and close it when its Disconnect method is called.
type connectDisconnector struct {
	kube         client.Client
	usage        resource.Tracker
	log          logging.Logger
	newServiceFn func(ctx context.Context, creds []byte, kube client.Client) (*kadm.Client, error)
	cachedClient *kadm.Client
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connectDisconnector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.AccessControlList)
	if !ok {
		return nil, errors.New(errNotAccessControlList)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	svc, err := c.newServiceFn(ctx, data, c.kube)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}
	c.cachedClient = svc

	return &external{kafkaClient: svc, log: c.log}, nil
}

func (c *connectDisconnector) Disconnect(ctx context.Context) error {
	if c.cachedClient != nil {
		c.cachedClient.Close()
	}
	c.cachedClient = nil
	return nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	kafkaClient *kadm.Client
	log         logging.Logger
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.AccessControlList)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotAccessControlList)
	}

	// Check if the external name is set, to determine if ACL has been created or not
	ext := meta.GetExternalName(cr)
	if ext == "" {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	extname, _ := acl.ConvertFromJSON(meta.GetExternalName(cr))
	compare := acl.CompareAcls(*extname, *acl.Generate(&cr.Spec.ForProvider))
	diff := acl.Diff(*extname, *acl.Generate(&cr.Spec.ForProvider))

	if !compare {
		err := strings.Join(diff, " ")
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, errors.New(err)
	}

	ae, err := acl.List(ctx, c.kafkaClient, extname)

	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errListACL)
	}

	if ae == nil {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	cr.Status.SetConditions(v1.Available())

	return managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        true,
		ResourceLateInitialized: false,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {

	cr, ok := mg.(*v1alpha1.AccessControlList)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotAccessControlList)
	}

	generated := acl.Generate(&cr.Spec.ForProvider)
	extname, err := acl.ConvertToJSON(generated)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, "could not convert external name to JSON")
	}
	if meta.GetExternalName(cr) == "" {
		meta.SetExternalName(cr, extname)
		return managed.ExternalCreation{ExternalNameAssigned: true}, acl.Create(ctx, c.kafkaClient, generated)
	}

	return managed.ExternalCreation{}, acl.Create(ctx, c.kafkaClient, generated)
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {

	return managed.ExternalUpdate{}, errors.New(errUpdateNotSupported)
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {

	cr, ok := mg.(*v1alpha1.AccessControlList)
	if !ok {
		return errors.New(errNotAccessControlList)
	}

	return acl.Delete(ctx, c.kafkaClient, acl.Generate(&cr.Spec.ForProvider))
}
