// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package porch

import (
	"context"
	"fmt"
	"strings"

	api "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	configapi "github.com/GoogleContainerTools/kpt/porch/api/porchconfig/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/pkg/repository"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"
)

var tracer = otel.Tracer("apiserver")

type packageRevisions struct {
	packageCommon
	rest.TableConvertor
}

var _ rest.Storage = &packageRevisions{}
var _ rest.Lister = &packageRevisions{}
var _ rest.Getter = &packageRevisions{}
var _ rest.Scoper = &packageRevisions{}
var _ rest.Creater = &packageRevisions{}
var _ rest.Updater = &packageRevisions{}
var _ rest.GracefulDeleter = &packageRevisions{}

func (r *packageRevisions) New() runtime.Object {
	return &api.PackageRevision{}
}

func (r *packageRevisions) NewList() runtime.Object {
	return &api.PackageRevisionList{}
}

func (r *packageRevisions) NamespaceScoped() bool {
	return true
}

// List selects resources in the storage which match to the selector. 'options' can be nil.
func (r *packageRevisions) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	ctx, span := tracer.Start(ctx, "packageRevisions::List", trace.WithAttributes())
	defer span.End()

	result := &api.PackageRevisionList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PackageRevisionList",
			APIVersion: api.SchemeGroupVersion.Identifier(),
		},
	}

	filter, err := parsePackageRevisionFieldSelector(options.FieldSelector)
	if err != nil {
		return nil, err
	}

	if err := r.packageCommon.listPackages(ctx, filter, func(p repository.PackageRevision) error {
		item := p.GetPackageRevision()
		result.Items = append(result.Items, *item)
		return nil
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// Get implements the Getter interface
func (r *packageRevisions) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	ctx, span := tracer.Start(ctx, "packageRevisions::Get", trace.WithAttributes())
	defer span.End()

	return r.packageCommon.getPackageRevision(ctx, name, options)
}

// Create implements the Creater interface.
func (r *packageRevisions) Create(ctx context.Context, runtimeObject runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	ctx, span := tracer.Start(ctx, "packageRevisions::Create", trace.WithAttributes())
	defer span.End()

	ns, namespaced := genericapirequest.NamespaceFrom(ctx)
	if !namespaced {
		return nil, apierrors.NewBadRequest("namespace must be specified")
	}

	obj, ok := runtimeObject.(*api.PackageRevision)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected PackageRevision object, got %T", runtimeObject))
	}

	// TODO: Accpept some form of client-provided name, for example using GenerateName
	// and figure out where we can store it (in Kptfile?). Porch can then append unique
	// suffix to the names while respecting client-provided value as well.
	if obj.Name != "" {
		klog.Warningf("Client provided metadata.name %q", obj.Name)
	}

	repositoryName := obj.Spec.RepositoryName
	if repositoryName == "" {
		return nil, apierrors.NewBadRequest("spec.repositoryName is required")
	}

	var repositoryObj configapi.Repository
	repositoryID := types.NamespacedName{Namespace: ns, Name: repositoryName}
	if err := r.coreClient.Get(ctx, repositoryID, &repositoryObj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewNotFound(configapi.KindRepository.GroupResource(), repositoryID.Name)
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("error getting repository %v: %w", repositoryID, err))
	}

	fieldErrors := r.createStrategy.Validate(ctx, runtimeObject)
	if len(fieldErrors) > 0 {
		return nil, apierrors.NewInvalid(api.SchemeGroupVersion.WithKind("PackageRevision").GroupKind(), obj.Name, fieldErrors)
	}

	rev, err := r.cad.CreatePackageRevision(ctx, &repositoryObj, obj)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	created := rev.GetPackageRevision()
	return created, nil
}

// Update implements the Updater interface.

// Update finds a resource in the storage and updates it. Some implementations
// may allow updates creates the object - they should set the created boolean
// to true.
func (r *packageRevisions) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	ctx, span := tracer.Start(ctx, "packageRevisions::Update", trace.WithAttributes())
	defer span.End()

	return r.packageCommon.updatePackageRevision(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
}

