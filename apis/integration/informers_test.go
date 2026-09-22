/*
Copyright The Kubernetes Authors.

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

package integration_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	bufferv1alpha1 "k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/autoscaling.x-k8s.io/v1alpha1"
	bufferv1beta1 "k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/autoscaling.x-k8s.io/v1beta1"
	bufferfake "k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/client/clientset/versioned/fake"
	bufferscheme "k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/client/clientset/versioned/scheme"
	bufferinformers "k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/client/informers/externalversions"
	quotav1alpha1 "k8s.io/autoscaler/cluster-autoscaler/apis/capacityquota/autoscaling.x-k8s.io/v1alpha1"
	quotav1beta1 "k8s.io/autoscaler/cluster-autoscaler/apis/capacityquota/autoscaling.x-k8s.io/v1beta1"
	quotafake "k8s.io/autoscaler/cluster-autoscaler/apis/capacityquota/client/clientset/versioned/fake"
	quotascheme "k8s.io/autoscaler/cluster-autoscaler/apis/capacityquota/client/clientset/versioned/scheme"
	quotainformers "k8s.io/autoscaler/cluster-autoscaler/apis/capacityquota/client/informers/externalversions"
	requestv1 "k8s.io/autoscaler/cluster-autoscaler/apis/provisioningrequest/autoscaling.x-k8s.io/v1"
	requestv1beta1 "k8s.io/autoscaler/cluster-autoscaler/apis/provisioningrequest/autoscaling.x-k8s.io/v1beta1"
	requestfake "k8s.io/autoscaler/cluster-autoscaler/apis/provisioningrequest/client/clientset/versioned/fake"
	requestscheme "k8s.io/autoscaler/cluster-autoscaler/apis/provisioningrequest/client/clientset/versioned/scheme"
	requestinformers "k8s.io/autoscaler/cluster-autoscaler/apis/provisioningrequest/client/informers/externalversions"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func TestGeneratedInformerIntegration(t *testing.T) {
	t.Run("capacitybuffer/v1alpha1", func(t *testing.T) {
		object := &bufferv1alpha1.CapacityBuffer{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := bufferfake.NewSimpleClientset(object)
		factory := bufferinformers.NewSharedInformerFactoryWithOptions(client, 0, bufferinformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1alpha1().CapacityBuffers()
		exerciseInformer(t, object, bufferscheme.Scheme, bufferv1alpha1.SchemeGroupVersion.WithResource("capacitybuffers"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
	t.Run("capacitybuffer/v1beta1", func(t *testing.T) {
		object := &bufferv1beta1.CapacityBuffer{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := bufferfake.NewSimpleClientset(object)
		factory := bufferinformers.NewSharedInformerFactoryWithOptions(client, 0, bufferinformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1beta1().CapacityBuffers()
		exerciseInformer(t, object, bufferscheme.Scheme, bufferv1beta1.SchemeGroupVersion.WithResource("capacitybuffers"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
	t.Run("capacityquota/v1alpha1", func(t *testing.T) {
		object := &quotav1alpha1.CapacityQuota{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := quotafake.NewSimpleClientset(object)
		factory := quotainformers.NewSharedInformerFactoryWithOptions(client, 0, quotainformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1alpha1().CapacityQuotas()
		exerciseInformer(t, object, quotascheme.Scheme, quotav1alpha1.GroupVersion.WithResource("capacityquotas"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
	t.Run("capacityquota/v1beta1", func(t *testing.T) {
		object := &quotav1beta1.CapacityQuota{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := quotafake.NewSimpleClientset(object)
		factory := quotainformers.NewSharedInformerFactoryWithOptions(client, 0, quotainformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1beta1().CapacityQuotas()
		exerciseInformer(t, object, quotascheme.Scheme, quotav1beta1.GroupVersion.WithResource("capacityquotas"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
	t.Run("provisioningrequest/v1beta1", func(t *testing.T) {
		object := &requestv1beta1.ProvisioningRequest{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := requestfake.NewSimpleClientset(object)
		factory := requestinformers.NewSharedInformerFactoryWithOptions(client, 0, requestinformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1beta1().ProvisioningRequests()
		exerciseInformer(t, object, requestscheme.Scheme, requestv1beta1.SchemeGroupVersion.WithResource("provisioningrequests"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
	t.Run("provisioningrequest/v1", func(t *testing.T) {
		object := &requestv1.ProvisioningRequest{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test", ResourceVersion: "1"}}
		client := requestfake.NewSimpleClientset(object)
		factory := requestinformers.NewSharedInformerFactoryWithOptions(client, 0, requestinformers.WithNamespace("test"))
		informer := factory.Autoscaling().V1().ProvisioningRequests()
		exerciseInformer(t, object, requestscheme.Scheme, requestv1.SchemeGroupVersion.WithResource("provisioningrequests"), client.Tracker(), informer.TypedInformer(), informer.Informer())
	})
}

type apiObject interface {
	runtime.Object
	metav1.Object
	comparable
}

func exerciseInformer[T apiObject](t *testing.T, object T, scheme *runtime.Scheme, gvr schema.GroupVersionResource, tracker clienttesting.ObjectTracker, typed cache.TypedSharedIndexInformer[T], legacy cache.SharedIndexInformer) {
	t.Helper()
	codecs := serializer.NewCodecFactory(scheme)
	encoded, err := runtime.Encode(codecs.LegacyCodec(gvr.GroupVersion()), object)
	if err != nil {
		t.Fatal(err)
	}
	decoded, gvk, err := codecs.UniversalDeserializer().Decode(encoded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gvk.GroupVersion() != gvr.GroupVersion() || reflect.TypeOf(decoded) != reflect.TypeOf(object) {
		t.Fatalf("scheme round trip changed type or version: %T, %v", decoded, gvk)
	}
	if typed.GetIndexer() != legacy.GetIndexer() {
		t.Fatal("typed and legacy informers do not share the factory cache")
	}

	type event struct {
		operation string
		name      string
		version   string
	}
	events := make(chan event, 10)
	_, err = typed.AddTypedEventHandler(cache.TypedResourceEventHandlerFuncs[T]{
		AddFunc:    func(obj T) { events <- event{"add", obj.GetName(), obj.GetResourceVersion()} },
		UpdateFunc: func(_, obj T) { events <- event{"update", obj.GetName(), obj.GetResourceVersion()} },
		DeleteFunc: func(obj cache.DeletedObject[T]) { events <- event{"delete", obj.GetName(), ""} },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); typed.RunWithContext(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if !cache.WaitForCacheSync(ctx.Done(), typed.HasSynced) {
		t.Fatal("informer did not sync")
	}
	expect := func(want event) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event = %+v, want %+v", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %+v: %v", want, ctx.Err())
		}
	}
	expect(event{"add", "sample", "1"})
	key := "test/sample"
	cached, err := typed.GetTypedIndexer().ByTypedIndex(cache.NamespaceIndex, "test")
	if err != nil || len(cached) != 1 || cached[0].GetResourceVersion() != "1" {
		t.Fatalf("typed initial cache: objects=%v err=%v", cached, err)
	}
	updated := object.DeepCopyObject().(T)
	updated.SetResourceVersion("2")
	if err := tracker.Update(gvr, updated, "test"); err != nil {
		t.Fatal(err)
	}
	expect(event{"update", "sample", "2"})
	cachedLegacy, exists, err := legacy.GetIndexer().GetByKey(key)
	if err != nil || !exists || cachedLegacy.(T).GetResourceVersion() != "2" {
		t.Fatalf("legacy updated cache: object=%v exists=%v err=%v", cachedLegacy, exists, err)
	}
	if err := tracker.Delete(gvr, "test", "sample"); err != nil {
		t.Fatal(err)
	}
	expect(event{"delete", "sample", ""})
	if cached, err := typed.GetTypedIndexer().ByTypedIndex(cache.NamespaceIndex, "test"); err != nil || len(cached) != 0 {
		t.Fatalf("typed cache retained deleted object: objects=%v err=%v", cached, err)
	}
}
