/*
Copyright 2019 The Knative Authors

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

package revision

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	cachingclient "knative.dev/caching/pkg/client/injection/client"
	imageinformer "knative.dev/caching/pkg/client/injection/informers/caching/v1alpha1/image"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	networkingclient "knative.dev/networking/pkg/client/injection/client"
	certificateinformer "knative.dev/networking/pkg/client/injection/informers/networking/v1alpha1/certificate"
	"knative.dev/pkg/changeset"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	deploymentinformer "knative.dev/pkg/client/injection/kube/informers/apps/v1/deployment"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	painformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/podautoscaler"
	revisioninformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/revision"
	revisionreconciler "knative.dev/serving/pkg/client/injection/reconciler/serving/v1/revision"
	"knative.dev/serving/pkg/observability"

	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	netcfg "knative.dev/networking/pkg/config"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	apisconfig "knative.dev/serving/pkg/apis/config"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/deployment"
	"knative.dev/serving/pkg/reconciler/revision/config"
)

// digestResolutionWorkers is the number of image digest resolutions that can
// take place in parallel. MaxIdleConns and MaxIdleConnsPerHost for the digest
// resolution's Transport will also be set to this value.
const digestResolutionWorkers = 100

// NewController initializes the controller and is called by the generated code
// Registers eventhandlers to enqueue events
func NewController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	return newControllerWithOptions(ctx, cmw)
}

type reconcilerOption func(*Reconciler)

func newControllerWithOptions(
	ctx context.Context,
	cmw configmap.Watcher,
	opts ...reconcilerOption,
) *controller.Impl {
	logger := logging.FromContext(ctx)
	revisionInformer := revisioninformer.Get(ctx)
	deploymentInformer := deploymentinformer.Get(ctx)
	imageInformer := imageinformer.Get(ctx)
	paInformer := painformer.Get(ctx)
	certificateInformer := certificateinformer.Get(ctx)

	c := &Reconciler{
		kubeclient:       kubeclient.Get(ctx),
		client:           servingclient.Get(ctx),
		networkingclient: networkingclient.Get(ctx),
		cachingclient:    cachingclient.Get(ctx),

		podAutoscalerLister: paInformer.Lister(),
		imageLister:         imageInformer.Lister(),
		deploymentLister:    deploymentInformer.Lister(),
		certificateLister:   certificateInformer.Lister(),
	}

	impl := revisionreconciler.NewImpl(ctx, c, func(impl *controller.Impl) controller.Options {
		configsToResync := []interface{}{
			&netcfg.Config{},
			&observability.Config{},
			&deployment.Config{},
			&apisconfig.Defaults{},
		}

		resync := configmap.TypeFilter(configsToResync...)(func(string, interface{}) {
			// Triggers syncs on all revisions when configuration
			// changes
			impl.GlobalResync(revisionInformer.Informer())
		})

		configStore := config.NewStore(logger.Named("config-store"), resync)
		configStore.WatchConfigs(cmw)
		return controller.Options{ConfigStore: configStore}
	})

	c.tracker = impl.Tracker

	transport := http.DefaultTransport
	if rt, err := newResolverTransport(k8sCertPath, digestResolutionWorkers, digestResolutionWorkers); err != nil {
		logging.FromContext(ctx).Errorw("Failed to create resolver transport", zap.Error(err))
	} else {
		transport = rt
	}

	userAgent := fmt.Sprintf("knative/%s (serving)", changeset.Get())

	digestResolveQueue := workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.NewTypedMaxOfRateLimiter(
		newItemExponentialFailureRateLimiter(1*time.Second, 1000*time.Second),
		// 10 qps, 100 bucket size.  This is only for retry speed and its only the overall factor (not per item)
		&workqueue.TypedBucketRateLimiter[any]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	), workqueue.TypedRateLimitingQueueConfig[any]{Name: "digests"})

	resolver := newBackgroundResolver(logger, &digestResolver{client: kubeclient.Get(ctx), transport: transport, userAgent: userAgent}, digestResolveQueue, impl.EnqueueKey)
	resolver.Start(ctx.Done(), digestResolutionWorkers)
	c.resolver = resolver

	// Set up an event handler for when the resource types of interest change
	revisionInformer.Informer().AddEventHandler(controller.HandleAll(impl.Enqueue))

	handleMatchingControllers := cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterController(&v1.Revision{}),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	}
	deploymentInformer.Informer().AddEventHandler(handleMatchingControllers)
	paInformer.Informer().AddEventHandler(handleMatchingControllers)
	certificateInformer.Informer().AddEventHandler(controller.HandleAll(
		// Call the tracker's OnChanged method, but we've seen the objects
		// coming through this path missing TypeMeta, so ensure it is properly
		// populated.
		controller.EnsureTypeMeta(
			c.tracker.OnChanged,
			v1alpha1.SchemeGroupVersion.WithKind("Certificate"),
		),
	))

	// We don't watch for changes to Image because we don't incorporate any of its
	// properties into our own status and should work completely in the absence of
	// a functioning Image controller.

	for _, opt := range opts {
		opt(c)
	}
	return impl
}