// Delete implements the GracefulDeleter interface.
// Delete finds a resource in the storage and deletes it.
// The delete attempt is validated by the deleteValidation first.
// If options are provided, the resource will attempt to honor them or return an invalid
// request error.
// Although it can return an arbitrary error value, IsNotFound(err) is true for the
// returned error value err when the specified resource is not found.
// Delete *may* return the object that was deleted, or a status object indicating additional
// information about deletion.
// It also returns a boolean which is set to true if the resource was instantly
// deleted or false if it will be deleted asynchronously.
func (r *packageRevisions) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	ctx, span := tracer.Start(ctx, "packageRevisions::Delete", trace.WithAttributes())
	defer span.End()

	ns, namespaced := genericapirequest.NamespaceFrom(ctx)
	if !namespaced {
		return nil, false, apierrors.NewBadRequest("namespace must be specified")
	}

	oldPackage, err := r.packageCommon.getPackage(ctx, name)
	if err != nil {
		return nil, false, err
	}

	oldObj := oldPackage.GetPackageRevision()

	if deleteValidation != nil {
		err := deleteValidation(ctx, oldObj)
		if err != nil {
			klog.Infof("delete failed validation: %v", err)
			return nil, false, err
		}
	}

	// TODO: Verify options are empty?

	repositoryName, err := ParseRepositoryName(name)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid name %q", name))
	}

	var repositoryObj configapi.Repository
	repositoryID := types.NamespacedName{Namespace: ns, Name: repositoryName}
	if err := r.coreClient.Get(ctx, repositoryID, &repositoryObj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, apierrors.NewNotFound(configapi.KindRepository.GroupResource(), repositoryID.Name)
		}
		return nil, false, apierrors.NewInternalError(fmt.Errorf("error getting repository %v: %w", repositoryID, err))
	}

	if err := r.cad.DeletePackageRevision(ctx, &repositoryObj, oldPackage); err != nil {
		return nil, false, apierrors.NewInternalError(err)
	}

	// TODO: Should we do an async delete?
	return oldObj, true, nil
}

// PackageRevisions Update Strategy

type packageRevisionStrategy struct{}

var _ SimpleRESTUpdateStrategy = packageRevisionStrategy{}

func (s packageRevisionStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
}

func (s packageRevisionStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	allErrs := field.ErrorList{}
	oldRevision := old.(*api.PackageRevision)
	newRevision := obj.(*api.PackageRevision)

	switch lifecycle := oldRevision.Spec.Lifecycle; lifecycle {
	case "", api.PackageRevisionLifecycleDraft, api.PackageRevisionLifecycleProposed:
		// valid

	default:
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "lifecycle"), lifecycle, fmt.Sprintf("can only update package with lifecycle value one of %s",
			strings.Join([]string{
				string(api.PackageRevisionLifecycleDraft),
				string(api.PackageRevisionLifecycleProposed),
			}, ",")),
		))

	}

	switch lifecycle := newRevision.Spec.Lifecycle; lifecycle {
	case "", api.PackageRevisionLifecycleDraft, api.PackageRevisionLifecycleProposed:
		// valid

	default:
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "lifecycle"), lifecycle, fmt.Sprintf("value can be only updated to %s",
			strings.Join([]string{
				string(api.PackageRevisionLifecycleDraft),
				string(api.PackageRevisionLifecycleProposed),
			}, ",")),
		))
	}

	return allErrs
}

func (s packageRevisionStrategy) Canonicalize(obj runtime.Object) {
	pr := obj.(*api.PackageRevision)
	if pr.Spec.Lifecycle == "" {
		// Set default
		pr.Spec.Lifecycle = api.PackageRevisionLifecycleDraft
	}
}

var _ SimpleRESTCreateStrategy = packageRevisionStrategy{}

// Validate returns an ErrorList with validation errors or nil.  Validate
// is invoked after default fields in the object have been filled in
// before the object is persisted.  This method should not mutate the
// object.
func (s packageRevisionStrategy) Validate(ctx context.Context, runtimeObj runtime.Object) field.ErrorList {
	allErrs := field.ErrorList{}

	obj := runtimeObj.(*api.PackageRevision)

	switch lifecycle := obj.Spec.Lifecycle; lifecycle {
	case "", api.PackageRevisionLifecycleDraft:
		// valid

	default:
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "lifecycle"), lifecycle, fmt.Sprintf("value can be only created as %s",
			strings.Join([]string{
				string(api.PackageRevisionLifecycleDraft),
			}, ",")),
		))
	}

	return allErrs
}
