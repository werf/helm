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
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
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
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	cachetools "k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/validation"
	"k8s.io/kubernetes/pkg/kubectl/cmd/get"
)

// MissingGetHeader is added to Get's output when a resource is not found.
const MissingGetHeader = "==> MISSING\nKIND\t\tNAME\n"

const (
	SetReplicasOnlyOnCreationAnnotation  = "werf.io/set-replicas-only-on-creation"
	SetResourcesOnlyOnCreationAnnotation = "werf.io/set-resources-only-on-creation"

	repairPatchAnnotation        = "debug.werf.io/repair-patch"
	repairPatchErrorsAnnotation  = "debug.werf.io/repair-patch-errors"
	repairInstructionsAnnotation = "debug.werf.io/repair-instructions"
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

// Create creates Kubernetes resources from an io.reader.
//
// Namespace will set the namespace.
func (c *Client) Create(namespace string, reader io.Reader, timeout int64, shouldWait bool) error {
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
	if err := perform(infos, createResource); err != nil {
		return err
	}
	if shouldWait {
		return c.waitForResources(time.Duration(timeout)*time.Second, infos)
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

// BuildUnstructuredTable reads Kubernetes objects and returns unstructured infos
// as a Table. This is meant for viewing resources and displaying them in a table.
// This is similar to BuildUnstructured but transforms the request for table
// display.
func (c *Client) BuildUnstructuredTable(namespace string, reader io.Reader) (Result, error) {
	var result Result

	result, err := c.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(namespace).
		DefaultNamespace().
		Stream(reader, "").
		Flatten().
		TransformRequests(transformRequests).
		Do().Infos()
	return result, scrubValidationError(err)
}

// This is used to retrieve a table view of the data. A table view is how kubectl
// retrieves the information Helm displays as resources in status. Note, table
// data is returned as a Table type that does not conform to the runtime.Object
// interface but is that type. So, you can't transform it into Go objects easily.
func transformRequests(req *rest.Request) {

	// The request headers are for both the v1 and v1beta1 versions of the table
	// as Kubernetes 1.14 and older used the beta version.
	req.SetHeader("Accept", strings.Join([]string{
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1.SchemeGroupVersion.Version, metav1.GroupName),
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1beta1.SchemeGroupVersion.Version, metav1beta1.GroupName),
		"application/json",
	}, ","))
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

func sortByKey(objs map[string][]runtime.Object) []string {
	var keys []string
	// Create a simple slice, so we can sort it
	for key := range objs {
		keys = append(keys, key)
	}
	// Sort alphabetically by version/kind keys
	sort.Strings(keys)
	return keys
}

// We have slices of tables that need to be sorted by name. In this case the
// self link is used so the sorting will include namespace and name.
func sortTableSlice(objs []runtime.Object) []runtime.Object {
	// If there are 0 or 1 objects to sort there is nothing to sort so
	// the list can be returned
	if len(objs) < 2 {
		return objs
	}

	ntbl := &metav1.Table{}
	unstr, ok := objs[0].(*unstructured.Unstructured)
	if !ok {
		return objs
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstr.Object, ntbl); err != nil {
		return objs
	}

	// Sort the list of objects
	var newObjs []runtime.Object
	namesCache := make(map[string]runtime.Object, len(objs))
	var names []string
	var key string
	for _, obj := range objs {
		unstr, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return objs
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstr.Object, ntbl); err != nil {
			return objs
		}

		// At this point we have a table. Each table has just one row. We are
		// sorting the tables by the first cell (name) in the first and only
		// row. If the first cell of the first row cannot be gotten as a string
		// we return the original unsorted list.
		if len(ntbl.Rows) == 0 { // Make sure there are rows to read from
			return objs
		}
		if len(ntbl.Rows[0].Cells) == 0 { // Make sure there are cells to read
			return objs
		}
		key, ok = ntbl.Rows[0].Cells[0].(string)
		if !ok {
			return objs
		}
		namesCache[key] = obj
		names = append(names, key)
	}

	sort.Strings(names)

	for _, name := range names {
		newObjs = append(newObjs, namesCache[name])
	}

	return newObjs
}

// Get gets Kubernetes resources as pretty-printed string.
//
// Namespace will set the namespace.
func (c *Client) Get(namespace string, reader io.Reader) (string, error) {
	// Since we don't know what order the objects come in, let's group them by the types and then sort them, so
	// that when we print them, they come out looking good (headers apply to subgroups, etc.).
	objs := make(map[string][]runtime.Object)
	gk := make(map[string]schema.GroupKind)
	mux := &sync.Mutex{}

	// The contents of the reader are used two times. The bytes are coppied out
	// for use in future readers.
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", err
	}

	// Get the table display for the objects associated with the release. This
	// is done in table format so that it can be displayed in the status in
	// the same way kubectl displays the resource information.
	// Note, the response returns unstructured data instead of typed objects.
	// These cannot be easily (i.e., via the go packages) transformed into
	// Go types.
	tinfos, err := c.BuildUnstructuredTable(namespace, bytes.NewBuffer(b))
	if err != nil {
		return "", err
	}

	missing := []string{}
	err = perform(tinfos, func(info *resource.Info) error {
		mux.Lock()
		defer mux.Unlock()
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
		gk[vk] = gvk.GroupKind()

		// Initialize map. The main map groups resources based on version/kind
		// The second level is a simple 'Name' to 'Object', that will help sort
		// the individual resource later
		if objs[vk] == nil {
			objs[vk] = []runtime.Object{}
		}
		// Map between the resource name to the underlying info object
		objs[vk] = append(objs[vk], resourceInfoToObject(info, c))

		return nil
	})
	if err != nil {
		return "", err
	}

	// This section finds related resources (e.g., pods). Before looking up pods
	// the resources the pods are made from need to be looked up in a manner
	// that can be turned into Go types and worked with.
	infos, err := c.BuildUnstructured(namespace, bytes.NewBuffer(b))
	if err != nil {
		return "", err
	}
	err = perform(infos, func(info *resource.Info) error {
		mux.Lock()
		defer mux.Unlock()
		if err := info.Get(); err != nil {
			c.Log("WARNING: Failed Get for resource %q: %s", info.Name, err)
			missing = append(missing, fmt.Sprintf("%v\t\t%s", info.Mapping.Resource, info.Name))
			return nil
		}

		//Get the relation pods
		objs, err = c.getSelectRelationPod(info, objs)
		if err != nil {
			c.Log("Warning: get the relation pod is failed, err:%s", err.Error())
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	// Ok, now we have all the objects grouped by types (say, by v1/Pod, v1/Service, etc.), so
	// spin through them and print them. Printer is cool since it prints the header only when
	// an object type changes, so we can just rely on that. Problem is it doesn't seem to keep
	// track of tab widths.
	buf := new(bytes.Buffer)

	// Sort alphabetically by version/kind keys
	vkKeys := sortByKey(objs)
	// Iterate on sorted version/kind types
	for _, t := range vkKeys {
		if _, err = fmt.Fprintf(buf, "==> %s\n", t); err != nil {
			return "", err
		}
		vk := objs[t]

		// The request made for tables returns each Kubernetes object as its
		// own table. The normal sorting provided by kubectl and cli-runtime
		// does not handle this case. Here we sort within each of our own
		// grouping.
		vk = sortTableSlice(vk)

		// The printer flag setup follows a simalar setup to kubectl
		printFlags := get.NewHumanPrintFlags()
		if lgk, ok := gk[t]; ok {
			printFlags.SetKind(lgk)
		}
		printer, _ := printFlags.ToPrinter("")
		printer, err = printers.NewTypeSetter(scheme.Scheme).WrapToPrinter(printer, nil)
		if err != nil {
			return "", err
		}
		printer = &get.TablePrinter{Delegate: printer}

		for _, resource := range vk {
			if err := printer.PrintObj(resource, buf); err != nil {
				c.Log("failed to print object type %s: %v", t, err)
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

// Update reads the current configuration and a target configuration from io.reader
// and creates resources that don't already exist, updates resources that have been modified
// in the target configuration and deletes resources from the current configuration that are
// not present in the target configuration.
//
// Namespace will set the namespaces.
//
// Deprecated: use UpdateWithOptions instead.
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
	CleanupOnFail bool
}

// UpdateWithOptions reads the current configuration and a target configuration from io.reader
// and creates resources that don't already exist, updates resources that have been modified
// in the target configuration and deletes resources from the current configuration that are
// not present in the target configuration.
//
// Namespace will set the namespaces. UpdateOptions provides additional parameters to control
// update behavior.
func (c *Client) UpdateWithOptions(namespace string, originalReader, targetReader io.Reader, opts UpdateOptions) error {
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

			// Since the resource does not exist, create it.
			if err := createResource(target); err != nil {
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
			return fmt.Errorf(
				"kind %s with the name %q already exists in the cluster and wasn't defined in the previous release. Before upgrading, please either delete the resource from the cluster or remove it from the chart",
				target.Mapping.GroupVersionKind.Kind,
				target.Name,
			)
		}

		if err := updateResource(c, target, currentObj, originalInfo.Object, opts.Force, opts.Recreate); err != nil {
			c.Log("error updating the resource %q:\n\t %v", target.Name, err)
			updateErrors = append(updateErrors, err.Error())
		}
		return nil
	})

	cleanupErrors := []string{}

	if opts.CleanupOnFail && (err != nil || len(updateErrors) != 0) {
		c.Log("Cleanup on fail enabled: cleaning up newly created resources due to update manifests failures")
		cleanupErrors = c.cleanup(newlyCreatedResources)
	}

	switch {
	case err != nil:
		return fmt.Errorf(strings.Join(append([]string{err.Error()}, cleanupErrors...), " && "))
	case len(updateErrors) != 0:
		return fmt.Errorf(strings.Join(append(updateErrors, cleanupErrors...), " && "))
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

// WaitUntilCRDEstablished polls the given CRD until it reaches the established
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

	errs := make(chan error)
	go batchPerform(infos, fn, errs)

	for range infos {
		err := <-errs
		if err != nil {
			return err
		}
	}
	return nil
}

func batchPerform(infos Result, fn ResourceActorFunc, errs chan<- error) {
	var kind string
	var wg sync.WaitGroup
	for _, info := range infos {
		currentKind := info.Object.GetObjectKind().GroupVersionKind().Kind
		if kind != currentKind {
			wg.Wait()
			kind = currentKind
		}
		wg.Add(1)
		go func(i *resource.Info) {
			errs <- fn(i)
			wg.Done()
		}(info)
	}
}

func getObjectAnnotation(obj runtime.Object, annoName string) string {
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		logboek.LogErrorF("Unable to fetch annotations of kube object: %s\n", err)
		return ""
	}
	return annots[annoName]
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

	if os.Getenv("WERF_DEBUG_3WM_ANNOTATIONS_MODE") == "1" {
		logboek.LogInfoF("Set annotation %s=%s for %s named %q\n", annoName, annoValue, kind, name)
	}

	return metadataAccessor.SetAnnotations(obj, annots)
}

func createResource(info *resource.Info) error {
	obj, err := resource.NewHelper(info.Client, info.Mapping).Create(info.Namespace, true, info.Object, nil)
	if err != nil {
		return err
	}
	return info.Refresh(obj, true)
}

func deleteResource(info *resource.Info) error {
	policy := metav1.DeletePropagationBackground
	opts := &metav1.DeleteOptions{PropagationPolicy: &policy}
	_, err := resource.NewHelper(info.Client, info.Mapping).DeleteWithOptions(info.Namespace, info.Name, opts)
	return err
}

func applyPatch(target *resource.Info, current runtime.Object, patch []byte) ([]byte, error) {
	oldData, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("serializing current configuration: %s", err)
	}
	return applyPatchForData(target, oldData, patch)
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

		var patchMap strategicpatch.JSONMap
		if err := json.Unmarshal(patch, &patchMap); err != nil {
			return nil, fmt.Errorf("unable to unmarshal json patch: %s: %s", patch, err)
		}

		resMap, err := strategicpatch.StrategicMergeMapPatch(oldDataMap, patchMap, versionedObject)
		if err != nil {
			return nil, fmt.Errorf("failed to apply strategic merge patch: %s", err)
		}

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

	// Unstructured objects, such as CRDs, may not have a not registered error
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

func createRepairPatch(target *resource.Info, currentObj, originalObj runtime.Object) ([]byte, types.PatchType, error) {
	twoWayMergePatch, _, err := createPatch(target, originalObj)
	if err != nil {
		return nil, "", fmt.Errorf("unable to create two-way merge patch: %s", err)
	}

	setReplicasOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetReplicasOnlyOnCreationAnnotation)
	setResourcesOnlyOnCreationAnnoValue := getObjectAnnotation(target.Object, SetResourcesOnlyOnCreationAnnotation)

	isReplicasOnlyOnCreation := setReplicasOnlyOnCreationAnnoValue == "true"
	isResourcesOnlyOnCreation := setResourcesOnlyOnCreationAnnoValue == "true"

	newCurrentData, err := applyPatch(target, currentObj, twoWayMergePatch)
	if err != nil {
		return nil, "", fmt.Errorf("unable to construct new current state using two-way merge patch: %s", err)
	}

	newCurrentData = filterResourceData(newCurrentData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation)

	targetData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, "", fmt.Errorf("serializing target configuration: %s", err)
	}

	targetData = filterResourceData(targetData, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation)

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
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(targetData, targetData, newCurrentData, preconditions...)
		if err != nil {
			if mergepatch.IsPreconditionFailed(err) {
				return nil, "", fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
			}
			return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, targetData, targetData, newCurrentData), target.Source, err)
		}
	case err != nil:
		return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf("getting instance of versioned object for %v:", target.Mapping.GroupVersionKind), target.Source, err)
	default:
		lookupPatchMeta, err = strategicpatch.NewPatchMetaFromStruct(versionedObject)
		if err != nil {
			return nil, "", cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, targetData, targetData, newCurrentData), target.Source, err)
		}

		patch, err = strategicpatch.CreateThreeWayMergePatch(targetData, targetData, newCurrentData, lookupPatchMeta, true)
		if err != nil {
			return nil, types.StrategicMergePatchType, fmt.Errorf("failed to create two-way merge patch: %v", err)
		}
	}

	return patch, patchType, nil
}

