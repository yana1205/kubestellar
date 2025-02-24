/*
Copyright The KubeStellar Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"

	v1alpha1 "github.com/kubestellar/kubestellar/pkg/apis/meta/v1alpha1"
)

// FakeAPIResources implements APIResourceInterface
type FakeAPIResources struct {
	Fake *FakeMetaV1alpha1
}

var apiresourcesResource = schema.GroupVersionResource{Group: "meta.kubestellar.io", Version: "v1alpha1", Resource: "apiresources"}

var apiresourcesKind = schema.GroupVersionKind{Group: "meta.kubestellar.io", Version: "v1alpha1", Kind: "APIResource"}

// Get takes name of the aPIResource, and returns the corresponding aPIResource object, and an error if there is any.
func (c *FakeAPIResources) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.APIResource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootGetAction(apiresourcesResource, name), &v1alpha1.APIResource{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.APIResource), err
}

// List takes label and field selectors, and returns the list of APIResources that match those selectors.
func (c *FakeAPIResources) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.APIResourceList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootListAction(apiresourcesResource, apiresourcesKind, opts), &v1alpha1.APIResourceList{})
	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.APIResourceList{ListMeta: obj.(*v1alpha1.APIResourceList).ListMeta}
	for _, item := range obj.(*v1alpha1.APIResourceList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested aPIResources.
func (c *FakeAPIResources) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewRootWatchAction(apiresourcesResource, opts))
}

// Create takes the representation of a aPIResource and creates it.  Returns the server's representation of the aPIResource, and an error, if there is any.
func (c *FakeAPIResources) Create(ctx context.Context, aPIResource *v1alpha1.APIResource, opts v1.CreateOptions) (result *v1alpha1.APIResource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootCreateAction(apiresourcesResource, aPIResource), &v1alpha1.APIResource{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.APIResource), err
}

// Update takes the representation of a aPIResource and updates it. Returns the server's representation of the aPIResource, and an error, if there is any.
func (c *FakeAPIResources) Update(ctx context.Context, aPIResource *v1alpha1.APIResource, opts v1.UpdateOptions) (result *v1alpha1.APIResource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateAction(apiresourcesResource, aPIResource), &v1alpha1.APIResource{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.APIResource), err
}

// Delete takes name of the aPIResource and deletes it. Returns an error if one occurs.
func (c *FakeAPIResources) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewRootDeleteActionWithOptions(apiresourcesResource, name, opts), &v1alpha1.APIResource{})
	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeAPIResources) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewRootDeleteCollectionAction(apiresourcesResource, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.APIResourceList{})
	return err
}

// Patch applies the patch and returns the patched aPIResource.
func (c *FakeAPIResources) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.APIResource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(apiresourcesResource, name, pt, data, subresources...), &v1alpha1.APIResource{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.APIResource), err
}
