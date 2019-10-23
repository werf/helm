/*
Copyright The Helm Authors.

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

package kube // import "k8s.io/helm/pkg/kube"

import (
	"bytes"
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/flant/logboek"
	"k8s.io/apimachinery/pkg/api/meta"

	jsonpatch "github.com/evanphx/json-patch"
	appsv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	batch "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	resource_quantity "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/scheme"
	watchtools "k8s.io/client-go/tools/watch"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/validation"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/kubectl/cmd/get"
)

// MissingGetHeader is added to Get's output when a resource is not found.
const MissingGetHeader = "==> MISSING\nKIND\t\tNAME\n"

const (
	ownerReleaseAnnotation = "werf.io/owner-release"
)

const (
	SetReplicasOnlyOnCreationAnnotation  = "werf.io/set-replicas-only-on-creation"
	SetResourcesOnlyOnCreationAnnotation = "werf.io/set-resources-only-on-creation"

	repairPatchAnnotation        = "debug.werf.io/repair-patch"
	repairPatchErrorsAnnotation  = "debug.werf.io/repair-patch-errors"
	validationWarningsAnnotation = "debug.werf.io/validation-messages"

	// Repair messages may include: warnings, instructions and other messages related to repair process
	repairMessagesAnnotation = "debug.werf.io/repair-messages"
)

var (
	RepairDebugMessages []string
	LastClientWarnings  []string
)

// ErrNoObjectsVisited indicates that during a visit operation, no matching objects were found.
var ErrNoObjectsVisited = goerrors.New("no objects visited")

var metadataAccessor = meta.NewAccessor()

// Client represents a client capable of communicating with the Kubernetes API.
type Client struct {
	cmdutil.Factory
	Log func(string, ...interface{})

	ResourcesWaiter ResourcesWaiter
}

type ResourcesWaiter interface {
	// WatchUntilReady watch the resource in reader until it is "ready".
	//
	// For Jobs, "ready" means the job ran to completion (excited without error).
	// For all other kinds, it means the kind was created or modified without
	// error.
	WatchUntilReady(namespace string, reader io.Reader, timeout time.Duration) error

	// WaitForResources polls to get the current status of all pods, PVCs, and Services
	// until all are ready or a timeout is reached
	WaitForResources(timeout time.Duration, created Result) error
}

func (c *Client) SetResourcesWaiter(waiter ResourcesWaiter) {
	c.ResourcesWaiter = waiter
}

// New creates a new Client.
func New(getter genericclioptions.RESTClientGetter) *Client {
	if getter == nil {
		getter = genericclioptions.NewConfigFlags(true)
	}

	err := apiextv1beta1.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}

	return &Client{
		Factory: cmdutil.NewFactory(getter),
		Log:     nopLogger,
	}
}

var nopLogger = func(_ string, _ ...interface{}) {}

// ResourceActorFunc performs an action on a single resource.
type ResourceActorFunc func(*resource.Info) error

// CreateOptions provides options to control create behavior
type CreateOptions struct {
	Timeout     int64
	ShouldWait  bool
	ReleaseName string
}

// Deprecated; use CreateWithOptions instead
func (c *Client) Create(namespace string, reader io.Reader, timeout int64, shouldWait bool) error {
	return c.CreateWithOptions(namespace, reader, CreateOptions{Timeout: timeout, ShouldWait: shouldWait})
}

// CreateWithOptions creates Kubernetes resources from an io.reader.
//
// Namespace will set the namespace.
func (c *Client) CreateWithOptions(namespace string, reader io.Reader, opts CreateOptions) error {
	LastClientWarnings = nil

	client, err := c.KubernetesClientSet()
	if err != nil {
		return err
	}
	if err := ensureNamespace(client, namespace); err != nil {
		return err
	}
	c.Log("building resources from manifest")
	infos, buildErr := c.BuildUnstructured(namespace, reader)
	if buildErr != nil {
		return buildErr
	}

	c.Log("creating %d resource(s)", len(infos))

	if err := perform(infos, func(info *resource.Info) error {
		helper := resource.NewHelper(info.Client, info.Mapping)
		currentObj, err := helper.Get(info.Namespace, info.Name, info.Export)

		if errors.IsNotFound(err) {
			// resource not found

			validateAndSetWarnings(c, info.Object)

			if err := createResource(info, opts.ReleaseName); err != nil {
				return fmt.Errorf("unable to create resource %s/%s: %s", info.Mapping.GroupVersionKind.Kind, info.Name, err)
			}

			return nil
		} else if err != nil {
			return fmt.Errorf("could not get information about the resource %s/%s: %s", info.Mapping.GroupVersionKind, info.Name, err)
		}

		// resource found

		adoptObjectAllowed := false
		ownerRelease := getObjectAnnotation(currentObj, ownerReleaseAnnotation)
		if opts.ReleaseName != "" && ownerRelease == opts.ReleaseName {
			adoptObjectAllowed = true
		}

		if !adoptObjectAllowed {
			return fmt.Errorf(
				"Kind %s with the name %q already exists in the cluster. Before installing, please either delete the resource from the cluster or remove it from the chart or set \"%s\": \"%s\" annotation to the object, which will cause werf to adopt existing resource into the release",
				info.Mapping.GroupVersionKind.Kind,
				info.Name,
				ownerReleaseAnnotation,
				opts.ReleaseName,
			)
		}

		validateAndSetWarnings(c, info.Object)
		if err := adoptResource(c, info, currentObj, opts.ReleaseName); err != nil {
			return fmt.Errorf("unable to adopt resource %s/%s: %s", info.Mapping.GroupVersionKind.Kind, info.Name, err)
		}

		return nil
	}); err != nil {
		return err
	}

	if opts.ShouldWait {
		return c.waitForResources(time.Duration(opts.Timeout)*time.Second, infos)
	}
	return nil
}

func (c *Client) newBuilder(namespace string, reader io.Reader) *resource.Result {
	return c.NewBuilder().
		ContinueOnError().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		Schema(c.validator()).
		NamespaceParam(namespace).
		DefaultNamespace().
		Stream(reader, "").
		Flatten().
		Do()
}

func (c *Client) validator() validation.Schema {
	schema, err := c.Validator(true)
	if err != nil {
		c.Log("warning: failed to load schema: %s", err)
	}
	return schema
}

// BuildUnstructured reads Kubernetes objects and returns unstructured infos.
func (c *Client) BuildUnstructured(namespace string, reader io.Reader) (Result, error) {
	var result Result

	result, err := c.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(namespace).
		DefaultNamespace().
		Stream(reader, "").
		Flatten().
		Do().Infos()
	return result, scrubValidationError(err)
}

// Validate reads Kubernetes manifests and validates the content.
//
// This function does not actually do schema validation of manifests. Adding
// validation now breaks existing clients of helm: https://github.com/helm/helm/issues/5750
func (c *Client) Validate(namespace string, reader io.Reader) error {
	_, err := c.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(namespace).
		DefaultNamespace().
		// Schema(c.validator()). // No schema validation
		Stream(reader, "").
		Flatten().
		Do().Infos()
	return scrubValidationError(err)
}

// Build validates for Kubernetes objects and returns resource Infos from a io.Reader.
func (c *Client) Build(namespace string, reader io.Reader) (Result, error) {
	var result Result
	result, err := c.newBuilder(namespace, reader).Infos()
	return result, scrubValidationError(err)
}

// Return the resource info as internal
func resourceInfoToObject(info *resource.Info, c *Client) runtime.Object {
	internalObj, err := asInternal(info)
	if err != nil {
		// If the problem is just that the resource is not registered, don't print any
		// error. This is normal for custom resources.
		if !runtime.IsNotRegisteredError(err) {
			c.Log("Warning: conversion to internal type failed: %v", err)
		}
		// Add the unstructured object in this situation. It will still get listed, just
		// with less information.
		return info.Object
	}

	return internalObj
}

func sortByKey(objs map[string](map[string]runtime.Object)) []string {
	var keys []string
	// Create a simple slice, so we can sort it
	for key := range objs {
		keys = append(keys, key)
	}
	// Sort alphabetically by version/kind keys
	sort.Strings(keys)
	return keys
}

// Get gets Kubernetes resources as pretty-printed string.
//
// Namespace will set the namespace.
func (c *Client) Get(namespace string, reader io.Reader) (string, error) {
	// Since we don't know what order the objects come in, let's group them by the types and then sort them, so
	// that when we print them, they come out looking good (headers apply to subgroups, etc.).
	objs := make(map[string](map[string]runtime.Object))
	infos, err := c.BuildUnstructured(namespace, reader)
	if err != nil {
		return "", err
	}

	var objPods = make(map[string][]v1.Pod)

	missing := []string{}
	err = perform(infos, func(info *resource.Info) error {
		c.Log("Doing get for %s: %q", info.Mapping.GroupVersionKind.Kind, info.Name)
		if err := info.Get(); err != nil {
			c.Log("WARNING: Failed Get for resource %q: %s", info.Name, err)
			missing = append(missing, fmt.Sprintf("%v\t\t%s", info.Mapping.Resource, info.Name))
			return nil
		}

		// Use APIVersion/Kind as grouping mechanism. I'm not sure if you can have multiple
		// versions per cluster, but this certainly won't hurt anything, so let's be safe.
		gvk := info.ResourceMapping().GroupVersionKind
		vk := gvk.Version + "/" + gvk.Kind

		// Initialize map. The main map groups resources based on version/kind
		// The second level is a simple 'Name' to 'Object', that will help sort
		// the individual resource later
		if objs[vk] == nil {
			objs[vk] = make(map[string]runtime.Object)
		}
		// Map between the resource name to the underlying info object
		objs[vk][info.Name] = resourceInfoToObject(info, c)

		//Get the relation pods
		objPods, err = c.getSelectRelationPod(info, objPods)
		if err != nil {
			c.Log("Warning: get the relation pod is failed, err:%s", err.Error())
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	//here, we will add the objPods to the objs
	for key, podItems := range objPods {
		for i := range podItems {
			pod := &core.Pod{}

			legacyscheme.Scheme.Convert(&podItems[i], pod, nil)
			if objs[key+"(related)"] == nil {
				objs[key+"(related)"] = make(map[string]runtime.Object)
			}
			objs[key+"(related)"][pod.ObjectMeta.Name] = runtime.Object(pod)
		}
	}

	// Ok, now we have all the objects grouped by types (say, by v1/Pod, v1/Service, etc.), so
	// spin through them and print them. Printer is cool since it prints the header only when
	// an object type changes, so we can just rely on that. Problem is it doesn't seem to keep
	// track of tab widths.
	buf := new(bytes.Buffer)
	printFlags := get.NewHumanPrintFlags()

	// Sort alphabetically by version/kind keys
	vkKeys := sortByKey(objs)
	// Iterate on sorted version/kind types
	for _, t := range vkKeys {
		if _, err = fmt.Fprintf(buf, "==> %s\n", t); err != nil {
			return "", err
		}
		typePrinter, _ := printFlags.ToPrinter("")

		var sortedResources []string
		for resource := range objs[t] {
			sortedResources = append(sortedResources, resource)
		}
		sort.Strings(sortedResources)

		// Now that each individual resource within the specific version/kind
		// is sorted, we print each resource using the k8s printer
		vk := objs[t]
		for _, resourceName := range sortedResources {
			if err := typePrinter.PrintObj(vk[resourceName], buf); err != nil {
				c.Log("failed to print object type %s, object: %q :\n %v", t, resourceName, err)
				return "", err
			}
		}
		if _, err := buf.WriteString("\n"); err != nil {
			return "", err
		}
	}
	if len(missing) > 0 {
		buf.WriteString(MissingGetHeader)
		for _, s := range missing {
			fmt.Fprintln(buf, s)
		}
	}
	return buf.String(), nil
}

// Deprecated; use UpdateWithOptions instead
func (c *Client) Update(namespace string, originalReader, targetReader io.Reader, force bool, recreate bool, timeout int64, shouldWait bool) error {
	return c.UpdateWithOptions(namespace, originalReader, targetReader, UpdateOptions{
		Force:      force,
		Recreate:   recreate,
		Timeout:    timeout,
		ShouldWait: shouldWait,
	})
}

// UpdateOptions provides options to control update behavior
type UpdateOptions struct {
	Force      bool
	Recreate   bool
	Timeout    int64
	ShouldWait bool
	// Allow deletion of new resources created in this update when update failed
	CleanupOnFail    bool
	ReleaseName      string
	UseThreeWayMerge bool
}

// UpdateWithOptions reads in the current configuration and a target configuration from io.reader
// and creates resources that don't already exists, updates resources that have been modified
// in the target configuration and deletes resources from the current configuration that are
// not present in the target configuration.
//
// Namespace will set the namespaces.
func (c *Client) UpdateWithOptions(namespace string, originalReader, targetReader io.Reader, opts UpdateOptions) error {
	LastClientWarnings = nil

	if opts.UseThreeWayMerge {
		opts.CleanupOnFail = false
	}

	original, err := c.BuildUnstructured(namespace, originalReader)
	if err != nil {
		return fmt.Errorf("failed decoding reader into objects: %s", err)
	}

	c.Log("building resources from updated manifest")
	target, err := c.BuildUnstructured(namespace, targetReader)
	if err != nil {
		return fmt.Errorf("failed decoding reader into objects: %s", err)
	}

	newlyCreatedResources := []*resource.Info{}
	updateErrors := []string{}
	adoptErrors := []string{}

	c.Log("checking %d resources for changes", len(target))
	err = target.Visit(func(target *resource.Info, err error) error {
		if err != nil {
			return err
		}

		helper := resource.NewHelper(target.Client, target.Mapping)
		currentObj, err := helper.Get(target.Namespace, target.Name, target.Export)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("Could not get information about the resource: %s", err)
			}

			validateAndSetWarnings(c, target.Object)

			// Since the resource does not exist, create it.
			if err := createResource(target, opts.ReleaseName); err != nil {
				return fmt.Errorf("failed to create resource: %s", err)
			}
			newlyCreatedResources = append(newlyCreatedResources, target)

			c.Log("Created a new %s called %q\n", target.Mapping.GroupVersionKind.Kind, target.Name)
			return nil
		}

		originalInfo := original.Get(target)

		// The resource already exists in the cluster, but it wasn't defined in the previous release.
		// In this case, we consider it to be a resource that was previously un-managed by the release and error out,
		// asking for the user to intervene.
		//
		// See https://github.com/helm/helm/issues/1193 for more info.
		if originalInfo == nil {
			adoptObjectAllowed := false
			ownerRelease := getObjectAnnotation(currentObj, ownerReleaseAnnotation)
			if opts.ReleaseName != "" && ownerRelease == opts.ReleaseName {
				adoptObjectAllowed = true
			}

			if !adoptObjectAllowed {
				return fmt.Errorf(
					"kind %s with the name %q already exists in the cluster and wasn't defined in the previous release. Before upgrading, please either delete the resource from the cluster or remove it from the chart or set \"%s\": \"%s\" annotation to the object, which will cause werf to adopt existing resource into the release",
					target.Mapping.GroupVersionKind.Kind,
					target.Name,
					ownerReleaseAnnotation,
					opts.ReleaseName,
				)
			}

			validateAndSetWarnings(c, target.Object)
			if err := adoptResource(c, target, currentObj, opts.ReleaseName); err != nil {
				c.Log("error adopting resource %s/%s:\n\t %v", target.Mapping.GroupVersionKind.Kind, target.Name, err)
				adoptErrors = append(adoptErrors, err.Error())
			}
		} else {
			validateAndSetWarnings(c, target.Object)
			if err := updateResource(c, target, currentObj, originalInfo.Object, opts.Force, opts.Recreate, opts.UseThreeWayMerge, opts.ReleaseName); err != nil {
				c.Log("error updating the resource %q:\n\t %v", target.Name, err)
				updateErrors = append(updateErrors, err.Error())
			}
		}

		return nil
	})

	cleanupErrors := []string{}

	if opts.CleanupOnFail && (err != nil || len(updateErrors) != 0 || len(adoptErrors) != 0) {
		c.Log("Cleanup on fail enabled: cleaning up newly created resources due to update manifests failures")
		cleanupErrors = c.cleanup(newlyCreatedResources)
	}

	switch {
	case err != nil:
		return fmt.Errorf(strings.Join(append([]string{err.Error()}, cleanupErrors...), " && "))
	case len(updateErrors)+len(adoptErrors) != 0:
		return fmt.Errorf(strings.Join(append(updateErrors, append(adoptErrors, cleanupErrors...)...), " && "))
	}

	for _, info := range original.Difference(target) {
		c.Log("Deleting %q in %s...", info.Name, info.Namespace)

		if err := info.Get(); err != nil {
			c.Log("Unable to get obj %q, err: %s", info.Name, err)
		}
		annotations, err := metadataAccessor.Annotations(info.Object)
		if err != nil {
			c.Log("Unable to get annotations on %q, err: %s", info.Name, err)
		}
		if ResourcePolicyIsKeep(annotations) {
			policy := annotations[ResourcePolicyAnno]
			c.Log("Skipping delete of %q due to annotation [%s=%s]", info.Name, ResourcePolicyAnno, policy)
			continue
		}

		if err := deleteResource(info); err != nil {
			c.Log("Failed to delete %q, err: %s", info.Name, err)
		}
	}
	if opts.ShouldWait {
		err := c.waitForResources(time.Duration(opts.Timeout)*time.Second, target)

		if opts.CleanupOnFail && err != nil {
			c.Log("Cleanup on fail enabled: cleaning up newly created resources due to wait failure during update")
			cleanupErrors = c.cleanup(newlyCreatedResources)
			return fmt.Errorf(strings.Join(append([]string{err.Error()}, cleanupErrors...), " && "))
		}

		return err
	}
	return nil
}

func (c *Client) cleanup(newlyCreatedResources []*resource.Info) (cleanupErrors []string) {
	for _, info := range newlyCreatedResources {
		kind := info.Mapping.GroupVersionKind.Kind
		c.Log("Deleting newly created %s with the name %q in %s...", kind, info.Name, info.Namespace)
		if err := deleteResource(info); err != nil {
			c.Log("Error deleting newly created %s with the name %q in %s: %s", kind, info.Name, info.Namespace, err)
			cleanupErrors = append(cleanupErrors, err.Error())
		}
	}
	return
}

// Delete deletes Kubernetes resources from an io.reader.
//
// Namespace will set the namespace.
func (c *Client) Delete(namespace string, reader io.Reader) error {
	return c.DeleteWithTimeout(namespace, reader, 0, false)
}

// DeleteWithTimeout deletes Kubernetes resources from an io.reader. If shouldWait is true, the function
// will wait for all resources to be deleted from etcd before returning, or when the timeout
// has expired.
//
// Namespace will set the namespace.
func (c *Client) DeleteWithTimeout(namespace string, reader io.Reader, timeout int64, shouldWait bool) error {
	infos, err := c.BuildUnstructured(namespace, reader)
	if err != nil {
		return err
	}
	err = perform(infos, func(info *resource.Info) error {
		c.Log("Starting delete for %q %s", info.Name, info.Mapping.GroupVersionKind.Kind)
		err := deleteResource(info)
		return c.skipIfNotFound(err)
	})
	if err != nil {
		return err
	}

	if shouldWait {
		c.Log("Waiting for %d seconds for delete to be completed", timeout)
		return waitUntilAllResourceDeleted(infos, time.Duration(timeout)*time.Second)
	}

	return nil
}

func (c *Client) skipIfNotFound(err error) error {
	if errors.IsNotFound(err) {
		c.Log("%v", err)
		return nil
	}
	return err
}

func waitUntilAllResourceDeleted(infos Result, timeout time.Duration) error {
	return wait.Poll(2*time.Second, timeout, func() (bool, error) {
		allDeleted := true
		err := perform(infos, func(info *resource.Info) error {
			innerErr := info.Get()
			if errors.IsNotFound(innerErr) {
				return nil
			}
			if innerErr != nil {
				return innerErr
			}
			allDeleted = false
			return nil
		})
		if err != nil {
			return false, err
		}
		return allDeleted, nil
	})
}

func (c *Client) watchTimeout(t time.Duration) ResourceActorFunc {
	return func(info *resource.Info) error {
		return c.watchUntilReady(t, info)
	}
}

// WatchUntilReady watches the resource given in the reader, and waits until it is ready.
//
// This function is mainly for hook implementations. It watches for a resource to
// hit a particular milestone. The milestone depends on the Kind.
//
// For most kinds, it checks to see if the resource is marked as Added or Modified
// by the Kubernetes event stream. For some kinds, it does more:
//
// - Jobs: A job is marked "Ready" when it has successfully completed. This is
//   ascertained by watching the Status fields in a job's output.
//
// Handling for other kinds will be added as necessary.
func (c *Client) WatchUntilReady(namespace string, reader io.Reader, timeout int64, shouldWait bool) error {
	if c.ResourcesWaiter != nil {
		return c.ResourcesWaiter.WatchUntilReady(namespace, reader, time.Duration(timeout)*time.Second)
	}

	infos, err := c.BuildUnstructured(namespace, reader)
	if err != nil {
		return err
	}
	// For jobs, there's also the option to do poll c.Jobs(namespace).Get():
	// https://github.com/adamreese/kubernetes/blob/master/test/e2e/job.go#L291-L300
	return perform(infos, c.watchTimeout(time.Duration(timeout)*time.Second))
}

// WatchUntilCRDEstablished polls the given CRD until it reaches the established
// state. A CRD needs to reach the established state before CRs can be created.
//
// If a naming conflict condition is found, this function will return an error.
func (c *Client) WaitUntilCRDEstablished(reader io.Reader, timeout time.Duration) error {
	infos, err := c.BuildUnstructured(metav1.NamespaceAll, reader)
	if err != nil {
		return err
	}

	return perform(infos, c.pollCRDEstablished(timeout))
}

func (c *Client) pollCRDEstablished(t time.Duration) ResourceActorFunc {
	return func(info *resource.Info) error {
		return c.pollCRDUntilEstablished(t, info)
	}
}

func (c *Client) pollCRDUntilEstablished(timeout time.Duration, info *resource.Info) error {
	return wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		err := info.Get()
		if err != nil {
			return false, fmt.Errorf("unable to get CRD: %v", err)
		}

		crd := &apiextv1beta1.CustomResourceDefinition{}
		err = scheme.Scheme.Convert(info.Object, crd, nil)
		if err != nil {
			return false, fmt.Errorf("unable to convert to CRD type: %v", err)
		}

		for _, cond := range crd.Status.Conditions {
			switch cond.Type {
			case apiextv1beta1.Established:
				if cond.Status == apiextv1beta1.ConditionTrue {
					return true, nil
				}
			case apiextv1beta1.NamesAccepted:
				if cond.Status == apiextv1beta1.ConditionFalse {
					return false, fmt.Errorf("naming conflict detected for CRD %s", crd.GetName())
				}
			}
		}

		return false, nil
	})
}

func perform(infos Result, fn ResourceActorFunc) error {
	if len(infos) == 0 {
		return ErrNoObjectsVisited
	}

	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

func filter(infos Result, fn func(*resource.Info) (bool, error)) (Result, error) {
	if len(infos) == 0 {
		return nil, ErrNoObjectsVisited
	}

	var newInfos Result

	for i := range infos {
		keep, err := fn(infos[i])
		if err != nil {
			return nil, err
		} else if keep {
			newInfos = append(newInfos, infos[i])
		}
	}

	return newInfos, nil
}

func setObjectAnnotation(obj runtime.Object, annoName, annoValue string) error {
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		return err
	}

	if annots == nil {
		annots = map[string]string{}
	}

	annots[annoName] = annoValue

	kind, err := metadataAccessor.Kind(obj)
	if err != nil {
		return err
	}

	name, err := metadataAccessor.Name(obj)
	if err != nil {
		return err
	}

	if debugUpdateResource() {
		logboek.LogInfoF("Set annotation %s=%s for %s named %q\n", annoName, annoValue, kind, name)
	}

	return metadataAccessor.SetAnnotations(obj, annots)
}

func getObjectAnnotation(obj runtime.Object, annoName string) string {
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		logboek.LogErrorF("Unable to fetch annotations of kube object: %s\n", err)
		return ""
	}
	return annots[annoName]
}

func deleteObjectAnnotaion(obj runtime.Object, annoName string) {
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		logboek.LogErrorF("Unable to fetch annotations of kube object: %s\n", err)
		return
	}
	delete(annots, annoName)
}

func createResource(info *resource.Info, releaseName string) error {
	if err := setObjectAnnotation(info.Object, ownerReleaseAnnotation, releaseName); err != nil {
		return fmt.Errorf("unable to set %s=%s annotation: %s", ownerReleaseAnnotation, releaseName, err)
	}

	obj, err := resource.NewHelper(info.Client, info.Mapping).Create(info.Namespace, true, info.Object, nil)
	if err != nil {
		return err
	}
	return info.Refresh(obj, true)
}

func deleteResource(info *resource.Info) error {
	// FIXME: do not delete resources of FAILED release which does not belong to the release
	policy := metav1.DeletePropagationBackground
	opts := &metav1.DeleteOptions{PropagationPolicy: &policy}
	_, err := resource.NewHelper(info.Client, info.Mapping).DeleteWithOptions(info.Namespace, info.Name, opts)
	return err
}

func adoptResource(c *Client, target *resource.Info, currentObj runtime.Object, releaseName string) error {
	if err := setObjectAnnotation(target.Object, ownerReleaseAnnotation, releaseName); err != nil {
		return fmt.Errorf("unable to set %s=%s annotation: %s", ownerReleaseAnnotation, releaseName, err)
	}

	return updateResource(c, target, currentObj, target.Object, false, false, true, releaseName)
}

func applyPatchForData(target *resource.Info, oldData, patch []byte) ([]byte, error) {
	if len(patch) == 0 || string(patch) == "{}" {
		return oldData, nil
	}

	// Get a versioned object
	versionedObject, err := asVersioned(target)

	// Unstructured objects, such as CRDs, may not have an not registered error
	// returned from ConvertToVersion. Anything that's unstructured should
	// use the jsonpatch.CreateMergePatch. Strategic Merge Patch is not supported
	// on objects like CRDs.
	_, isUnstructured := versionedObject.(runtime.Unstructured)

	// On newer K8s versions, CRDs aren't unstructured but has this dedicated type
	_, isCRD := versionedObject.(*apiextv1beta1.CustomResourceDefinition)

	switch {
	case runtime.IsNotRegisteredError(err), isUnstructured, isCRD:
		// fall back to generic JSON merge patch
		res, err := jsonpatch.MergePatch(oldData, patch)
		if err != nil {
			return nil, fmt.Errorf("failed to merge patch: %s", err)
		}
		return res, nil
	case err != nil:
		return nil, fmt.Errorf("failed to get versionedObject: %s", err)
	default:
		var oldDataMap strategicpatch.JSONMap
		if err := json.Unmarshal(oldData, &oldDataMap); err != nil {
			return nil, fmt.Errorf("unable to unmarshal json data: %s: %s", oldData, err)
		}
		//fmt.Printf("!!! oldDataMap: %#v\n", oldDataMap)

		var patchMap strategicpatch.JSONMap
		if err := json.Unmarshal(patch, &patchMap); err != nil {
			return nil, fmt.Errorf("unable to unmarshal json patch: %s: %s", patch, err)
		}
		//fmt.Printf("!!! patchMap: %#v\n", patchMap)

		//schema, err := strategicpatch.NewPatchMetaFromStruct(versionedObject)
		//if err != nil {
		//	return nil, err
		//}

		//resMap, err := strategicpatch.MergeStrategicMergeMapPatchUsingLookupPatchMeta(schema, oldDataMap, patchMap)
		resMap, err := strategicpatch.StrategicMergeMapPatch(oldDataMap, patchMap, versionedObject)
		if err != nil {
			return nil, fmt.Errorf("failed to apply strategic merge patch: %s", err)
		}
		//res1, _ := json.Marshal(resMap)
		//fmt.Printf("!!! res1:\n%s\n", res1)

		//resMap2, err := strategicpatch.StrategicMergeMapPatch(oldDataMap, patchMap, versionedObject)
		//if err != nil {
		//	return nil, fmt.Errorf("failed to apply strategic merge patch: %s", err)
		//}
		//res2, _ := json.Marshal(resMap2)
		//fmt.Printf("!!! res2:\n%s\n", res2)

		res, err := json.Marshal(resMap)
		if err != nil {
			return nil, fmt.Errorf("unable to marshal data map: %v: %s", resMap, err)
		}

		return res, nil
	}
}

func createPatch(target *resource.Info, current runtime.Object) ([]byte, types.PatchType, error) {
	oldData, err := json.Marshal(current)
	if err != nil {
		return nil, types.StrategicMergePatchType, fmt.Errorf("serializing current configuration: %s", err)
	}

	newData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, types.StrategicMergePatchType, fmt.Errorf("serializing target configuration: %s", err)
	}

	// While different objects need different merge types, the parent function
	// that calls this does not try to create a patch when the data (first
	// returned object) is nil. We can skip calculating the merge type as
	// the returned merge type is ignored.
	if apiequality.Semantic.DeepEqual(oldData, newData) {
		return nil, types.StrategicMergePatchType, nil
	}

	// Get a versioned object
	versionedObject, err := asVersioned(target)

	// Unstructured objects, such as CRDs, may not have an not registered error
	// returned from ConvertToVersion. Anything that's unstructured should
	// use the jsonpatch.CreateMergePatch. Strategic Merge Patch is not supported
	// on objects like CRDs.
	_, isUnstructured := versionedObject.(runtime.Unstructured)

	// On newer K8s versions, CRDs aren't unstructured but has this dedicated type
	_, isCRD := versionedObject.(*apiextv1beta1.CustomResourceDefinition)

	switch {
	case runtime.IsNotRegisteredError(err), isUnstructured, isCRD:
		// fall back to generic JSON merge patch
		patch, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return nil, types.MergePatchType, fmt.Errorf("failed to create merge patch: %v", err)
		}
		return patch, types.MergePatchType, nil
	case err != nil:
		return nil, types.StrategicMergePatchType, fmt.Errorf("failed to get versionedObject: %s", err)
	default:
		patch, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, versionedObject)
		if err != nil {
			return nil, types.StrategicMergePatchType, fmt.Errorf("failed to create two-way merge patch: %v", err)
		}
		return patch, types.StrategicMergePatchType, nil
	}
}

func debugUpdateResource() bool {
	return os.Getenv("WERF_DEBUG_UPDATE_RESOURCE") == "1"
}

func createFinalThreeWayMergePatch(c *Client, target *resource.Info, currentObj, originalObj runtime.Object) ([]byte, types.PatchType, error) {
	setObjectAnnotation(originalObj, repairPatchAnnotation, getObjectAnnotation(currentObj, repairPatchAnnotation))
	setObjectAnnotation(originalObj, repairPatchErrorsAnnotation, getObjectAnnotation(currentObj, repairPatchErrorsAnnotation))
	setObjectAnnotation(originalObj, repairMessagesAnnotation, getObjectAnnotation(currentObj, repairMessagesAnnotation))

	deleteObjectAnnotaion(target.Object, repairPatchAnnotation)
	deleteObjectAnnotaion(target.Object, repairPatchErrorsAnnotation)
	deleteObjectAnnotaion(target.Object, repairMessagesAnnotation)

	setReplicasOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetReplicasOnlyOnCreationAnnotation)
	setResourcesOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetResourcesOnlyOnCreationAnnotation)

	isReplicasOnlyOnCreation := setReplicasOnlyOnCreationAnnoValue == "true"
	isResourcesOnlyOnCreation := setResourcesOnlyOnCreationAnnoValue == "true"

	currentData, err := json.Marshal(currentObj)
	if err != nil {
		return nil, "", fmt.Errorf("serializing current configuration: %s", err)
	}
	filteredCurrentData, err := filterManifestForRepairPatch(currentData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{FilterVolumesDownwardApi: true})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter current manifest: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- currentData:\n%s\n", currentData)
		fmt.Printf("-- filteredCurrentData:\n%s\n", filteredCurrentData)
	}

	targetData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, "", fmt.Errorf("serializing target configuration: %s", err)
	}
	filteredTargetData, err := filterManifestForRepairPatch(targetData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter target manifest: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- targetData:\n%s\n", targetData)
		fmt.Printf("-- filteredTargetData:\n%s\n", filteredTargetData)
	}

	originalData, err := json.Marshal(originalObj)
	if err != nil {
		return nil, "", fmt.Errorf("serializing original configuration: %s", err)
	}
	filteredOriginalData, err := filterManifestForRepairPatch(originalData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter original manifest: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- originalData:\n%s\n", originalData)
		fmt.Printf("-- filteredOriginalData:\n%s\n", filteredOriginalData)
	}

	firstStagePatch, _, err := createThreeWayMergePatch(target, filteredOriginalData, filteredTargetData, filteredCurrentData)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create first stage three-way-merge patch: %s", err)
	}
	filteredFirstStagePatch, err := filterRepairPatch(firstStagePatch)
	if err != nil {
		return nil, "", fmt.Errorf("failed to filter first stage three-way-merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- firstStagePatch:\n%s\n", firstStagePatch)
		fmt.Printf("-- filteredFirstStagePatch:\n%s\n", filteredFirstStagePatch)
	}

	currentDataAfterFirstStagePatch, err := applyPatchForData(target, filteredCurrentData, filteredFirstStagePatch)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct new current state using first stage repair three-way-merge patch: %s", err)
	}
	validatedCurrentDataAfterFirstStagePatch, err := makeValidatedManifest(target, currentDataAfterFirstStagePatch)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct validated manifest for current state after first stage repair three-way-merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- currentDataAfterFirstStagePatch:\n%s\n", currentDataAfterFirstStagePatch)
		fmt.Printf("-- validatedCurrentDataAfterFirstStagePatch:\n%s\n", validatedCurrentDataAfterFirstStagePatch)
	}

	finalPatch, patchType, err := createThreeWayMergePatch(target, filteredCurrentData, validatedCurrentDataAfterFirstStagePatch, filteredCurrentData)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create final three-way-merge patch: %s", err)
	}
	filteredFinalPatch, err := filterRepairPatch(finalPatch)
	if err != nil {
		return nil, "", fmt.Errorf("failed to filter final three-way-merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- finalPatch:\n%s\n", finalPatch)
		fmt.Printf("-- filteredFinalPatch:\n%s\n", filteredFinalPatch)
		fmt.Printf("-- finalPatchType: %s\n", patchType)
	}

	return filteredFinalPatch, patchType, nil
}

func createRepairPatch(c *Client, target *resource.Info, currentObj, originalObj runtime.Object) ([]byte, types.PatchType, error) {
	setReplicasOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetReplicasOnlyOnCreationAnnotation)
	setResourcesOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetResourcesOnlyOnCreationAnnotation)

	isReplicasOnlyOnCreation := setReplicasOnlyOnCreationAnnoValue == "true"
	isResourcesOnlyOnCreation := setResourcesOnlyOnCreationAnnoValue == "true"

	currentData, err := json.Marshal(currentObj)
	if err != nil {
		return nil, "", fmt.Errorf("serializing current configuration: %s", err)
	}
	filteredCurrentData, err := filterManifestForRepairPatch(currentData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{FilterVolumesDownwardApi: true})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter current manifest: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- currentData:\n%s\n", currentData)
		fmt.Printf("-- filteredCurrentData:\n%s\n", filteredCurrentData)
	}

	targetData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, "", fmt.Errorf("serializing target configuration: %s", err)
	}
	filteredTargetData, err := filterManifestForRepairPatch(targetData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter target manifest: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- targetData:\n%s\n", targetData)
		fmt.Printf("-- filteredTargetData:\n%s\n", filteredTargetData)
	}

	helmApplyPatch, _, err := createPatch(target, originalObj)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create helm-apply two-way merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- helmApplyPatch (2 way: previous chart version -> current chart version):\n%s\n", helmApplyPatch)
	}

	if debugUpdateResource() {
		originalData, err := json.Marshal(originalObj)
		if err != nil {
			return nil, "", fmt.Errorf("serializing original configuration: %s", err)
		}
		threeWayPatch, _, err := createThreeWayMergePatch(target, originalData, targetData, currentData)
		fmt.Printf("-- threeWayPatch (default kubectl-apply patch) (err=%s):\n%s\n", err, threeWayPatch)
	}

	currentDataAfterHelmApply, err := applyPatchForData(target, filteredCurrentData, helmApplyPatch)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct new current state using helm-apply two-way merge patch: %s", err)
	}
	// Filter is needed because helmApplyPatch was created as in helm without filters to target and original
	filteredCurrentDataAfterHelmApply, err := filterManifestForRepairPatch(currentDataAfterHelmApply, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation, filterManifestOptions{FilterVolumesDownwardApi: true})
	if err != nil {
		return nil, "", fmt.Errorf("unable to filter current manifest after helm apply: %s", err)
	}
	validatedFilteredCurrentDataAfterHelmApply, err := makeValidatedManifest(target, filteredCurrentDataAfterHelmApply)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct validated manifest for current state after helm-apply two-way merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- currentDataAfterHelmApply:\n%s\n", currentDataAfterHelmApply)
		fmt.Printf("-- filteredCurrentDataAfterHelmApply:\n%s\n", filteredCurrentDataAfterHelmApply)
		fmt.Printf("-- validatedFilteredCurrentDataAfterHelmApply:\n%s\n", validatedFilteredCurrentDataAfterHelmApply)
	}

	firstStageRepairPatch, _, err := createThreeWayMergePatch(target, filteredTargetData, filteredTargetData, filteredCurrentDataAfterHelmApply)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create first stage repair three-way-merge patch: %s", err)
	}
	filteredFirstStageRepairPatch, err := filterRepairPatch(firstStageRepairPatch)
	if err != nil {
		return nil, "", fmt.Errorf("failed to filter first stage repair patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- firstStageRepairPatch:\n%s\n", firstStageRepairPatch)
		fmt.Printf("-- filteredFirstStageRepairPatch:\n%s\n", filteredFirstStageRepairPatch)
	}

	currentDataAfterFirstStageRepair, err := applyPatchForData(target, filteredCurrentDataAfterHelmApply, filteredFirstStageRepairPatch)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct new current state using first stage repair three-way-merge patch: %s", err)
	}
	validatedCurrentDataAfterFirstStageRepair, err := makeValidatedManifest(target, currentDataAfterFirstStageRepair)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct validated manifest for current state after first stage repair three-way-merge patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- currentDataAfterFirstStageRepair:\n%s\n", currentDataAfterFirstStageRepair)
		fmt.Printf("-- validatedCurrentDataAfterFirstStageRepair:\n%s\n", validatedCurrentDataAfterFirstStageRepair)
	}

	repairPatch, repairPatchType, err := createThreeWayMergePatch(target, validatedFilteredCurrentDataAfterHelmApply, validatedCurrentDataAfterFirstStageRepair, validatedFilteredCurrentDataAfterHelmApply)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create second stage repair three-way-merge patch: %s", err)
	}
	filteredRepairPatch, err := filterRepairPatch(repairPatch)
	if err != nil {
		return nil, "", fmt.Errorf("failed to filter repair patch: %s", err)
	}
	if debugUpdateResource() {
		fmt.Printf("-- repairPatch:\n%s\n", repairPatch)
		fmt.Printf("-- filteredRepairPatch:\n%s\n", filteredRepairPatch)
	}

	return filteredRepairPatch, repairPatchType, nil
}

func makeValidatedManifest(target *resource.Info, manifest []byte) ([]byte, error) {
	var dataMap map[string]interface{}
	if err := json.Unmarshal(manifest, &dataMap); err != nil {
		return nil, fmt.Errorf("unable to unmarshal manifest json: %s", err)
	}

	// Create the versioned struct from the type defined in the restmapping
	// (which is the API version we'll be submitting the patch to)
	versionedObject, err := asVersioned(target)
	if err != nil {
		return manifest, nil
	}

	newObj := versionedObject.DeepCopyObject()

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(dataMap, newObj); err != nil {
		return nil, fmt.Errorf("object creation from unstructured manifest failed: %s", err)
	}

	newManifest, err := json.Marshal(newObj)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal new manifest json: %s", err)
	}

	return newManifest, nil
}

func validateManifest(c *Client, manifest []byte) error {
	schema, err := c.Validator(true)
	if err != nil {
		return fmt.Errorf("unable to create validator: %s", err)
	}

	if err := schema.ValidateBytes(manifest); err != nil {
		var dataMap map[string]interface{}
		if err := json.Unmarshal(manifest, &dataMap); err != nil {
			return fmt.Errorf("unable to unmarshal manifest json: %s", err)
		}

		kind := ""
		name := ""

		if value, ok := dataMap["kind"].(string); ok {
			kind = strings.ToLower(value)
		}
		if metadata := getMapByKey(dataMap, "metadata"); metadata != nil {
			if value, ok := metadata["name"].(string); ok {
				name = value
			}
		}

		return fmt.Errorf("%s/%s: %s", kind, name, err)
	}

	return nil
}

func getMapByKey(dataMap map[string]interface{}, key string) map[string]interface{} {
	if valueI, hasKey := dataMap[key]; hasKey {
		if value, ok := valueI.(map[string]interface{}); ok {
			return value
		}
	}
	return nil
}

func getSliceByKey(dataMap map[string]interface{}, key string) []interface{} {
	if valueI, hasKey := dataMap[key]; hasKey {
		if value, ok := valueI.([]interface{}); ok {
			return value
		}
	}
	return nil
}

func getMapByIndex(dataSlice []interface{}, index int) map[string]interface{} {
	if index < len(dataSlice) && index >= 0 {
		valueI := dataSlice[index]
		if value, ok := valueI.(map[string]interface{}); ok {
			return value
		}
	}
	return nil
}

type filterManifestOptions struct {
	FilterVolumesDownwardApi bool
}

func filterManifestForRepairPatch(manifest []byte, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation bool, opts filterManifestOptions) ([]byte, error) {
	var dataMap map[string]interface{}
	if err := json.Unmarshal(manifest, &dataMap); err != nil {
		return nil, fmt.Errorf("unable to unmarshal manifest json: %s", err)
	}

	if spec := getMapByKey(dataMap, "spec"); spec != nil {
		if template := getMapByKey(spec, "template"); template != nil {
			if spec := getMapByKey(template, "spec"); spec != nil {
				if containers := getSliceByKey(spec, "containers"); containers != nil {
					for i := range containers {
						if container := getMapByIndex(containers, i); container != nil {
							// Remove empty (null or "") container env "value" fields
							if env := getSliceByKey(container, "env"); env != nil {
								for i := range env {
									if envElem := getMapByIndex(env, i); envElem != nil {
										valueI := envElem["value"]
										if valueI == nil {
											delete(envElem, "value")
										} else {
											valueStr := fmt.Sprintf("%v", valueI)
											if valueStr == "" {
												delete(envElem, "value")
											}
										}
									}
								}
							} // env

							// Normalize resources quantity values
							if resources := getMapByKey(container, "resources"); resources != nil {
								for _, resourcesGroupName := range []string{"limits", "requests"} {
									if settings := getMapByKey(resources, resourcesGroupName); settings != nil {
										for _, resourceName := range []string{"cpu", "memory", "storage", "ephemeral-storage"} {
											if rawQuantityI, hasKey := settings[resourceName]; hasKey {
												rawQuantityStr := fmt.Sprintf("%v", rawQuantityI)
												if q, err := resource_quantity.ParseQuantity(rawQuantityStr); err == nil {
													settings[resourceName] = q.String()
												}
											}
										}
									}
								}
							} // resources
						}
					}
				} // containers

				if opts.FilterVolumesDownwardApi {
					if volumes := getSliceByKey(spec, "volumes"); volumes != nil {
						for i := range volumes {
							if volume := getMapByIndex(volumes, i); volume != nil {
								if downwardAPI := getMapByKey(volume, "downwardAPI"); downwardAPI != nil {
									if items := getSliceByKey(downwardAPI, "items"); items != nil {
										for i := range items {
											if item := getMapByIndex(items, i); item != nil {
												if fieldRef := getMapByKey(item, "fieldRef"); fieldRef != nil {
													if _, hasKey := fieldRef["apiVersion"]; hasKey {
														delete(fieldRef, "apiVersion")
														RepairDebugMessages = append(RepairDebugMessages, fmt.Sprintf("deleted volumes[].downwardApi.items[].apiVersion for manifest"))
													}
												}
											}
										}
									}
								}
							}
						}
					} // volumes
				}
			}
		}

		// Remove "volumeClaimTemplates" because it is forbidden to change this field in a patch
		if _, hasKey := spec["volumeClaimTemplates"]; hasKey {
			delete(spec, "volumeClaimTemplates")
		}
	}

	dataMap = processResourceReplicasAndResources(dataMap, func(node map[string]interface{}, field string) {
		if field == "replicas" && isReplicasOnlyOnCreation || field == "resources" && isResourcesOnlyOnCreation {
			delete(node, field)
		}
	})

	newManifest, err := json.Marshal(dataMap)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal new manifest json: %s", err)
	}

	return newManifest, nil
}

func filterRepairPatch(patch []byte) ([]byte, error) {
	var dataMap map[string]interface{}
	if err := json.Unmarshal(patch, &dataMap); err != nil {
		return nil, fmt.Errorf("unable to unmarshal manifest json: %s", err)
	}

	if spec := getMapByKey(dataMap, "spec"); spec != nil {
		if strategy := getMapByKey(spec, "strategy"); strategy != nil {
			if rollingUpdateI, hasKey := strategy["rollingUpdate"]; hasKey {
				if rollingUpdateI == nil {
					delete(strategy, "rollingUpdate")
					delete(strategy, "$retainKeys")

					if debugUpdateResource() {
						fmt.Printf("-- Deleted rollingUpdate and $retainKeys for patch:\n%s\n", patch)
					}
					RepairDebugMessages = append(RepairDebugMessages, fmt.Sprintf("deleted rollingUpdate and $retainKeys for patch"))
				}
			}

			if len(strategy) == 0 {
				delete(spec, "strategy")
			}
		}

		if len(spec) == 0 {
			delete(dataMap, "spec")
		}
	}

	type queueElem struct {
		Key    string
		ValueI interface{}
		MapRef map[string]interface{}
	}
	var queue []queueElem
	for k, v := range dataMap {
		queue = append(queue, queueElem{k, v, dataMap})
	}
	for len(queue) > 0 {
		elem := queue[0]
		queue = queue[1:]

		if strings.HasPrefix(elem.Key, "$setElementOrder") || strings.HasPrefix(elem.Key, "$retainKeys") {
			delete(elem.MapRef, elem.Key)

			if debugUpdateResource() {
				fmt.Printf("-- Deleted %s for patch\n", elem.Key)
			}
			RepairDebugMessages = append(RepairDebugMessages, fmt.Sprintf("deleted %s for patch", elem.Key))

			continue
		}

		switch value := elem.ValueI.(type) {
		case map[string]interface{}:
			for k, v := range value {
				queue = append(queue, queueElem{k, v, value})
			}
		case []interface{}:
			for _, sliceValueI := range value {
				if sliceValue, ok := sliceValueI.(map[string]interface{}); ok {
					for k, v := range sliceValue {
						queue = append(queue, queueElem{k, v, sliceValue})
					}
				}
			}
		}
	}

	newPatch, err := json.Marshal(dataMap)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal new manifest json: %s", err)
	}

	return newPatch, nil
}

func createThreeWayMergePatch(target *resource.Info, original, modified, current []byte) ([]byte, types.PatchType, error) {
	var patchType types.PatchType
	var patch []byte
	var lookupPatchMeta strategicpatch.LookupPatchMeta
	createPatchErrFormat := "creating patch with:\noriginal:\n%s\nmodified:\n%s\ncurrent:\n%s\nfor:"

	// Create the versioned struct from the type defined in the restmapping
	// (which is the API version we'll be submitting the patch to)
	versionedObject, err := asVersioned(target)
	_, isUnstructured := versionedObject.(runtime.Unstructured)
	_, isCRD := versionedObject.(*apiextv1beta1.CustomResourceDefinition)

	switch {
	case runtime.IsNotRegisteredError(err), isUnstructured, isCRD:
		// fall back to generic JSON merge patch
		patchType = types.MergePatchType

		preconditions := []mergepatch.PreconditionFunc{mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"), mergepatch.RequireMetadataKeyUnchanged("name")}

		// fall back to generic JSON merge patch
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(original, modified, current, preconditions...)
		if err != nil {
			if mergepatch.IsPreconditionFailed(err) {
				return nil, "", fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
			}
			return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, original, modified, current), target.Source, err)
		}
	case err != nil:
		return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf("getting instance of versioned object for %v:", target.Mapping.GroupVersionKind), target.Source, err)
	default:
		patchType = types.StrategicMergePatchType

		lookupPatchMeta, err = strategicpatch.NewPatchMetaFromStruct(versionedObject)
		if err != nil {
			return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, original, modified, current), target.Source, err)
		}

		patch, err = strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, true)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create two-way merge patch: %v", err)
		}
	}

	return patch, patchType, nil
}

func validateAndSetWarnings(c *Client, obj runtime.Object) {
	if targetManifest, err := json.Marshal(obj); err == nil {
		if err := validateManifest(c, targetManifest); err != nil {
			msg := fmt.Sprintf("Validation of target data failed: %s", err)
			LastClientWarnings = append(LastClientWarnings, msg)
			_ = setObjectAnnotation(obj, validationWarningsAnnotation, msg)
		}
	}
}

func updateResource(c *Client, target *resource.Info, currentObj, originalObj runtime.Object, force bool, recreate bool, useThreeWayMerge bool, releaseName string) error {
	if useThreeWayMerge {
		patch, patchType, err := createFinalThreeWayMergePatch(c, target, currentObj, originalObj)
		if err != nil {
			return fmt.Errorf("failed to create three-way-merge patch: %s", err)
		}

		if patch != nil && string(patch) != "{}" {
			if err := sendPatchToServerAndUpdateTarget(c, target, patch, patchType, force, releaseName); err != nil {
				return err
			}
		}
	} else {
		repairPatchData, _, err := createRepairPatch(c, target, currentObj, originalObj)
		if err != nil {
			LastClientWarnings = append(LastClientWarnings, fmt.Sprintf("Unable to create repair patch: %s", err))
			_ = setObjectAnnotation(target.Object, repairPatchErrorsAnnotation, err.Error())
		} else {
			repairMessages := make([]string, 0)

			isRepairPatchEmpty := len(repairPatchData) == 0 || string(repairPatchData) == "{}"
			if !isRepairPatchEmpty {
				// main repair instruction parts
				mripart1 := fmt.Sprintf("%s named %s state is inconsistent with chart configuration state!", target.Mapping.GroupVersionKind.Kind, target.Name)
				mripart2 := fmt.Sprintf("Repair patch has been written to the %s annotation of %s named %s", repairPatchAnnotation, target.Mapping.GroupVersionKind.Kind, target.Name)
				mripart3 := "Execute the following command manually to repair resource state:"
				mripart4 := fmt.Sprintf("kubectl -n %s patch %s %s -p '%s'", target.Namespace, target.Mapping.GroupVersionKind.Kind, target.Name, repairPatchData)

				repairMessages = append(repairMessages, strings.Join([]string{
					mripart1,
				}, "\n"))

				repairPatchWarnings, err := checkRepairPatchForWarnings(repairPatchData)
				if err != nil {
					LastClientWarnings = append(LastClientWarnings, fmt.Sprintf("Repair patch warnings check failed: %s", err))
					_ = setObjectAnnotation(target.Object, repairPatchErrorsAnnotation, err.Error())
				}
				repairMessages = append(repairMessages, repairPatchWarnings...)

				repairMessages = append(repairMessages, strings.Join([]string{
					mripart2,
					mripart3,
					mripart4,
				}, "\n"))

				repairMessages = append(repairMessages, RepairDebugMessages...)
				RepairDebugMessages = []string{}

				defer func() {
					LastClientWarnings = append(LastClientWarnings, mripart1)

					for _, msg := range repairPatchWarnings {
						LastClientWarnings = append(LastClientWarnings, msg)
					}

					LastClientWarnings = append(LastClientWarnings, mripart2)
					LastClientWarnings = append(LastClientWarnings, mripart3)
					// FIXME: mripart4 should be printf-ed
					LastClientWarnings = append(LastClientWarnings, mripart4)
				}()
			}

			repairMessagesJsonData, _ := json.Marshal(repairMessages)
			_ = setObjectAnnotation(target.Object, repairMessagesAnnotation, string(repairMessagesJsonData))
			_ = setObjectAnnotation(target.Object, repairPatchAnnotation, string(repairPatchData))
		}

		patch, patchType, err := createPatch(target, originalObj)
		if err != nil {
			return fmt.Errorf("failed to create patch: %s", err)
		}

		if debugUpdateResource() {
			fmt.Printf("-- helm patch:\n%s\n", patch)
		}

		if patch == nil {
			c.Log("Looks like there are no changes for %s %q", target.Mapping.GroupVersionKind.Kind, target.Name)
			// This needs to happen to make sure that tiller has the latest info from the API
			// Otherwise there will be no labels and other functions that use labels will panic
			if err := target.Get(); err != nil {
				return fmt.Errorf("error trying to refresh resource information: %v", err)
			}
		} else {
			if err := sendPatchToServerAndUpdateTarget(c, target, patch, patchType, force, releaseName); err != nil {
				return err
			}
		}
	}

	if !recreate {
		return nil
	}

	versioned := asVersionedOrUnstructured(target)
	selector, ok := getSelectorFromObject(versioned)
	if !ok {
		return nil
	}

	client, err := c.KubernetesClientSet()
	if err != nil {
		return err
	}

	pods, err := client.CoreV1().Pods(target.Namespace).List(metav1.ListOptions{
		LabelSelector: labels.Set(selector).AsSelector().String(),
	})
	if err != nil {
		return err
	}

	// Restart pods
	for _, pod := range pods.Items {
		c.Log("Restarting pod: %v/%v", pod.Namespace, pod.Name)

		// Delete each pod for get them restarted with changed spec.
		if err := client.CoreV1().Pods(pod.Namespace).Delete(pod.Name, metav1.NewPreconditionDeleteOptions(string(pod.UID))); err != nil {
			return err
		}
	}
	return nil
}

func sendPatchToServerAndUpdateTarget(c *Client, target *resource.Info, patch []byte, patchType types.PatchType, force bool, releaseName string) error {
	// send patch to server
	helper := resource.NewHelper(target.Client, target.Mapping)

	obj, err := helper.Patch(target.Namespace, target.Name, patchType, patch, nil)
	if err != nil {
		kind := target.Mapping.GroupVersionKind.Kind
		log.Printf("Cannot patch %s: %q (%v)", kind, target.Name, err)

		if force {
			// Attempt to delete...
			if err := deleteResource(target); err != nil {
				return err
			}
			log.Printf("Deleted %s: %q", kind, target.Name)

			// ... and recreate
			if err := createResource(target, releaseName); err != nil {
				return fmt.Errorf("Failed to recreate resource: %s", err)
			}
			log.Printf("Created a new %s called %q\n", kind, target.Name)

			// No need to refresh the target, as we recreated the resource based
			// on it. In addition, it might not exist yet and a call to `Refresh`
			// may fail.
		} else {
			log.Print("Use --force to force recreation of the resource")
			return err
		}
	} else {
		// When patch succeeds without needing to recreate, refresh target.
		target.Refresh(obj, true)
	}

	return nil
}

func checkRepairPatchForWarnings(patch []byte) ([]string, error) {
	var ignoreInstructions []string

	var patchDataMap strategicpatch.JSONMap
	if err := json.Unmarshal(patch, &patchDataMap); err != nil {
		return nil, fmt.Errorf("unable to unmarshal patch json: %s", err)
	}

	_ = processResourceReplicasAndResources(patchDataMap, func(node map[string]interface{}, field string) {
		var fieldPath, annoName, autoscalerName string

		if field == "replicas" {
			fieldPath = fmt.Sprintf("spec.%s", field)
			annoName = SetReplicasOnlyOnCreationAnnotation
			autoscalerName = "HPA"
		} else { // "resources"
			fieldPath = fmt.Sprintf("*.%s", field)
			annoName = SetResourcesOnlyOnCreationAnnotation
			autoscalerName = "VPA"
		}

		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("Detected %s field change in the kubernetes live object state!", fieldPath))
		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("If you use %s autoscaler then remove this field from the resource manifest configuration of the chart.", autoscalerName))
		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("Otherwise, to ignore field %s changes in the kubernetes live object state add", fieldPath))
		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("\"%s\": \"true\" annotation to the resource manifest configuration of the chart,", annoName))
		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("or use the option --add-annotation=%s=true", annoName))
		ignoreInstructions = append(ignoreInstructions, fmt.Sprintf("— this option will add annotation to all deployed resources."))

	})

	return ignoreInstructions, nil
}

func processResourceReplicasAndResources(manifestDataMap map[string]interface{}, processFieldFunc func(node map[string]interface{}, field string)) map[string]interface{} {
	if spec, ok := manifestDataMap["spec"]; ok {
		if specTyped, ok := spec.(map[string]interface{}); ok {
			// process spec.replicas
			if _, ok := specTyped["replicas"]; ok {
				processFieldFunc(specTyped, "replicas")
			}

			// process spec.volumeClaimTemplates[*].spec.resources
			if specVolumeClaimTemplates, ok := specTyped["volumeClaimTemplates"]; ok {
				if specVolumeClaimTemplatesTyped, ok := specVolumeClaimTemplates.([]interface{}); ok {
					for ind, volumeClaimTemplate := range specVolumeClaimTemplatesTyped {
						if volumeClaimTemplateTyped, ok := volumeClaimTemplate.(map[string]interface{}); ok {
							if _, ok := volumeClaimTemplateTyped["resources"]; ok {
								processFieldFunc(volumeClaimTemplateTyped, "resources")
							}

							if len(volumeClaimTemplateTyped) == 0 {
								specVolumeClaimTemplatesTyped = append(specVolumeClaimTemplatesTyped[:ind], specVolumeClaimTemplatesTyped[ind+1:]...)
							}
						}
					}

					if len(specVolumeClaimTemplatesTyped) == 0 {
						delete(specTyped, "volumeClaimTemplates")
					}
				}
			}

			processSpecContainers := func(specTyped map[string]interface{}) {
				if containers, ok := specTyped["containers"]; ok {
					if containersTyped, ok := containers.([]interface{}); ok {
						for ind, container := range containersTyped {
							if containerTyped, ok := container.(map[string]interface{}); ok {
								if _, ok := containerTyped["resources"]; ok {
									processFieldFunc(containerTyped, "resources")
								}

								if len(containerTyped) == 0 {
									containersTyped = append(containersTyped[:ind], containersTyped[ind+1:]...)
								}
							}
						}

						if len(containersTyped) == 0 {
							delete(specTyped, "containers")
						}
					}
				}
			}

			// process spec.containers[*].resources
			processSpecContainers(specTyped)

			// process spec.template.spec.containers[*].resources
			if template, ok := specTyped["template"]; ok {
				if templateTyped, ok := template.(map[string]interface{}); ok {
					if templateSpec, ok := templateTyped["spec"]; ok {
						if templateSpecTyped, ok := templateSpec.(map[string]interface{}); ok {
							processSpecContainers(templateSpecTyped)

							if len(templateSpecTyped) == 0 {
								delete(templateTyped, "spec")
							}
						}
					}

					if len(templateTyped) == 0 {
						delete(specTyped, "template")
					}
				}
			}

			if len(specTyped) == 0 {
				delete(manifestDataMap, "spec")
			}
		}
	}

	return manifestDataMap
}

func getSelectorFromObject(obj runtime.Object) (map[string]string, bool) {
	switch typed := obj.(type) {

	case *v1.ReplicationController:
		return typed.Spec.Selector, true

	case *extv1beta1.ReplicaSet:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1.ReplicaSet:
		return typed.Spec.Selector.MatchLabels, true

	case *extv1beta1.Deployment:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1beta1.Deployment:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1beta2.Deployment:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1.Deployment:
		return typed.Spec.Selector.MatchLabels, true

	case *extv1beta1.DaemonSet:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1beta2.DaemonSet:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1.DaemonSet:
		return typed.Spec.Selector.MatchLabels, true

	case *batch.Job:
		return typed.Spec.Selector.MatchLabels, true

	case *appsv1beta1.StatefulSet:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1beta2.StatefulSet:
		return typed.Spec.Selector.MatchLabels, true
	case *appsv1.StatefulSet:
		return typed.Spec.Selector.MatchLabels, true

	default:
		return nil, false
	}
}

func (c *Client) watchUntilReady(timeout time.Duration, info *resource.Info) error {
	w, err := resource.NewHelper(info.Client, info.Mapping).WatchSingle(info.Namespace, info.Name, info.ResourceVersion)
	if err != nil {
		return err
	}

	kind := info.Mapping.GroupVersionKind.Kind
	c.Log("Watching for changes to %s %s with timeout of %v", kind, info.Name, timeout)

	// What we watch for depends on the Kind.
	// - For a Job, we watch for completion.
	// - For all else, we watch until Ready.
	// In the future, we might want to add some special logic for types
	// like Ingress, Volume, etc.

	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), timeout)
	defer cancel()
	_, err = watchtools.UntilWithoutRetry(ctx, w, func(e watch.Event) (bool, error) {
		switch e.Type {
		case watch.Added, watch.Modified:
			// For things like a secret or a config map, this is the best indicator
			// we get. We care mostly about jobs, where what we want to see is
			// the status go into a good state. For other types, like ReplicaSet
			// we don't really do anything to support these as hooks.
			c.Log("Add/Modify event for %s: %v", info.Name, e.Type)
			if kind == "Job" {
				return c.waitForJob(e, info.Name)
			}
			return true, nil
		case watch.Deleted:
			c.Log("Deleted event for %s", info.Name)
			return true, nil
		case watch.Error:
			// Handle error and return with an error.
			c.Log("Error event for %s", info.Name)
			return true, fmt.Errorf("Failed to deploy %s", info.Name)
		default:
			return false, nil
		}
	})
	return err
}

// waitForJob is a helper that waits for a job to complete.
//
// This operates on an event returned from a watcher.
func (c *Client) waitForJob(e watch.Event, name string) (bool, error) {
	job := &batch.Job{}
	err := legacyscheme.Scheme.Convert(e.Object, job, nil)
	if err != nil {
		return true, err
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batch.JobComplete && c.Status == v1.ConditionTrue {
			return true, nil
		} else if c.Type == batch.JobFailed && c.Status == v1.ConditionTrue {
			return true, fmt.Errorf("Job failed: %s", c.Reason)
		}
	}

	c.Log("%s: Jobs active: %d, jobs failed: %d, jobs succeeded: %d", name, job.Status.Active, job.Status.Failed, job.Status.Succeeded)
	return false, nil
}

// scrubValidationError removes kubectl info from the message.
func scrubValidationError(err error) error {
	if err == nil {
		return nil
	}
	const stopValidateMessage = "if you choose to ignore these errors, turn validation off with --validate=false"

	if strings.Contains(err.Error(), stopValidateMessage) {
		return goerrors.New(strings.Replace(err.Error(), "; "+stopValidateMessage, "", -1))
	}
	return err
}

// WaitAndGetCompletedPodPhase waits up to a timeout until a pod enters a completed phase
// and returns said phase (PodSucceeded or PodFailed qualify).
func (c *Client) WaitAndGetCompletedPodPhase(namespace string, reader io.Reader, timeout time.Duration) (v1.PodPhase, error) {
	infos, err := c.Build(namespace, reader)
	if err != nil {
		return v1.PodUnknown, err
	}
	info := infos[0]

	kind := info.Mapping.GroupVersionKind.Kind
	if kind != "Pod" {
		return v1.PodUnknown, fmt.Errorf("%s is not a Pod", info.Name)
	}

	if err := c.watchPodUntilComplete(timeout, info); err != nil {
		return v1.PodUnknown, err
	}

	if err := info.Get(); err != nil {
		return v1.PodUnknown, err
	}
	status := info.Object.(*v1.Pod).Status.Phase

	return status, nil
}

func (c *Client) watchPodUntilComplete(timeout time.Duration, info *resource.Info) error {
	w, err := resource.NewHelper(info.Client, info.Mapping).WatchSingle(info.Namespace, info.Name, info.ResourceVersion)
	if err != nil {
		return err
	}

	c.Log("Watching pod %s for completion with timeout of %v", info.Name, timeout)
	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), timeout)
	defer cancel()
	_, err = watchtools.UntilWithoutRetry(ctx, w, func(e watch.Event) (bool, error) {
		return isPodComplete(e)
	})

	return err
}

func isPodComplete(event watch.Event) (bool, error) {
	o, ok := event.Object.(*v1.Pod)
	if !ok {
		return true, fmt.Errorf("expected a *v1.Pod, got %T", event.Object)
	}
	if event.Type == watch.Deleted {
		return false, fmt.Errorf("pod not found")
	}
	switch o.Status.Phase {
	case v1.PodFailed, v1.PodSucceeded:
		return true, nil
	}
	return false, nil
}

//get a kubernetes resources' relation pods
// kubernetes resource used select labels to relate pods
func (c *Client) getSelectRelationPod(info *resource.Info, objPods map[string][]v1.Pod) (map[string][]v1.Pod, error) {
	if info == nil {
		return objPods, nil
	}

	c.Log("get relation pod of object: %s/%s/%s", info.Namespace, info.Mapping.GroupVersionKind.Kind, info.Name)

	versioned := asVersionedOrUnstructured(info)
	selector, ok := getSelectorFromObject(versioned)
	if !ok {
		return objPods, nil
	}

	client, _ := c.KubernetesClientSet()

	pods, err := client.CoreV1().Pods(info.Namespace).List(metav1.ListOptions{
		LabelSelector: labels.Set(selector).AsSelector().String(),
	})
	if err != nil {
		return objPods, err
	}

	for _, pod := range pods.Items {
		vk := "v1/Pod"
		if !isFoundPod(objPods[vk], pod) {
			objPods[vk] = append(objPods[vk], pod)
		}
	}
	return objPods, nil
}

func isFoundPod(podItem []v1.Pod, pod v1.Pod) bool {
	for _, value := range podItem {
		if (value.Namespace == pod.Namespace) && (value.Name == pod.Name) {
			return true
		}
	}
	return false
}

func asVersionedOrUnstructured(info *resource.Info) runtime.Object {
	obj, _ := asVersioned(info)
	return obj
}

func asVersioned(info *resource.Info) (runtime.Object, error) {
	converter := runtime.ObjectConvertor(scheme.Scheme)
	groupVersioner := runtime.GroupVersioner(schema.GroupVersions(scheme.Scheme.PrioritizedVersionsAllGroups()))
	if info.Mapping != nil {
		groupVersioner = info.Mapping.GroupVersionKind.GroupVersion()
	}

	obj, err := converter.ConvertToVersion(info.Object, groupVersioner)
	if err != nil {
		return info.Object, err
	}
	return obj, nil
}

func asInternal(info *resource.Info) (runtime.Object, error) {
	groupVersioner := info.Mapping.GroupVersionKind.GroupKind().WithVersion(runtime.APIVersionInternal).GroupVersion()
	return legacyscheme.Scheme.ConvertToVersion(info.Object, groupVersioner)
}