func updateResource(c *Client, target *resource.Info, currentObj, originalObj runtime.Object, force bool, recreate bool) error {
	repairPatchData, _, err := createRepairPatch(target, currentObj, originalObj)
	if err != nil {
		logboek.LogErrorF("WARNING Unable to create repair patch: %s\n", err)
		_ = setObjectAnnotation(target.Object, repairPatchErrorsAnnotation, err.Error())
	} else {
		isRepairPatchEmpty := len(repairPatchData) == 0 || string(repairPatchData) == "{}"
		if !isRepairPatchEmpty {
			var repairInstructions []string

			// main repair instruction parts
			mripart1 := fmt.Sprintf("%s named %s state is inconsistent with chart configuration state!", target.Mapping.GroupVersionKind.Kind, target.Name)
			mripart2 := fmt.Sprintf("Repair patch has been written to the %s annotation of %s named %s", repairPatchAnnotation, target.Mapping.GroupVersionKind.Kind, target.Name)
			mripart3 := "Execute the following command manually to repair resource state:"
			mripart4 := fmt.Sprintf("kubectl -n %s patch %s %s -p '%s'", target.Namespace, target.Mapping.GroupVersionKind.Kind, target.Name, repairPatchData)

			repairInstructions = append(repairInstructions, strings.Join([]string{
				mripart1,
				mripart2,
				mripart3,
				mripart4,
			}, "\n"))

			ignoreInstructions := checkRepairPatchData(repairPatchData)
			repairInstructions = append(repairInstructions, ignoreInstructions...)

			repairInstructionJsonData, _ := json.Marshal(repairInstructions)
			_ = setObjectAnnotation(target.Object, repairInstructionsAnnotation, string(repairInstructionJsonData))

			defer func() {
				logboek.LogErrorF("WARNING ###########################################################################\n")
				logboek.LogErrorF("WARNING %s\n", mripart1)
				logboek.LogErrorF("WARNING %s\n", mripart2)

				for _, instruction := range ignoreInstructions {
					logboek.LogErrorF(fmt.Sprintf("WARNING %s\n", instruction))
				}

				logboek.LogErrorF("WARNING %s\n", mripart3)
				fmt.Printf("%s\n", mripart4)
				logboek.LogErrorF("WARNING ###########################################################################\n")
			}()
		}

		_ = setObjectAnnotation(target.Object, repairPatchAnnotation, string(repairPatchData))
	}

	patch, patchType, err := createPatch(target, originalObj)
	if err != nil {
		return fmt.Errorf("failed to create patch: %s", err)
	}

	if patch == nil {
		c.Log("Looks like there are no changes for %s %q", target.Mapping.GroupVersionKind.Kind, target.Name)
		// This needs to happen to make sure that tiller has the latest info from the API
		// Otherwise there will be no labels and other functions that use labels will panic
		if err := target.Get(); err != nil {
			return fmt.Errorf("error trying to refresh resource information: %v", err)
		}
	} else {
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
				if err := createResource(target); err != nil {
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

func filterResourceData(data []byte, isReplicasOnlyOnCreation, isResourcesOnlyOnCreation bool) []byte {
	updatedData := processResourceReplicasAndResources(data, func(node map[string]interface{}, field string) {
		if field == "replicas" && isReplicasOnlyOnCreation || field == "resources" && isResourcesOnlyOnCreation {
			delete(node, field)
		}
	})

	return updatedData
}

func checkRepairPatchData(data []byte) []string {
	var ignoreInstructions []string

	_ = processResourceReplicasAndResources(data, func(node map[string]interface{}, field string) {
		var fieldPath, annoName string

		if field == "replicas" {
			fieldPath = fmt.Sprintf("spec.%s", field)
			annoName = SetReplicasOnlyOnCreationAnnotation
		} else { // "resources"
			fieldPath = fmt.Sprintf("*.%s", field)
			annoName = SetResourcesOnlyOnCreationAnnotation
		}

		ignoreInstruction := fmt.Sprintf("To ignore %[1]s add '%[2]s: true' annotation to resource manually. Otherwise, use the option --add-annotation=%[2]s=true for adding annotation to all deploying resources", fieldPath, annoName)
		ignoreInstructions = append(ignoreInstructions, ignoreInstruction)
	})

	if len(ignoreInstructions) != 0 {
		ignoreInstructions = append([]string{"If you use HPA/VPA remove related fields from resource manifest"}, ignoreInstructions...)
	}

	return ignoreInstructions
}

func processResourceReplicasAndResources(resourceData []byte, processFieldFunc func(node map[string]interface{}, field string)) []byte {
	res := map[string]interface{}{}
	if err := json.Unmarshal(resourceData, &res); err != nil {
		panic(err)
	}

	if spec, ok := res["spec"]; ok {
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
				delete(res, "spec")
			}
		}

		updatedResourceData, err := json.Marshal(res)
		if err != nil {
			panic(err)
		}

		return updatedResourceData
	} else {
		return resourceData
	}
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
	// Use a selector on the name of the resource. This should be unique for the
	// given version and kind
	selector, err := fields.ParseSelector(fmt.Sprintf("metadata.name=%s", info.Name))
	if err != nil {
		return err
	}
	lw := cachetools.NewListWatchFromClient(info.Client, info.Mapping.Resource.Resource, info.Namespace, selector)

	kind := info.Mapping.GroupVersionKind.Kind
	c.Log("Watching for changes to %s %s with timeout of %v", kind, info.Name, timeout)

	// What we watch for depends on the Kind.
	// - For a Job, we watch for completion.
	// - For all else, we watch until Ready.
	// In the future, we might want to add some special logic for types
	// like Ingress, Volume, etc.

	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), timeout)
	defer cancel()
	_, err = watchtools.ListWatchUntil(ctx, lw, func(e watch.Event) (bool, error) {
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
	err := scheme.Scheme.Convert(e.Object, job, nil)
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
	lw := cachetools.NewListWatchFromClient(info.Client, info.Mapping.Resource.Resource, info.Namespace, fields.ParseSelectorOrDie(fmt.Sprintf("metadata.name=%s", info.Name)))

	c.Log("Watching pod %s for completion with timeout of %v", info.Name, timeout)
	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), timeout)
	defer cancel()
	_, err := watchtools.ListWatchUntil(ctx, lw, func(e watch.Event) (bool, error) {
		return isPodComplete(e)
	})

	return err
}

// GetPodLogs takes pod name and namespace and returns the current logs (streaming is NOT enabled).
func (c *Client) GetPodLogs(name, ns string) (io.ReadCloser, error) {
	client, err := c.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	req := client.CoreV1().Pods(ns).GetLogs(name, &v1.PodLogOptions{})
	logReader, err := req.Stream()
	if err != nil {
		return nil, fmt.Errorf("error in opening log stream, got: %s", err)
	}
	return logReader, nil
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

// get a kubernetes resources' relation pods
// kubernetes resource used select labels to relate pods
func (c *Client) getSelectRelationPod(info *resource.Info, objs map[string][]runtime.Object) (map[string][]runtime.Object, error) {
	if info == nil {
		return objs, nil
	}

	c.Log("get relation pod of object: %s/%s/%s", info.Namespace, info.Mapping.GroupVersionKind.Kind, info.Name)

	versioned := asVersionedOrUnstructured(info)
	selector, ok := getSelectorFromObject(versioned)
	if !ok {
		return objs, nil
	}

	// The related pods are looked up in Table format so that their display can
	// be printed in a manner similar to kubectl when it get pods. The response
	// can be used with a table printer.
	infos, err := c.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(info.Namespace).
		DefaultNamespace().
		ResourceTypes("pods").
		LabelSelector(labels.Set(selector).AsSelector().String()).
		TransformRequests(transformRequests).
		Do().Infos()
	if err != nil {
		return objs, err
	}

	for _, info := range infos {
		vk := "v1/Pod(related)"
		objs[vk] = append(objs[vk], info.Object)
	}

	return objs, nil
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
	return scheme.Scheme.ConvertToVersion(info.Object, groupVersioner)
}
